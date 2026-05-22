// Package indexer wires source → chunk → store into a single pipeline
// runnable from CLI, MCP, or dashboard. It owns:
//
//   - the lock file (only one indexer writes at a time)
//   - the incremental gate (content_hash + last_mtime stored on the
//     session row; unchanged files are skipped without re-reading)
//   - the progress channel (SSE-friendly: non-blocking sends, drops
//     when the receiver isn't ready)
//
// The package has no knowledge of MCP, HTTP, or jsonl beyond what
// internal/source exposes; everything is plain Go function calls.
package indexer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/wm-it/claude-knowledge-vault/internal/chunk"
	"github.com/wm-it/claude-knowledge-vault/internal/source"
	"github.com/wm-it/claude-knowledge-vault/internal/store"
)

// LockFileName is written into Options.DataDir while indexing.
const LockFileName = "index.lock"

// LockTimeout is the age after which a lock file is considered
// abandoned and removed automatically. Mirrors env-sync's
// syncengine.LockTimeout.
const LockTimeout = 5 * time.Minute

// ErrIndexInProgress is returned when a fresh lock file already
// exists. Callers should report this to the user, not retry — a
// second click on "Re-index" while one is running should noop.
var ErrIndexInProgress = errors.New("indexer: another index in progress")

// Phase labels emitted on Progress.
const (
	PhaseWalk = "walk" // walking ~/.claude/projects
	PhaseFile = "file" // processing one jsonl
	PhaseDone = "done" // pipeline finished
)

// Options configures Run.
type Options struct {
	// Root is the directory to walk. Required. Typically
	// ~/.claude/projects.
	Root string
	// DataDir is where the lock file lives. Required. Typically
	// ${CLAUDE_PLUGIN_DATA}. Auto-mkdir if absent.
	DataDir string
	// Force re-indexes files even when content_hash matches the
	// existing session row. Useful for schema upgrades.
	Force bool
	// Since gates the walk by mtime — `mtime > Since`. Zero value
	// disables (walk everything that passes the hash gate).
	Since time.Time
	// Progress, if non-nil, receives one or more progress snapshots
	// during the run. Sends are non-blocking; a slow receiver drops
	// intermediate updates but the terminal `done` event is sent
	// with one blocking attempt so callers can use it to await
	// completion.
	Progress chan<- Progress
	// Now is injected for deterministic lock-staleness tests.
	// Nil → time.Now.
	Now func() time.Time
	// OnFileError, if non-nil, is invoked when a single jsonl file
	// fails to parse / read. The walk continues. nil → silent.
	OnFileError func(path string, err error)
	// ChunkOpts forwards to chunk.Split. Zero value uses defaults.
	ChunkOpts chunk.Options
}

// Progress is one update tick. Fields are cumulative.
type Progress struct {
	Phase          string
	FilesTotal     int
	FilesScanned   int
	FilesIndexed   int
	TurnsInserted  int
	ChunksInserted int
	// CurrentFile is the jsonl currently being processed.
	CurrentFile string
}

// Result summarises one Run invocation.
type Result struct {
	FilesScanned   int
	FilesIndexed   int
	TurnsInserted  int
	ChunksInserted int
	Elapsed        time.Duration
}

// Run executes one indexing pass. Idempotent: calling twice in a row
// with unchanged jsonl files re-touches zero rows.
func Run(ctx context.Context, db *store.DB, opts *Options) (*Result, error) {
	if db == nil {
		return nil, errors.New("indexer: nil store")
	}
	if opts == nil {
		return nil, errors.New("indexer: nil options")
	}
	if opts.Root == "" {
		return nil, errors.New("indexer: Options.Root required")
	}
	if opts.DataDir == "" {
		return nil, errors.New("indexer: Options.DataDir required")
	}
	if err := os.MkdirAll(opts.DataDir, 0o700); err != nil {
		return nil, fmt.Errorf("indexer: mkdir DataDir: %w", err)
	}

	release, err := acquireLock(opts)
	if err != nil {
		return nil, err
	}
	defer release()

	start := nowFn(opts)()
	res := &Result{}

	emit(opts, Progress{Phase: PhaseWalk})
	summaries, walkErr := source.Walk(ctx, source.WalkOptions{
		Root:    opts.Root,
		Since:   opts.Since,
		OnError: opts.OnFileError,
	})
	if walkErr != nil {
		return res, fmt.Errorf("indexer: walk: %w", walkErr)
	}
	total := len(summaries)
	emit(opts, Progress{Phase: PhaseWalk, FilesTotal: total})

	for i := range summaries {
		sum := &summaries[i] // 144 B per FileSummary; range-copy is wasteful
		if err := ctx.Err(); err != nil {
			return res, err
		}
		res.FilesScanned++

		// Incremental gate: existing session with the same content
		// hash and we're not forcing → skip without re-reading.
		if !opts.Force {
			existing, found, gErr := db.GetSession(ctx, sum.SessionID)
			if gErr != nil {
				notifyFile(opts, sum.Path, gErr)
				continue
			}
			if found && existing.ContentHash == sum.ContentHash {
				emit(opts, progressFor(res, total, sum.Path))
				continue
			}
		}

		fileTurns, fileChunks, ixErr := indexOne(ctx, db, sum, opts)
		if ixErr != nil {
			notifyFile(opts, sum.Path, ixErr)
			continue
		}
		res.FilesIndexed++
		res.TurnsInserted += fileTurns
		res.ChunksInserted += fileChunks
		emit(opts, progressFor(res, total, sum.Path))
	}

	res.Elapsed = nowFn(opts)().Sub(start)
	emitDone(opts, progressFor(res, total, ""))
	return res, nil
}

// indexOne replaces the existing session row (if any) and writes the
// fresh chunk set. Returns the count of turns + chunks inserted.
func indexOne(ctx context.Context, db *store.DB, sum *source.FileSummary, opts *Options) (turns, chunks int, err error) {
	// DeleteSession is a no-op when the session doesn't exist yet.
	// When it does, the old chunks must be cleared before we re-
	// insert under the same (session_id, turn_index) keys.
	if dErr := db.DeleteSession(ctx, sum.SessionID); dErr != nil {
		return 0, 0, fmt.Errorf("delete-old: %w", dErr)
	}

	readTurns, rErr := source.ReadTurns(sum.Path, func(line int, err error) {
		notifyFile(opts, fmt.Sprintf("%s:%d", sum.Path, line), err)
	})
	if rErr != nil {
		return 0, 0, fmt.Errorf("read: %w", rErr)
	}

	sess := store.Session{
		ID:          sum.SessionID,
		ProjectPath: sum.ProjectPath,
		FilePath:    sum.Path,
		FirstTS:     sum.FirstTS,
		LastTS:      sum.LastTS,
		ContentHash: sum.ContentHash,
		LastMtime:   sum.Mtime,
		TurnCount:   len(readTurns),
	}
	if uErr := db.UpsertSession(ctx, &sess); uErr != nil {
		return 0, 0, fmt.Errorf("upsert-session: %w", uErr)
	}

	for _, t := range readTurns {
		if err := ctx.Err(); err != nil {
			return turns, chunks, err
		}
		if iErr := db.InsertTurn(ctx, store.Turn{
			SessionID: t.SessionID,
			TurnIndex: t.TurnIndex,
			Role:      t.Role,
			TS:        t.TS,
			RawSize:   t.RawSize,
		}); iErr != nil {
			return turns, chunks, fmt.Errorf("insert-turn: %w", iErr)
		}
		turns++

		parts := chunk.Split(t.Text, opts.ChunkOpts)
		for _, p := range parts {
			if cErr := db.InsertChunk(ctx, &store.Chunk{
				SessionID: t.SessionID,
				TurnIndex: t.TurnIndex,
				Role:      t.Role,
				TS:        t.TS,
				Title:     p.Title,
				Content:   p.Content,
			}); cErr != nil {
				return turns, chunks, fmt.Errorf("insert-chunk: %w", cErr)
			}
			chunks++
		}
	}
	return turns, chunks, nil
}

// ─── lock ────────────────────────────────────────────────────────────

func acquireLock(opts *Options) (release func(), err error) {
	path := filepath.Join(opts.DataDir, LockFileName)
	now := nowFn(opts)

	if info, statErr := os.Stat(path); statErr == nil {
		age := now().Sub(info.ModTime())
		if age < LockTimeout {
			return nil, fmt.Errorf("%w (age %s)", ErrIndexInProgress,
				age.Round(time.Second))
		}
		// Stale → remove and continue.
		_ = os.Remove(path)
	}
	if err := os.WriteFile(path, []byte(now().Format(time.RFC3339Nano)), 0o600); err != nil {
		return nil, fmt.Errorf("indexer: write lock: %w", err)
	}
	return func() { _ = os.Remove(path) }, nil
}

// ─── helpers ─────────────────────────────────────────────────────────

func nowFn(opts *Options) func() time.Time {
	if opts != nil && opts.Now != nil {
		return opts.Now
	}
	return time.Now
}

// emit does a non-blocking send so a slow / absent receiver can't
// stall the indexer.
func emit(opts *Options, p Progress) {
	if opts == nil || opts.Progress == nil {
		return
	}
	select {
	case opts.Progress <- p:
	default:
	}
}

// emitDone tries the same non-blocking send first but, if the
// channel has buffer space at any time during a tight ~100 ms window,
// makes one further attempt with a short timeout so callers using
// a buffered channel as a completion signal don't miss it.
func emitDone(opts *Options, p Progress) {
	if opts == nil || opts.Progress == nil {
		return
	}
	p.Phase = PhaseDone
	select {
	case opts.Progress <- p:
		return
	default:
	}
	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()
	select {
	case opts.Progress <- p:
	case <-timer.C:
	}
}

func notifyFile(opts *Options, path string, err error) {
	if opts != nil && opts.OnFileError != nil {
		opts.OnFileError(path, err)
	}
}

func progressFor(res *Result, total int, currentFile string) Progress {
	return Progress{
		Phase:          PhaseFile,
		FilesTotal:     total,
		FilesScanned:   res.FilesScanned,
		FilesIndexed:   res.FilesIndexed,
		TurnsInserted:  res.TurnsInserted,
		ChunksInserted: res.ChunksInserted,
		CurrentFile:    currentFile,
	}
}
