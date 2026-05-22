package search

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wm-it/claude-knowledge-vault/internal/store"
)

// ─── fixtures ─────────────────────────────────────────────────────────

func openStore(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "vault.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func seed(t *testing.T, db *store.DB, sessionID, content, role string, idx int) {
	t.Helper()
	ctx := context.Background()
	t0 := time.Date(2026, 5, 1, 12, idx, 0, 0, time.UTC)
	s := store.Session{
		ID:          sessionID,
		ProjectPath: "/p",
		FilePath:    "/p/" + sessionID + ".jsonl",
		FirstTS:     t0,
		LastTS:      t0,
		ContentHash: "h",
		LastMtime:   t0,
		TurnCount:   1,
	}
	if err := db.UpsertSession(ctx, &s); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertTurn(ctx, store.Turn{
		SessionID: sessionID, TurnIndex: idx, Role: role, TS: t0, RawSize: len(content),
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertChunk(ctx, &store.Chunk{
		SessionID: sessionID, TurnIndex: idx, Role: role, TS: t0,
		Title: role + " turn", Content: content,
	}); err != nil {
		t.Fatal(err)
	}
}

// ─── Run end-to-end ──────────────────────────────────────────────────

func TestRun_EmptyQuery(t *testing.T) {
	db := openStore(t)
	got, err := Run(context.Background(), db, &Options{Query: "   "})
	if err != nil || got != nil {
		t.Errorf("empty query: got %v / %v", got, err)
	}
}

func TestRun_NilStore(t *testing.T) {
	_, err := Run(context.Background(), nil, &Options{Query: "x"})
	if err == nil {
		t.Error("expected error on nil store")
	}
}

func TestRun_NilOptions(t *testing.T) {
	db := openStore(t)
	_, err := Run(context.Background(), db, nil)
	if err == nil {
		t.Error("expected error on nil options")
	}
}

func TestRun_EmptyStore(t *testing.T) {
	db := openStore(t)
	got, err := Run(context.Background(), db, &Options{Query: "hello"})
	if err != nil {
		t.Errorf("err: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected 0 results from empty store, got %d", len(got))
	}
}

func TestRun_BM25_HappyPath(t *testing.T) {
	db := openStore(t)
	seed(t, db, "s1",
		"decided that webhook signing must use HMAC-SHA256 with a 30 day rotation period",
		"user", 0)
	seed(t, db, "s1",
		"the alternative is storing tokens in the database which we rejected",
		"assistant", 1)

	got, err := Run(context.Background(), db, &Options{Query: "webhook signing"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("expected at least 1 hit")
	}
	if got[0].Source != SourceBM25 {
		t.Errorf("source=%q, want bm25", got[0].Source)
	}
	if got[0].Score <= 0 {
		t.Errorf("expected positive normalised score, got %f", got[0].Score)
	}
	if !strings.Contains(strings.ToLower(got[0].Snippet), "webhook") {
		t.Errorf("snippet missing 'webhook': %q", got[0].Snippet)
	}
}

func TestRun_TrigramFallback(t *testing.T) {
	db := openStore(t)
	// Content has "alphabravo" jammed together; BM25 won't tokenise
	// out "bravo" because porter sees a single token. Trigram does.
	seed(t, db, "s1", "alphabravo charlie delta", "user", 0)

	got, err := Run(context.Background(), db, &Options{Query: "bravo"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(got) == 0 {
		t.Fatal("expected trigram fallback hit")
	}
	if got[0].Source != SourceTrigram {
		t.Errorf("expected trigram source, got %q", got[0].Source)
	}
}

func TestRun_DisableFallback(t *testing.T) {
	db := openStore(t)
	seed(t, db, "s1", "alphabravo charlie delta", "user", 0)
	got, err := Run(context.Background(), db, &Options{
		Query: "bravo", DisableFallback: true,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected 0 hits with fallback disabled, got %d", len(got))
	}
}

func TestRun_RoleFilter(t *testing.T) {
	db := openStore(t)
	seed(t, db, "s1", "deployment notes go here", "user", 0)
	seed(t, db, "s1", "deployment notes echoed back", "assistant", 1)
	got, err := Run(context.Background(), db, &Options{
		Query: "deployment", Role: "assistant",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(got) != 1 || got[0].Role != "assistant" {
		t.Errorf("role filter failed: %+v", got)
	}
}

func TestRun_SinceFilter(t *testing.T) {
	db := openStore(t)
	seed(t, db, "s1", "old hit about deployment", "user", 0)
	seed(t, db, "s1", "new hit about deployment", "user", 5)
	since := time.Date(2026, 5, 1, 12, 3, 0, 0, time.UTC)
	got, err := Run(context.Background(), db, &Options{
		Query: "deployment", Since: since,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(got) != 1 || got[0].TurnIndex != 5 {
		t.Errorf("since filter failed: %+v", got)
	}
}

// ─── buildMATCH safety ───────────────────────────────────────────────

func TestBuildMATCH_EscapesInternalQuotes(t *testing.T) {
	// Boundary " is stripped by stripBoundaryPunct (it's punctuation),
	// so the only place a " survives into a term is *inside* a word
	// — e.g. when the user pasted a malformed string literal. fts5
	// phrase quoting then needs that " doubled to "".
	expr, terms := buildMATCH(`one say"foo three`)
	if !strings.Contains(expr, `"say""foo"`) {
		t.Errorf("internal quote not doubled: expr=%q", expr)
	}
	if len(terms) != 3 {
		t.Errorf("term count: got %d, want 3 (terms=%v)", len(terms), terms)
	}
}

func TestBuildMATCH_StripsBoundaryPunct(t *testing.T) {
	expr, terms := buildMATCH("hello, world! (foo)")
	for _, want := range []string{"hello", "world", "foo"} {
		if !contains(terms, want) {
			t.Errorf("missing term %q in %v (expr=%q)", want, terms, expr)
		}
	}
}

func TestBuildMATCH_KeepsInternalPunct(t *testing.T) {
	expr, _ := buildMATCH("HMAC-SHA256")
	// Hyphen kept inside the phrase quotes — important for technical IDs.
	if !strings.Contains(expr, `"HMAC-SHA256"`) {
		t.Errorf("internal hyphen dropped: %q", expr)
	}
}

func TestBuildMATCH_PunctuationOnly(t *testing.T) {
	expr, terms := buildMATCH("!!! ??? ###")
	if expr != "" {
		t.Errorf("expected empty MATCH for pure punctuation, got %q", expr)
	}
	if len(terms) != 0 {
		t.Errorf("expected 0 terms, got %v", terms)
	}
}

func TestRun_PunctuationOnlyShortCircuits(t *testing.T) {
	db := openStore(t)
	seed(t, db, "s1", "anything", "user", 0)
	got, err := Run(context.Background(), db, &Options{Query: "!!!"})
	if err != nil || got != nil {
		t.Errorf("punct-only: got %v / %v", got, err)
	}
}

// ─── snippet extraction ──────────────────────────────────────────────

func TestExtractSnippet_ShortContentReturnedWhole(t *testing.T) {
	got := extractSnippet("just twelve chars", []string{"chars"}, 240)
	if got != "just twelve chars" {
		t.Errorf("short content should pass through: got %q", got)
	}
}

func TestExtractSnippet_CentredOnFirstHit(t *testing.T) {
	long := strings.Repeat("padding ", 50) + "TARGET " + strings.Repeat("trailer ", 50)
	got := extractSnippet(long, []string{"target"}, 80)
	if !strings.Contains(got, "TARGET") {
		t.Errorf("snippet should contain target: %q", got)
	}
	if !strings.HasPrefix(got, "… ") || !strings.HasSuffix(got, " …") {
		t.Errorf("expected ellipsis on both sides: %q", got)
	}
}

func TestExtractSnippet_NoHitFallsBackToHead(t *testing.T) {
	long := strings.Repeat("abc ", 200)
	got := extractSnippet(long, []string{"nope"}, 40)
	if strings.HasPrefix(got, "… ") {
		t.Errorf("with no hit, window should start at head: %q", got)
	}
	if !strings.HasSuffix(got, " …") {
		t.Errorf("window should ellipsis on the right: %q", got)
	}
}

func TestExtractSnippet_UnicodeBoundary(t *testing.T) {
	// Multi-byte runes (한글) — ensure no mid-rune slice panics.
	long := strings.Repeat("가나다 ", 80) + "마커 " + strings.Repeat("바사아 ", 80)
	got := extractSnippet(long, []string{"마커"}, 60)
	if !strings.Contains(got, "마커") {
		t.Errorf("unicode snippet missing marker: %q", got)
	}
}

// ─── helpers ─────────────────────────────────────────────────────────

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
