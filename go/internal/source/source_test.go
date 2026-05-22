package source

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ─── fixtures ─────────────────────────────────────────────────────────

// jsonl turn shape constants — extracted so individual tests can
// compose specific corpora without copy-pasting raw JSON everywhere.
const (
	userStringContent = `{"type":"user","timestamp":"2026-05-04T11:27:01.184Z","sessionId":"S","message":{"role":"user","content":"buddy mcp status"}}`

	assistantBlocks = `{"type":"assistant","timestamp":"2026-05-04T11:27:02.000Z","sessionId":"S","message":{"role":"assistant","content":[` +
		`{"type":"thinking","thinking":"hmm, mcp"},` +
		`{"type":"text","text":"here is the status"},` +
		`{"type":"tool_use","name":"Bash","input":{"command":"echo hi"}}` +
		`]}}`

	systemMeta = `{"type":"system","timestamp":"2026-05-04T11:27:00.000Z","subtype":"init"}`
	attachment = `{"type":"attachment","uuid":"x"}`
	lastPrompt = `{"type":"last-prompt","leafUuid":"x","sessionId":"S"}`
	emptyText  = `{"type":"user","timestamp":"2026-05-04T11:27:03.000Z","message":{"role":"user","content":"   "}}`
	noContent  = `{"type":"assistant","timestamp":"2026-05-04T11:27:04.000Z","message":{"role":"assistant","content":[]}}`
)

func writeJSONL(t *testing.T, dir, name string, lines ...string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// ─── Walk ────────────────────────────────────────────────────────────

func TestWalk_EmptyRoot(t *testing.T) {
	dir := t.TempDir()
	got, err := Walk(context.Background(), WalkOptions{Root: dir})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil summaries for empty dir, got %d", len(got))
	}
}

func TestWalk_NoRootReturnsNil(t *testing.T) {
	got, err := Walk(context.Background(), WalkOptions{
		Root: filepath.Join(t.TempDir(), "does-not-exist"),
	})
	if err != nil || got != nil {
		t.Errorf("non-existent root: got %v / %v, want (nil, nil)", got, err)
	}
}

func TestWalk_RootRequired(t *testing.T) {
	_, err := Walk(context.Background(), WalkOptions{})
	if err == nil {
		t.Error("expected error on empty Root")
	}
}

func TestWalk_FindsJsonlAndSummarises(t *testing.T) {
	root := t.TempDir()
	// project subdir mirroring real ~/.claude/projects/<dir>/<uuid>.jsonl
	p := writeJSONL(t, filepath.Join(root, "proj-a"), "session-1.jsonl",
		userStringContent, assistantBlocks, systemMeta, attachment)
	_ = writeJSONL(t, filepath.Join(root, "proj-a"), "not-a-session.txt",
		"ignore me")

	got, err := Walk(context.Background(), WalkOptions{Root: root})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 summary, got %d", len(got))
	}
	s := got[0]
	if s.Path != p {
		t.Errorf("path mismatch: %s", s.Path)
	}
	if s.SessionID != "session-1" {
		t.Errorf("session id: got %q", s.SessionID)
	}
	if s.TurnCount != 2 {
		t.Errorf("expected 2 indexable turns (user + assistant), got %d", s.TurnCount)
	}
	if s.ContentHash == "" {
		t.Errorf("content hash empty")
	}
	if s.FirstTS.IsZero() || s.LastTS.IsZero() {
		t.Errorf("timestamps not populated: %+v", s)
	}
	if !s.FirstTS.Before(s.LastTS) && !s.FirstTS.Equal(s.LastTS) {
		t.Errorf("first/last ordering wrong: first=%v last=%v", s.FirstTS, s.LastTS)
	}
}

func TestWalk_MtimeGate(t *testing.T) {
	root := t.TempDir()
	path := writeJSONL(t, root, "s.jsonl", userStringContent)
	// Set mtime to a known past.
	past := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(path, past, past); err != nil {
		t.Fatal(err)
	}
	since := past.Add(time.Hour) // gate is after the file's mtime
	got, err := Walk(context.Background(), WalkOptions{Root: root, Since: since})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("mtime gate failed: got %d summaries", len(got))
	}
}

func TestWalk_SkipsNonRegularFiles(t *testing.T) {
	root := t.TempDir()
	// Write a real file then symlink to it; the symlink should be
	// skipped as non-regular.
	realPath := writeJSONL(t, root, "real.jsonl", userStringContent)
	link := filepath.Join(root, "link.jsonl")
	if err := os.Symlink(realPath, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	got, err := Walk(context.Background(), WalkOptions{Root: root})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("expected 1 (real file only), got %d", len(got))
	}
}

func TestWalk_ContextCancel(t *testing.T) {
	root := t.TempDir()
	writeJSONL(t, root, "a.jsonl", userStringContent)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Walk(ctx, WalkOptions{Root: root})
	if err == nil {
		t.Error("expected context cancellation error")
	}
}

// ─── ReadTurns ───────────────────────────────────────────────────────

func TestReadTurns_UserStringAndAssistantBlocks(t *testing.T) {
	dir := t.TempDir()
	p := writeJSONL(t, dir, "S.jsonl", userStringContent, assistantBlocks)
	turns, err := ReadTurns(p, nil)
	if err != nil {
		t.Fatalf("ReadTurns: %v", err)
	}
	if len(turns) != 2 {
		t.Fatalf("expected 2 turns, got %d", len(turns))
	}
	if turns[0].Role != "user" || !strings.Contains(turns[0].Text, "buddy mcp") {
		t.Errorf("user turn wrong: %+v", turns[0])
	}
	if turns[1].Role != "assistant" {
		t.Errorf("assistant role missing: %+v", turns[1])
	}
	if !strings.Contains(turns[1].Text, "hmm, mcp") {
		t.Errorf("thinking block dropped: %q", turns[1].Text)
	}
	if !strings.Contains(turns[1].Text, "here is the status") {
		t.Errorf("text block dropped: %q", turns[1].Text)
	}
	if strings.Contains(turns[1].Text, "echo hi") {
		t.Errorf("tool_use leaked into text: %q", turns[1].Text)
	}
	if turns[0].TurnIndex != 0 || turns[1].TurnIndex != 1 {
		t.Errorf("turn indices wrong: %d / %d", turns[0].TurnIndex, turns[1].TurnIndex)
	}
	if turns[0].SessionID != "S" {
		t.Errorf("session id: %q", turns[0].SessionID)
	}
}

func TestReadTurns_SkipsNonIndexableTypes(t *testing.T) {
	dir := t.TempDir()
	p := writeJSONL(t, dir, "S.jsonl",
		systemMeta, attachment, lastPrompt, userStringContent)
	turns, err := ReadTurns(p, nil)
	if err != nil {
		t.Fatalf("ReadTurns: %v", err)
	}
	if len(turns) != 1 {
		t.Fatalf("expected only the user turn, got %d", len(turns))
	}
	if turns[0].TurnIndex != 0 {
		t.Errorf("TurnIndex should be 0 (non-indexable lines don't advance), got %d",
			turns[0].TurnIndex)
	}
}

func TestReadTurns_SkipsEmptyText(t *testing.T) {
	dir := t.TempDir()
	p := writeJSONL(t, dir, "S.jsonl", emptyText, noContent, userStringContent)
	turns, err := ReadTurns(p, nil)
	if err != nil {
		t.Fatalf("ReadTurns: %v", err)
	}
	if len(turns) != 1 {
		t.Errorf("expected 1 turn (empties skipped), got %d", len(turns))
	}
}

func TestReadTurns_MalformedLineSkipped(t *testing.T) {
	dir := t.TempDir()
	p := writeJSONL(t, dir, "S.jsonl",
		"not json at all",
		`{"unterminated":`,
		userStringContent)
	var errsSeen []int
	turns, err := ReadTurns(p, func(line int, _ error) {
		errsSeen = append(errsSeen, line)
	})
	if err != nil {
		t.Fatalf("ReadTurns: %v", err)
	}
	if len(turns) != 1 {
		t.Errorf("expected 1 surviving turn, got %d", len(turns))
	}
	if len(errsSeen) != 2 {
		t.Errorf("expected 2 errors reported, got %d", len(errsSeen))
	}
}

func TestReadTurns_NonexistentFile(t *testing.T) {
	_, err := ReadTurns(filepath.Join(t.TempDir(), "nope.jsonl"), nil)
	if err == nil {
		t.Error("expected open error")
	}
}

// ─── decodeMessage edge cases ───────────────────────────────────────

func TestDecodeMessage_EmptyRaw(t *testing.T) {
	_, _, ok := decodeMessage(nil)
	if ok {
		t.Error("expected ok=false for nil raw")
	}
}

func TestDecodeMessage_MalformedEnvelope(t *testing.T) {
	_, _, ok := decodeMessage([]byte(`not-json`))
	if ok {
		t.Error("expected ok=false for non-json envelope")
	}
}

func TestDecodeMessage_StringContent(t *testing.T) {
	role, text, ok := decodeMessage([]byte(`{"role":"user","content":"hello"}`))
	if !ok || role != "user" || text != "hello" {
		t.Errorf("got ok=%v role=%q text=%q", ok, role, text)
	}
}

func TestDecodeMessage_ArrayDropsToolUse(t *testing.T) {
	raw := []byte(`{"role":"assistant","content":[` +
		`{"type":"text","text":"A"},` +
		`{"type":"tool_use","name":"Bash","input":{}},` +
		`{"type":"thinking","thinking":"B"}` +
		`]}`)
	_, text, ok := decodeMessage(raw)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if !strings.Contains(text, "A") || !strings.Contains(text, "B") {
		t.Errorf("text blocks missing: %q", text)
	}
	if strings.Contains(text, "Bash") {
		t.Errorf("tool_use leaked: %q", text)
	}
}

func TestDecodeMessage_AllBlocksDropped(t *testing.T) {
	raw := []byte(`{"role":"assistant","content":[{"type":"tool_use","name":"x","input":{}}]}`)
	_, _, ok := decodeMessage(raw)
	if ok {
		t.Error("expected ok=false when every block is non-text")
	}
}

// ─── peekTurnMeta ────────────────────────────────────────────────────

func TestPeekTurnMeta_SkipsNonIndexable(t *testing.T) {
	if _, ok := peekTurnMeta([]byte(systemMeta)); ok {
		t.Error("system should not be indexable")
	}
}

func TestPeekTurnMeta_MalformedReturnsFalse(t *testing.T) {
	if _, ok := peekTurnMeta([]byte(`{garbage`)); ok {
		t.Error("malformed should yield ok=false")
	}
}

func TestPeekTurnMeta_ParsesTimestamp(t *testing.T) {
	ts, ok := peekTurnMeta([]byte(userStringContent))
	if !ok {
		t.Fatal("expected ok")
	}
	if ts.IsZero() {
		t.Errorf("expected parsed ts, got zero")
	}
}

// ─── OnError walk callback ──────────────────────────────────────────

func TestWalk_OnErrorCallbackFires(t *testing.T) {
	root := t.TempDir()
	// Put a file we can read, plus a directory we mark unreadable.
	writeJSONL(t, root, "ok.jsonl", userStringContent)
	denied := filepath.Join(root, "denied")
	if err := os.Mkdir(denied, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(denied, 0o755) })

	var captured int
	got, err := Walk(context.Background(), WalkOptions{
		Root:    root,
		OnError: func(_ string, _ error) { captured++ },
	})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("expected 1 readable summary, got %d", len(got))
	}
	// Don't assert captured > 0 — root-owned CI runners can still
	// traverse 0o000 dirs. The point is OnError got a chance and the
	// walk didn't abort.
	_ = captured
}
