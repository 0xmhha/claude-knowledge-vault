package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wm-it/claude-knowledge-vault/internal/store"
)

// ─── path resolution ────────────────────────────────────────────────

func TestResolvePluginData_FlagWins(t *testing.T) {
	got, err := resolvePluginData("/tmp/explicit")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/tmp/explicit" {
		t.Errorf("flag override: got %q", got)
	}
}

func TestResolvePluginData_EnvFallback(t *testing.T) {
	t.Setenv("KVAULT_DATA", "/tmp/from-env")
	t.Setenv("CLAUDE_PLUGIN_DATA", "/tmp/from-plugin")
	got, err := resolvePluginData("")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/tmp/from-env" {
		t.Errorf("KVAULT_DATA precedence: got %q", got)
	}
}

func TestResolvePluginData_ClaudePluginDataFallback(t *testing.T) {
	t.Setenv("KVAULT_DATA", "")
	t.Setenv("CLAUDE_PLUGIN_DATA", "/tmp/from-plugin")
	got, err := resolvePluginData("")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/tmp/from-plugin" {
		t.Errorf("CLAUDE_PLUGIN_DATA fallback: got %q", got)
	}
}

func TestResolvePluginData_DefaultUsesCacheDir(t *testing.T) {
	t.Setenv("KVAULT_DATA", "")
	t.Setenv("CLAUDE_PLUGIN_DATA", "")
	got, err := resolvePluginData("")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(got, "claude-knowledge-vault") {
		t.Errorf("default should end in claude-knowledge-vault, got %q", got)
	}
}

func TestResolveRoot_FlagWins(t *testing.T) {
	got, err := resolveRoot("/tmp/projects")
	if err != nil || got != "/tmp/projects" {
		t.Errorf("flag override: got=%q err=%v", got, err)
	}
}

func TestResolveRoot_ClaudeHome(t *testing.T) {
	t.Setenv("CLAUDE_HOME", "/opt/claude")
	got, err := resolveRoot("")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/opt/claude/projects" {
		t.Errorf("expected /opt/claude/projects, got %q", got)
	}
}

func TestResolveRoot_DefaultClaudeProjects(t *testing.T) {
	t.Setenv("CLAUDE_HOME", "")
	got, err := resolveRoot("")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(got, filepath.Join(".claude", "projects")) {
		t.Errorf("default should end in .claude/projects, got %q", got)
	}
}

// ─── parseSince ─────────────────────────────────────────────────────

func TestParseSince_EmptyIsZero(t *testing.T) {
	got, err := parseSince("")
	if err != nil || !got.IsZero() {
		t.Errorf("empty since: got %v err=%v", got, err)
	}
}

func TestParseSince_RFC3339(t *testing.T) {
	got, err := parseSince("2026-05-01T00:00:00Z")
	if err != nil || got.Year() != 2026 || got.Month() != time.May {
		t.Errorf("parsed wrong: got %v err=%v", got, err)
	}
}

func TestParseSince_RejectsDuration(t *testing.T) {
	_, err := parseSince("30d")
	if err == nil {
		t.Error("expected error for 30d (PoC supports RFC3339 only)")
	}
}

// ─── buildTools surface ─────────────────────────────────────────────

func TestBuildTools_RegistersFour(t *testing.T) {
	db := openTestDB(t)
	tools := buildTools(db, "/tmp", "/tmp")
	if len(tools) != 4 {
		t.Fatalf("want 4 tools, got %d", len(tools))
	}
	want := map[string]bool{
		"kv_index": true, "kv_search": true, "kv_stats": true, "kv_purge": true,
	}
	for _, tool := range tools {
		if !want[tool.Name()] {
			t.Errorf("unexpected tool name %q", tool.Name())
		}
	}
}

func TestKvTool_SchemasAreValidJSON(t *testing.T) {
	db := openTestDB(t)
	for _, op := range []string{"index", "search", "stats", "purge"} {
		tool := &kvTool{db: db, root: "/tmp", data: "/tmp", op: op}
		s := tool.InputSchema()
		if len(s) == 0 {
			t.Errorf("op=%s: empty schema", op)
		}
		var dst any
		if err := json.Unmarshal(s, &dst); err != nil {
			t.Errorf("op=%s: schema not JSON: %v", op, err)
		}
		if tool.Description() == "" {
			t.Errorf("op=%s: empty description", op)
		}
	}
}

func TestKvTool_UnknownOp(t *testing.T) {
	tool := &kvTool{op: "what"}
	_, isError, err := tool.Call(context.Background(), nil)
	if err == nil || !isError {
		t.Errorf("expected error+isError for unknown op")
	}
}

// ─── kv_search end-to-end (with secret masking) ─────────────────────

func TestKvSearch_MasksSecretsInSnippet(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	// Seed a session whose content contains an Anthropic-shaped key.
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	s := store.Session{
		ID: "s1", ProjectPath: "/p", FilePath: "/p/s1.jsonl",
		FirstTS: now, LastTS: now, ContentHash: "h", LastMtime: now, TurnCount: 1,
	}
	if err := db.UpsertSession(ctx, &s); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertTurn(ctx, store.Turn{
		SessionID: "s1", TurnIndex: 0, Role: "user", TS: now, RawSize: 100,
	}); err != nil {
		t.Fatal(err)
	}
	leak := "sk-ant-AAAABBBBCCCCDDDDEEEEFFFFGGGGHHHH1234567890XYZ"
	if err := db.InsertChunk(ctx, &store.Chunk{
		SessionID: "s1", TurnIndex: 0, Role: "user", TS: now,
		Title: "Decisions", Content: "we kept the token " + leak + " thanks",
	}); err != nil {
		t.Fatal(err)
	}

	tool := &kvTool{db: db, root: "/tmp", data: "/tmp", op: "search"}
	out, isError, err := tool.Call(ctx, json.RawMessage(`{"query":"token thanks"}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if isError {
		t.Fatalf("unexpected isError; out=%s", out)
	}
	if strings.Contains(out, leak) {
		t.Errorf("secret leaked into search output: %s", out)
	}
	if !strings.Contains(out, "REDACTED") {
		t.Errorf("expected REDACTED marker: %s", out)
	}
}

// ─── kv_purge requires confirm ─────────────────────────────────────

func TestKvPurge_RequiresConfirm(t *testing.T) {
	db := openTestDB(t)
	tool := &kvTool{db: db, op: "purge"}
	out, isError, err := tool.Call(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !isError {
		t.Errorf("expected isError without confirm")
	}
	if !strings.Contains(out, "confirm") {
		t.Errorf("expected confirm hint, got %q", out)
	}
}

func TestKvPurge_WithConfirm(t *testing.T) {
	db := openTestDB(t)
	tool := &kvTool{db: db, op: "purge"}
	out, isError, err := tool.Call(context.Background(), json.RawMessage(`{"confirm":true}`))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if isError || !strings.Contains(out, "purged") {
		t.Errorf("expected 'purged' success, got out=%q isError=%v", out, isError)
	}
}

// ─── kv_search rejects empty query ─────────────────────────────────

func TestKvSearch_EmptyQueryRejected(t *testing.T) {
	db := openTestDB(t)
	tool := &kvTool{db: db, op: "search"}
	out, isError, _ := tool.Call(context.Background(), json.RawMessage(`{"query":""}`))
	if !isError || !strings.Contains(out, "required") {
		t.Errorf("expected 'query required' error, got %q", out)
	}
}

// ─── envInt ────────────────────────────────────────────────────────

func TestEnvInt_Fallback(t *testing.T) {
	t.Setenv("EXISTS_BUT_BAD", "not-a-number")
	if got := envInt("EXISTS_BUT_BAD", 7); got != 7 {
		t.Errorf("expected fallback 7, got %d", got)
	}
	t.Setenv("VALID", "42")
	if got := envInt("VALID", 0); got != 42 {
		t.Errorf("expected 42, got %d", got)
	}
	t.Setenv("MISSING", "")
	if got := envInt("MISSING", 99); got != 99 {
		t.Errorf("expected fallback 99 on empty, got %d", got)
	}
}

// ─── helpers ───────────────────────────────────────────────────────

func openTestDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "vault.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}
