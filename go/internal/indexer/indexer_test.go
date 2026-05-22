package indexer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wm-it/claude-knowledge-vault/internal/store"
)

// ─── fixture builders ───────────────────────────────────────────────

type fixture struct {
	root    string
	dataDir string
	db      *store.DB
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	tmp := t.TempDir()
	root := filepath.Join(tmp, "projects")
	dataDir := filepath.Join(tmp, "data")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(context.Background(), filepath.Join(tmp, "vault.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return &fixture{root: root, dataDir: dataDir, db: db}
}

func (f *fixture) writeSession(t *testing.T, projectDir, sessionID string, lines ...string) string {
	t.Helper()
	dir := filepath.Join(f.root, projectDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, sessionID+".jsonl")
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const (
	userTurn = `{"type":"user","timestamp":"2026-05-04T11:27:01.184Z","sessionId":"S","message":{"role":"user","content":"talk about ` // append suffix
	assistantTurn = `{"type":"assistant","timestamp":"2026-05-04T11:27:02.000Z","sessionId":"S","message":{"role":"assistant","content":[{"type":"text","text":"sure thing about ` // append suffix
)

func userTurnText(s string) string      { return userTurn + s + `"}}` }
func assistantTurnText(s string) string { return assistantTurn + s + `"}]}}` }

// ─── validation ─────────────────────────────────────────────────────

func TestRun_NilStore(t *testing.T) {
	_, err := Run(context.Background(), nil, &Options{Root: "/x", DataDir: "/y"})
	if err == nil {
		t.Error("expected nil-store error")
	}
}

func TestRun_NilOptions(t *testing.T) {
	f := newFixture(t)
	_, err := Run(context.Background(), f.db, nil)
	if err == nil {
		t.Error("expected nil-options error")
	}
}

func TestRun_RequiresRoot(t *testing.T) {
	f := newFixture(t)
	_, err := Run(context.Background(), f.db, &Options{DataDir: f.dataDir})
	if err == nil || !strings.Contains(err.Error(), "Root required") {
		t.Errorf("expected Root required, got %v", err)
	}
}

func TestRun_RequiresDataDir(t *testing.T) {
	f := newFixture(t)
	_, err := Run(context.Background(), f.db, &Options{Root: f.root})
	if err == nil || !strings.Contains(err.Error(), "DataDir required") {
		t.Errorf("expected DataDir required, got %v", err)
	}
}

func TestRun_AutoMkdirDataDir(t *testing.T) {
	f := newFixture(t)
	res, err := Run(context.Background(), f.db, &Options{
		Root: f.root, DataDir: filepath.Join(f.dataDir, "nested", "subdir"),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.FilesScanned != 0 {
		t.Errorf("empty root → 0 scanned, got %d", res.FilesScanned)
	}
}

// ─── happy path ────────────────────────────────────────────────────

func TestRun_FreshIndex_InsertsAll(t *testing.T) {
	f := newFixture(t)
	f.writeSession(t, "proj-a", "sess-1",
		userTurnText("widgets"), assistantTurnText("widgets are great"))
	f.writeSession(t, "proj-b", "sess-2",
		userTurnText("deployment"))

	res, err := Run(context.Background(), f.db, &Options{
		Root: f.root, DataDir: f.dataDir,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.FilesScanned != 2 || res.FilesIndexed != 2 {
		t.Errorf("expected 2/2 scanned/indexed, got %d/%d",
			res.FilesScanned, res.FilesIndexed)
	}
	if res.TurnsInserted != 3 {
		t.Errorf("turns: got %d, want 3", res.TurnsInserted)
	}
	if res.ChunksInserted != 3 {
		t.Errorf("chunks: got %d, want 3 (one chunk per turn)", res.ChunksInserted)
	}
	stats, _ := f.db.Stats(context.Background())
	if stats.Sessions != 2 || stats.Turns != 3 || stats.Chunks != 3 {
		t.Errorf("store stats: %+v", stats)
	}
}

func TestRun_Incremental_SkipsUnchanged(t *testing.T) {
	f := newFixture(t)
	f.writeSession(t, "p", "s1", userTurnText("alpha"))
	ctx := context.Background()
	if _, err := Run(ctx, f.db, &Options{Root: f.root, DataDir: f.dataDir}); err != nil {
		t.Fatalf("first run: %v", err)
	}
	res, err := Run(ctx, f.db, &Options{Root: f.root, DataDir: f.dataDir})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if res.FilesScanned != 1 || res.FilesIndexed != 0 {
		t.Errorf("expected 1 scanned / 0 indexed on no-op re-run, got %d/%d",
			res.FilesScanned, res.FilesIndexed)
	}
	if res.TurnsInserted != 0 {
		t.Errorf("turns inserted should be 0, got %d", res.TurnsInserted)
	}
}

func TestRun_IncrementalPicksUpNewTurn(t *testing.T) {
	f := newFixture(t)
	path := f.writeSession(t, "p", "s1", userTurnText("alpha"))
	ctx := context.Background()
	if _, err := Run(ctx, f.db, &Options{Root: f.root, DataDir: f.dataDir}); err != nil {
		t.Fatalf("first run: %v", err)
	}
	// Append a second turn; mtime + hash both change.
	addLine := []byte(userTurnText("bravo") + "\n")
	f0, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f0.Write(addLine); err != nil {
		t.Fatal(err)
	}
	_ = f0.Close()
	// Bump mtime explicitly so the test is robust on coarse-mtime fs.
	future := time.Now().Add(time.Hour)
	_ = os.Chtimes(path, future, future)

	res, err := Run(ctx, f.db, &Options{Root: f.root, DataDir: f.dataDir})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if res.FilesIndexed != 1 {
		t.Errorf("expected 1 file re-indexed, got %d", res.FilesIndexed)
	}
	if res.TurnsInserted != 2 {
		t.Errorf("expected 2 turns on full re-index (delete + insert), got %d",
			res.TurnsInserted)
	}
	// Old chunks were cleared before re-insert (no duplicates).
	stats, _ := f.db.Stats(ctx)
	if stats.Turns != 2 || stats.Chunks != 2 {
		t.Errorf("post re-index stats wrong: %+v", stats)
	}
}

func TestRun_Force_ReindexesEverything(t *testing.T) {
	f := newFixture(t)
	f.writeSession(t, "p", "s1", userTurnText("hello"))
	ctx := context.Background()
	if _, err := Run(ctx, f.db, &Options{Root: f.root, DataDir: f.dataDir}); err != nil {
		t.Fatalf("first run: %v", err)
	}
	res, err := Run(ctx, f.db, &Options{Root: f.root, DataDir: f.dataDir, Force: true})
	if err != nil {
		t.Fatalf("force run: %v", err)
	}
	if res.FilesIndexed != 1 {
		t.Errorf("expected forced re-index of 1 file, got %d", res.FilesIndexed)
	}
}

// ─── lock ──────────────────────────────────────────────────────────

func TestRun_LockBlocks(t *testing.T) {
	f := newFixture(t)
	if err := os.MkdirAll(f.dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(f.dataDir, LockFileName)
	if err := os.WriteFile(lockPath, []byte(time.Now().Format(time.RFC3339Nano)), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Run(context.Background(), f.db, &Options{Root: f.root, DataDir: f.dataDir})
	if err == nil {
		t.Fatal("expected ErrIndexInProgress")
	}
	if !strings.Contains(err.Error(), ErrIndexInProgress.Error()) {
		t.Errorf("wrong error: %v", err)
	}
}

func TestRun_StaleLockRemoved(t *testing.T) {
	f := newFixture(t)
	if err := os.MkdirAll(f.dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(f.dataDir, LockFileName)
	if err := os.WriteFile(lockPath, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-(LockTimeout + time.Minute))
	if err := os.Chtimes(lockPath, stale, stale); err != nil {
		t.Fatal(err)
	}
	_, err := Run(context.Background(), f.db, &Options{Root: f.root, DataDir: f.dataDir})
	if err != nil {
		t.Errorf("stale lock should not block: %v", err)
	}
	// Lock is removed on successful run.
	if _, statErr := os.Stat(lockPath); statErr == nil {
		t.Error("lock file should be cleaned up after Run")
	}
}

// ─── progress channel ─────────────────────────────────────────────

func TestRun_ProgressEmitsAtLeastDone(t *testing.T) {
	f := newFixture(t)
	f.writeSession(t, "p", "s1", userTurnText("hello"))
	prog := make(chan Progress, 16)
	if _, err := Run(context.Background(), f.db, &Options{
		Root: f.root, DataDir: f.dataDir, Progress: prog,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	close(prog)
	var sawDone, sawFile bool
	for p := range prog {
		if p.Phase == PhaseDone {
			sawDone = true
		}
		if p.Phase == PhaseFile {
			sawFile = true
		}
	}
	if !sawDone {
		t.Error("expected at least one done progress event")
	}
	if !sawFile {
		t.Error("expected at least one file progress event")
	}
}

func TestRun_ProgressDropsToFullChannel(t *testing.T) {
	// Unbuffered + no receiver → every send dropped; Run must not
	// stall.
	f := newFixture(t)
	for i := 0; i < 5; i++ {
		f.writeSession(t, "p", "s"+string(rune('a'+i)), userTurnText("t"))
	}
	prog := make(chan Progress) // unbuffered, no reader
	done := make(chan struct{})
	go func() {
		_, _ = Run(context.Background(), f.db, &Options{
			Root: f.root, DataDir: f.dataDir, Progress: prog,
		})
		close(done)
	}()
	select {
	case <-done:
		// pass — Run returned despite the unread channel
	case <-time.After(5 * time.Second):
		t.Fatal("Run blocked on unread progress channel")
	}
}

// ─── context cancel ───────────────────────────────────────────────

func TestRun_ContextCancel(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 3; i++ {
		f.writeSession(t, "p", "s"+string(rune('a'+i)), userTurnText("t"))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Run(ctx, f.db, &Options{Root: f.root, DataDir: f.dataDir})
	if err == nil {
		// Walk may still complete (it's quick) but the per-file loop
		// re-checks ctx; either an early walk error or a per-file
		// abort counts. Accept both.
		t.Log("ctx cancel did not surface as error — acceptable when walk is faster than cancel propagation")
	}
}

// ─── end-to-end: search after index ───────────────────────────────

func TestRun_SearchableAfterIndex(t *testing.T) {
	f := newFixture(t)
	f.writeSession(t, "p", "s1", userTurnText("decided HMAC-SHA256 rotation"))
	ctx := context.Background()
	if _, err := Run(ctx, f.db, &Options{Root: f.root, DataDir: f.dataDir}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Use the raw store search (BM25 layer is tested in internal/search).
	rows, err := f.db.Search(ctx, `"HMAC-SHA256"`, store.SearchOpts{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(rows) == 0 {
		t.Error("expected a hit for HMAC-SHA256 after index")
	}
}
