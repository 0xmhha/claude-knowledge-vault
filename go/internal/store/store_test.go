package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ─── fixtures ─────────────────────────────────────────────────────────

func openTest(t *testing.T) *DB {
	t.Helper()
	dir := t.TempDir()
	db, err := Open(context.Background(), filepath.Join(dir, "vault.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func sampleSession(id string) Session {
	t0 := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	return Session{
		ID:          id,
		ProjectPath: "/home/u/.claude/projects/abc",
		FilePath:    "/home/u/.claude/projects/abc/" + id + ".jsonl",
		FirstTS:     t0,
		LastTS:      t0.Add(time.Hour),
		ContentHash: "sha256:deadbeef",
		LastMtime:   t0.Add(time.Hour),
		TurnCount:   2,
	}
}

func seed(t *testing.T, db *DB, sessionID, content, role string, idx int) {
	t.Helper()
	ctx := context.Background()
	s := sampleSession(sessionID)
	if err := db.UpsertSession(ctx, &s); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}
	ts := time.Date(2026, 5, 1, 12, idx, 0, 0, time.UTC)
	if err := db.InsertTurn(ctx, Turn{
		SessionID: sessionID, TurnIndex: idx, Role: role, TS: ts, RawSize: len(content),
	}); err != nil {
		t.Fatalf("InsertTurn: %v", err)
	}
	if err := db.InsertChunk(ctx, &Chunk{
		SessionID: sessionID, TurnIndex: idx, Role: role, TS: ts,
		Title: role + " turn " + sessionID, Content: content,
	}); err != nil {
		t.Fatalf("InsertChunk: %v", err)
	}
}

// ─── lifecycle ────────────────────────────────────────────────────────

func TestOpen_CreatesFile(t *testing.T) {
	db := openTest(t)
	if db.Path() == "" {
		t.Error("Path() empty after Open")
	}
}

func TestOpen_ErrEmptyPath(t *testing.T) {
	_, err := Open(context.Background(), "")
	if err == nil {
		t.Error("expected error for empty path")
	}
}

func TestClose_Idempotent(t *testing.T) {
	db := openTest(t)
	if err := db.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	// modernc.org/sqlite's database/sql Close is idempotent, but the
	// nil-DB guard in our wrapper covers the "Close after zero-value"
	// case too.
	var zero *DB
	if err := zero.Close(); err != nil {
		t.Errorf("nil DB Close should be no-op, got: %v", err)
	}
}

// ─── migration ────────────────────────────────────────────────────────

func TestMigrate_Idempotent(t *testing.T) {
	db := openTest(t)
	// Already migrated by Open; a second call must be a no-op.
	if err := db.Migrate(context.Background()); err != nil {
		t.Errorf("re-Migrate: %v", err)
	}
	var v int
	if err := db.sql.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != currentSchemaVersion {
		t.Errorf("user_version=%d, want %d", v, currentSchemaVersion)
	}
}

func TestMigrate_RefusesFutureVersion(t *testing.T) {
	db := openTest(t)
	if _, err := db.sql.Exec(`PRAGMA user_version = 999`); err != nil {
		t.Fatal(err)
	}
	err := db.Migrate(context.Background())
	if err == nil || !strings.Contains(err.Error(), "older than the DB") {
		t.Errorf("expected refusal, got: %v", err)
	}
}

// ─── session CRUD ─────────────────────────────────────────────────────

func TestUpsertSession_InsertThenUpdate(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	s := sampleSession("s1")
	if err := db.UpsertSession(ctx, &s); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	// Bump turn count and re-upsert.
	s.TurnCount = 42
	if err := db.UpsertSession(ctx, &s); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	got, ok, err := db.GetSession(ctx, "s1")
	if err != nil || !ok {
		t.Fatalf("GetSession: err=%v ok=%v", err, ok)
	}
	if got.TurnCount != 42 {
		t.Errorf("turn_count=%d, want 42", got.TurnCount)
	}
	if !got.FirstTS.Equal(s.FirstTS) {
		t.Errorf("first_ts mismatch: got %v want %v", got.FirstTS, s.FirstTS)
	}
}

func TestGetSession_Missing(t *testing.T) {
	db := openTest(t)
	_, ok, err := db.GetSession(context.Background(), "nope")
	if err != nil {
		t.Errorf("err: %v", err)
	}
	if ok {
		t.Error("expected !ok for missing session")
	}
}

// ─── turn + chunk write ───────────────────────────────────────────────

func TestInsertTurn_Idempotent(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	s := sampleSession("s1")
	if err := db.UpsertSession(ctx, &s); err != nil {
		t.Fatal(err)
	}
	turn := Turn{
		SessionID: "s1", TurnIndex: 0, Role: "user",
		TS: time.Unix(1700000000, 0), RawSize: 12,
	}
	if err := db.InsertTurn(ctx, turn); err != nil {
		t.Fatal(err)
	}
	// Same PK → INSERT OR REPLACE; row count stays 1.
	if err := db.InsertTurn(ctx, turn); err != nil {
		t.Fatalf("re-insert turn: %v", err)
	}
	stats, err := db.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Turns != 1 {
		t.Errorf("turn count=%d, want 1", stats.Turns)
	}
}

func TestInsertChunk_BothIndexes(t *testing.T) {
	db := openTest(t)
	seed(t, db, "s1", "the quick brown fox jumps over a lazy dog", "user", 0)
	// Each InsertChunk should land in both chunks + chunks_trigram.
	var (
		primary  int
		trigram  int
	)
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM chunks`).Scan(&primary); err != nil {
		t.Fatal(err)
	}
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM chunks_trigram`).Scan(&trigram); err != nil {
		t.Fatal(err)
	}
	if primary != 1 || trigram != 1 {
		t.Errorf("primary=%d trigram=%d, want 1/1", primary, trigram)
	}
}

// ─── search ───────────────────────────────────────────────────────────

func TestSearch_EmptyQuery(t *testing.T) {
	db := openTest(t)
	res, err := db.Search(context.Background(), "   ", SearchOpts{})
	if err != nil {
		t.Errorf("err: %v", err)
	}
	if len(res) != 0 {
		t.Errorf("expected 0 results for empty query, got %d", len(res))
	}
}

// AC from IMPL_PLAN T-C.4: empty DB + Search("hello") → (0 rows, nil).
func TestSearch_EmptyDB(t *testing.T) {
	db := openTest(t)
	res, err := db.Search(context.Background(), "hello", SearchOpts{})
	if err != nil {
		t.Errorf("err: %v", err)
	}
	if len(res) != 0 {
		t.Errorf("expected 0 results from empty DB, got %d", len(res))
	}
}

func TestSearch_BasicMATCH(t *testing.T) {
	db := openTest(t)
	seed(t, db, "s1", "decided webhook signing must use HMAC-SHA256", "user", 0)
	seed(t, db, "s1", "alternative is storing tokens in the database", "assistant", 1)

	res, err := db.Search(context.Background(), "webhook", SearchOpts{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res) == 0 {
		t.Fatal("expected at least 1 result for 'webhook'")
	}
	if !strings.Contains(strings.ToLower(res[0].Content), "webhook") {
		t.Errorf("top hit content missing 'webhook': %q", res[0].Content)
	}
	if res[0].SessionID != "s1" || res[0].TurnIndex != 0 {
		t.Errorf("expected s1/0, got %s/%d", res[0].SessionID, res[0].TurnIndex)
	}
}

func TestSearch_PorterStemming(t *testing.T) {
	// Porter tokenizer should stem "running" → "run" so a query for
	// "run" matches a chunk containing "running".
	db := openTest(t)
	seed(t, db, "s1", "the tests were running for an hour", "user", 0)
	res, err := db.Search(context.Background(), "run", SearchOpts{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res) == 0 {
		t.Error("expected porter stemming to match 'run' against 'running'")
	}
}

func TestSearch_TrigramFallback(t *testing.T) {
	db := openTest(t)
	seed(t, db, "s1", "alpha bravo charlie delta echo foxtrot", "user", 0)
	// Trigram index supports partial substring; chunks (porter) does not.
	res, err := db.Search(context.Background(), "char", SearchOpts{Source: "trigram"})
	if err != nil {
		t.Fatalf("Search trigram: %v", err)
	}
	if len(res) == 0 {
		t.Error("expected trigram to match 'char' against 'charlie'")
	}
}

func TestSearch_RoleFilter(t *testing.T) {
	db := openTest(t)
	seed(t, db, "s1", "user message about widgets", "user", 0)
	seed(t, db, "s1", "assistant message about widgets", "assistant", 1)
	res, err := db.Search(context.Background(), "widgets", SearchOpts{Role: "assistant"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, r := range res {
		if r.Role != "assistant" {
			t.Errorf("role filter leaked: got role=%q", r.Role)
		}
	}
	if len(res) != 1 {
		t.Errorf("expected exactly 1 assistant hit, got %d", len(res))
	}
}

func TestSearch_SinceFilter(t *testing.T) {
	db := openTest(t)
	// seed two turns, idx=0 at minute 0, idx=5 at minute 5
	seed(t, db, "s1", "old turn about widgets", "user", 0)
	seed(t, db, "s1", "new turn about widgets", "user", 5)
	since := time.Date(2026, 5, 1, 12, 3, 0, 0, time.UTC) // between them
	res, err := db.Search(context.Background(), "widgets", SearchOpts{Since: since})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res) != 1 {
		t.Errorf("expected 1 result after Since, got %d", len(res))
	}
	if res[0].TurnIndex != 5 {
		t.Errorf("expected turn_index=5, got %d", res[0].TurnIndex)
	}
}

// ─── stats / purge / delete ───────────────────────────────────────────

func TestStats_Counts(t *testing.T) {
	db := openTest(t)
	seed(t, db, "s1", "first chunk", "user", 0)
	seed(t, db, "s2", "second chunk", "user", 0)
	stats, err := db.Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Sessions != 2 || stats.Turns != 2 || stats.Chunks != 2 {
		t.Errorf("got %+v, want 2/2/2", stats)
	}
}

func TestPurge_ClearsAll(t *testing.T) {
	db := openTest(t)
	seed(t, db, "s1", "stuff", "user", 0)
	if err := db.Purge(context.Background()); err != nil {
		t.Fatalf("Purge: %v", err)
	}
	stats, _ := db.Stats(context.Background())
	if stats != (Stats{}) {
		t.Errorf("Purge left rows: %+v", stats)
	}
	// Schema still usable after purge → re-seeding works.
	seed(t, db, "s1", "fresh", "user", 0)
	stats, _ = db.Stats(context.Background())
	if stats.Chunks != 1 {
		t.Errorf("post-purge insert failed: %+v", stats)
	}
}

func TestDeleteSession_CascadesToChunks(t *testing.T) {
	db := openTest(t)
	seed(t, db, "s1", "to be deleted", "user", 0)
	seed(t, db, "s2", "kept", "user", 0)
	if err := db.DeleteSession(context.Background(), "s1"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}

	// nil-arg guards on the write entry points.
	if err := db.UpsertSession(context.Background(), nil); err == nil {
		t.Error("expected error on nil session")
	}
	if err := db.InsertChunk(context.Background(), nil); err == nil {
		t.Error("expected error on nil chunk")
	}
	// firstLine helper coverage.
	if got := firstLine("foo\nbar"); got != "foo" {
		t.Errorf("firstLine multi-line: got %q", got)
	}
	if got := firstLine("single"); got != "single" {
		t.Errorf("firstLine single: got %q", got)
	}
	stats, _ := db.Stats(context.Background())
	if stats.Sessions != 1 || stats.Turns != 1 || stats.Chunks != 1 {
		t.Errorf("post-delete stats wrong: %+v", stats)
	}
	// And the deleted session is really gone (search must not return s1).
	res, _ := db.Search(context.Background(), "deleted", SearchOpts{})
	for _, r := range res {
		if r.SessionID == "s1" {
			t.Errorf("found chunk for deleted session s1: %+v", r)
		}
	}
}
