// Package source walks ~/.claude/projects/ and decodes Claude Code's
// per-session jsonl files into a normalised Turn stream that the
// indexer can feed to chunk + store.
//
// What we keep:
//
//	type=user        message.content (string OR text/thinking blocks)
//	type=assistant   message.content[*] of type text/thinking
//
// What we drop on the floor (PoC v1):
//
//	tool_use / tool_result   noisy, low search value, often base64
//	attachment / system / ai-title / last-prompt / permission-mode /
//	  file-history-snapshot   metadata noise, not conversation text
//
// Hostile-input policy: a malformed line is logged through OnError and
// skipped — never panics, never aborts the whole file. The corollary
// is that a partially corrupted jsonl still yields the turns that
// decoded cleanly.
package source

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Scanner buffer cap. Claude Code rarely writes lines > 1 MiB but
// pasted attachments + base64 tool_use can balloon a single JSON
// object well past the bufio default. 16 MiB is the same cap the
// MCP server in env-sync uses.
const maxLineBytes = 16 * 1024 * 1024

// fileSuffix is the only file extension we walk.
const fileSuffix = ".jsonl"

// Turn is the indexer-facing normalised conversation message.
type Turn struct {
	SessionID string
	TurnIndex int       // 0-based index within the file
	Role      string    // "user" | "assistant"
	Text      string    // extracted plain text (no tool_use payloads)
	TS        time.Time // turn timestamp
	RawSize   int       // bytes of the original jsonl line
}

// FileSummary captures one jsonl file's metadata for the sessions
// table + mtime-gated incremental indexing.
type FileSummary struct {
	Path        string    // absolute path to the .jsonl file
	SessionID   string    // file stem (UUID, typically)
	ProjectPath string    // parent directory inside ~/.claude/projects/
	FirstTS     time.Time // earliest turn timestamp
	LastTS      time.Time // latest turn timestamp
	Mtime       time.Time // file mtime
	ContentHash string    // hex sha256 of file contents
	TurnCount   int       // number of indexable turns (user + assistant)
}

// WalkOptions controls Walk.
type WalkOptions struct {
	// Root is the directory to walk (typically ~/.claude/projects).
	// Required.
	Root string
	// Since restricts the walk to files with mtime > Since. Zero
	// value means "no lower bound — walk everything".
	Since time.Time
	// OnError, if non-nil, is called for each per-file decode /
	// stat error so the caller can log instead of aborting.
	OnError func(path string, err error)
}

// Walk enumerates jsonl files under opts.Root and returns one
// FileSummary per file that survives the mtime gate. Returns
// (nil, nil) when Root doesn't exist — first-run on a machine that
// hasn't started Claude Code yet.
func Walk(ctx context.Context, opts WalkOptions) ([]FileSummary, error) {
	if opts.Root == "" {
		return nil, errors.New("source: WalkOptions.Root required")
	}
	if _, err := os.Stat(opts.Root); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}

	var out []FileSummary
	walkErr := filepath.WalkDir(opts.Root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			notify(opts.OnError, path, err)
			// Skip the offending subtree but keep walking the rest.
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(strings.ToLower(d.Name()), fileSuffix) {
			return nil
		}
		// Reject non-regular files (symlinks, sockets, FIFOs). Mirrors
		// claude-env-sync's hardening of WalkDir.
		info, statErr := d.Info()
		if statErr != nil {
			notify(opts.OnError, path, statErr)
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		mtime := info.ModTime()
		if !opts.Since.IsZero() && !mtime.After(opts.Since) {
			return nil // mtime gate
		}

		summary, sErr := summarise(path, info, mtime)
		if sErr != nil {
			notify(opts.OnError, path, sErr)
			return nil
		}
		out = append(out, summary)
		return nil
	})
	if walkErr != nil {
		return out, fmt.Errorf("source: walk %q: %w", opts.Root, walkErr)
	}
	return out, nil
}

// summarise reads a jsonl file once to compute its summary +
// indexable turn count. ReadTurns later re-reads the same file to
// stream turns — two-pass is intentional: Walk stays cheap (single
// SHA + count) and ReadTurns becomes a pure generator.
func summarise(path string, info fs.FileInfo, mtime time.Time) (FileSummary, error) {
	sum := FileSummary{
		Path:        path,
		SessionID:   strings.TrimSuffix(info.Name(), fileSuffix),
		ProjectPath: filepath.Dir(path),
		Mtime:       mtime,
	}
	// nosec G304: path is yielded by WalkDir on a caller-owned root.
	// gosec misreads the WalkDir contract as user-tainted.
	f, err := os.Open(path) //nolint:gosec
	if err != nil {
		return sum, fmt.Errorf("open: %w", err)
	}
	defer func() { _ = f.Close() }()

	hasher := sha256.New()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1024*1024), maxLineBytes)

	var (
		firstTS time.Time
		lastTS  time.Time
		count   int
	)
	for scanner.Scan() {
		line := scanner.Bytes()
		_, _ = hasher.Write(line)
		_, _ = hasher.Write([]byte{'\n'})
		t, ok := peekTurnMeta(line)
		if !ok {
			continue
		}
		count++
		if firstTS.IsZero() || t.Before(firstTS) {
			firstTS = t
		}
		if t.After(lastTS) {
			lastTS = t
		}
	}
	if err := scanner.Err(); err != nil {
		return sum, fmt.Errorf("scan: %w", err)
	}
	sum.ContentHash = hex.EncodeToString(hasher.Sum(nil))
	sum.FirstTS = firstTS
	sum.LastTS = lastTS
	sum.TurnCount = count
	return sum, nil
}

// peekTurnMeta extracts only the timestamp and type from one line,
// without decoding the full message payload. Returns ok=false for
// non-indexable types or malformed lines.
func peekTurnMeta(line []byte) (time.Time, bool) {
	var head struct {
		Type      string `json:"type"`
		Timestamp string `json:"timestamp"`
	}
	if err := json.Unmarshal(line, &head); err != nil {
		return time.Time{}, false
	}
	if !isIndexableType(head.Type) {
		return time.Time{}, false
	}
	if head.Timestamp == "" {
		// Indexable but timestamp-less — count it, but with a zero ts.
		// ts ordering will be best-effort.
		return time.Time{}, true
	}
	t, err := time.Parse(time.RFC3339Nano, head.Timestamp)
	if err != nil {
		return time.Time{}, true
	}
	return t, true
}

// ReadTurns streams the indexable turns in one jsonl file. Order
// matches file order; TurnIndex is assigned 0..N-1 across the
// indexable subset (non-indexable lines do not advance the counter).
//
// OnError, if non-nil, is invoked for each malformed line so the
// caller can log without losing the rest of the file.
func ReadTurns(path string, onErr func(line int, err error)) ([]Turn, error) {
	// nosec G304: path is from Walk above, which only emits paths
	// it found under a caller-owned root. No tainted input flows in.
	f, err := os.Open(path) //nolint:gosec
	if err != nil {
		return nil, fmt.Errorf("source: open %q: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	sessionID := strings.TrimSuffix(filepath.Base(path), fileSuffix)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1024*1024), maxLineBytes)

	var (
		turns []Turn
		idx   int
		ln    int
	)
	for scanner.Scan() {
		ln++
		raw := scanner.Bytes()
		// scanner.Bytes() reuses its underlying buffer across iterations
		// — copy before doing anything that captures the slice.
		lineCopy := append([]byte(nil), raw...)
		t, ok, decodeErr := decodeTurn(lineCopy, sessionID, idx)
		if decodeErr != nil {
			if onErr != nil {
				onErr(ln, decodeErr)
			}
			continue
		}
		if !ok {
			continue
		}
		turns = append(turns, t)
		idx++
	}
	if err := scanner.Err(); err != nil {
		return turns, fmt.Errorf("source: scan %q: %w", path, err)
	}
	return turns, nil
}

// decodeTurn parses one jsonl line into a Turn when it carries an
// indexable user/assistant message. Returns ok=false for skip-class
// lines (other types, empty content) without surfacing an error.
func decodeTurn(line []byte, sessionID string, idx int) (Turn, bool, error) {
	var head struct {
		Type      string          `json:"type"`
		Timestamp string          `json:"timestamp"`
		Message   json.RawMessage `json:"message"`
	}
	if err := json.Unmarshal(line, &head); err != nil {
		return Turn{}, false, err
	}
	if !isIndexableType(head.Type) {
		return Turn{}, false, nil
	}

	role, text, ok := decodeMessage(head.Message)
	if !ok {
		return Turn{}, false, nil
	}
	if strings.TrimSpace(text) == "" {
		return Turn{}, false, nil
	}
	// Prefer the message.role when present; fall back to the envelope
	// type. Both should agree but the envelope is more robust.
	if role == "" {
		role = head.Type
	}

	ts, _ := time.Parse(time.RFC3339Nano, head.Timestamp)
	return Turn{
		SessionID: sessionID,
		TurnIndex: idx,
		Role:      role,
		Text:      text,
		TS:        ts,
		RawSize:   len(line),
	}, true, nil
}

// decodeMessage handles the two shapes Claude Code emits:
//   - user: message.content is a plain string OR an array of blocks
//   - assistant: message.content is always an array of blocks
//
// Text from text + thinking blocks is concatenated with blank lines;
// tool_use / tool_result blocks are intentionally dropped.
func decodeMessage(raw json.RawMessage) (role, text string, ok bool) {
	if len(raw) == 0 {
		return "", "", false
	}
	var env struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return "", "", false
	}
	if len(env.Content) == 0 {
		return env.Role, "", false
	}

	// Try string content first.
	var s string
	if err := json.Unmarshal(env.Content, &s); err == nil {
		return env.Role, s, true
	}

	// Otherwise expect an array of blocks.
	var blocks []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		Thinking string `json:"thinking"`
	}
	if err := json.Unmarshal(env.Content, &blocks); err != nil {
		return env.Role, "", false
	}
	var parts []string
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		case "thinking":
			if b.Thinking != "" {
				parts = append(parts, b.Thinking)
			}
		}
	}
	if len(parts) == 0 {
		return env.Role, "", false
	}
	return env.Role, strings.Join(parts, "\n\n"), true
}

// isIndexableType is the v1 whitelist. Easy to extend later without
// changing the rest of the package.
func isIndexableType(t string) bool {
	return t == "user" || t == "assistant"
}

func notify(cb func(string, error), path string, err error) {
	if cb != nil {
		cb(path, err)
	}
}
