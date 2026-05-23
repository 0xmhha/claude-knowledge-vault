package dashboard

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wm-it/claude-knowledge-vault/internal/store"
)

// ─── fixtures ─────────────────────────────────────────────────────────

func newServer(t *testing.T) (*Server, *store.DB) {
	t.Helper()
	tmp := t.TempDir()
	db, err := store.Open(context.Background(), filepath.Join(tmp, "vault.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	root := filepath.Join(tmp, "projects")
	_ = os.MkdirAll(root, 0o755)
	return New(db, root, filepath.Join(tmp, "data")), db
}

func seedTurn(t *testing.T, db *store.DB, sessionID string, idx int, role, title, content string) {
	t.Helper()
	ctx := context.Background()
	ts := time.Date(2026, 5, 1, 12, idx, 0, 0, time.UTC)
	s := store.Session{
		ID: sessionID, ProjectPath: "/p",
		FilePath: "/p/" + sessionID + ".jsonl",
		FirstTS:  ts, LastTS: ts, ContentHash: "h", LastMtime: ts, TurnCount: 1,
	}
	if err := db.UpsertSession(ctx, &s); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertTurn(ctx, store.Turn{
		SessionID: sessionID, TurnIndex: idx, Role: role, TS: ts, RawSize: len(content),
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.InsertChunk(ctx, &store.Chunk{
		SessionID: sessionID, TurnIndex: idx, Role: role, TS: ts,
		Title: title, Content: content,
	}); err != nil {
		t.Fatal(err)
	}
}

func doReq(t *testing.T, h http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// ─── routing / health ─────────────────────────────────────────────────

func TestHealth(t *testing.T) {
	s, _ := newServer(t)
	rec := doReq(t, s.Handler(), http.MethodGet, "/healthz", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "ok") {
		t.Errorf("healthz: %d %q", rec.Code, rec.Body.String())
	}
}

func TestRoot_PlaceholderWhenNoStatic(t *testing.T) {
	// Explicit nil StaticHandler — bypasses the embed default to
	// assert the in-package placeholder is still served when callers
	// opt out (zero-value &Server{} construction).
	s, _ := newServer(t)
	s.StaticHandler = nil
	rec := doReq(t, s.Handler(), http.MethodGet, "/", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Dashboard UI ships") {
		t.Errorf("placeholder body wrong: %q", rec.Body.String())
	}
}

// ─── embed.FS-backed static assets (T-D.4) ──────────────────────────

func TestEmbed_ServesIndex(t *testing.T) {
	s, _ := newServer(t)
	rec := doReq(t, s.Handler(), http.MethodGet, "/", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// Must be the real embedded index.html, not the placeholder.
	if !strings.Contains(body, `id="search-form"`) {
		t.Errorf("expected embedded index.html, got first 200B: %q",
			body[:min(200, len(body))])
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("wrong content-type for /: %q", ct)
	}
}

func TestEmbed_ServesStyleCSS(t *testing.T) {
	s, _ := newServer(t)
	rec := doReq(t, s.Handler(), http.MethodGet, "/style.css", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "monospace") {
		t.Errorf("expected style.css body, got first 80B: %q",
			rec.Body.String()[:min(80, rec.Body.Len())])
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/css") {
		t.Errorf("expected text/css, got %q", ct)
	}
}

func TestEmbed_ServesAppJS(t *testing.T) {
	s, _ := newServer(t)
	rec := doReq(t, s.Handler(), http.MethodGet, "/app.js", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "EventSource") {
		t.Errorf("expected app.js with SSE wiring, got: %q",
			rec.Body.String()[:min(80, rec.Body.Len())])
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("expected javascript content-type, got %q", ct)
	}
}

func TestRoot_StaticHandlerOverride(t *testing.T) {
	s, _ := newServer(t)
	s.StaticHandler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "static-stub")
	})
	rec := doReq(t, s.Handler(), http.MethodGet, "/anything", nil)
	if rec.Body.String() != "static-stub" {
		t.Errorf("static handler not wired: %q", rec.Body.String())
	}
}

func TestRoot_404OnUnknownAPI(t *testing.T) {
	s, _ := newServer(t)
	rec := doReq(t, s.Handler(), http.MethodGet, "/api/nope", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rec.Code)
	}
}

func TestMethodGate(t *testing.T) {
	s, _ := newServer(t)
	rec := doReq(t, s.Handler(), http.MethodGet, "/api/index", nil)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", rec.Code)
	}
	if rec.Header().Get("Allow") != http.MethodPost {
		t.Errorf("missing Allow header: %q", rec.Header().Get("Allow"))
	}
}

// ─── /api/stats ───────────────────────────────────────────────────────

func TestStats_EmptyDB(t *testing.T) {
	s, _ := newServer(t)
	rec := doReq(t, s.Handler(), http.MethodGet, "/api/stats", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var st store.Stats
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if st != (store.Stats{}) {
		t.Errorf("expected zero stats, got %+v", st)
	}
}

// ─── /api/search ──────────────────────────────────────────────────────

func TestSearch_MissingQuery(t *testing.T) {
	s, _ := newServer(t)
	rec := doReq(t, s.Handler(), http.MethodGet, "/api/search", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rec.Code)
	}
}

func TestSearch_BadSince(t *testing.T) {
	s, _ := newServer(t)
	rec := doReq(t, s.Handler(), http.MethodGet,
		"/api/search?query=x&since=30d", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for non-RFC3339 since, got %d", rec.Code)
	}
}

func TestSearch_EmptyDBReturnsEmptyArray(t *testing.T) {
	s, _ := newServer(t)
	rec := doReq(t, s.Handler(), http.MethodGet,
		"/api/search?query=anything", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	if strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Errorf("expected [] for empty DB, got %q", rec.Body.String())
	}
}

func TestSearch_HitMasksSecrets(t *testing.T) {
	s, db := newServer(t)
	leak := "sk-ant-AAAABBBBCCCCDDDDEEEEFFFFGGGGHHHH1234567890ZZ"
	seedTurn(t, db, "s1", 0, "user", "Decisions",
		"we kept the token "+leak+" thanks for noting")

	rec := doReq(t, s.Handler(), http.MethodGet,
		"/api/search?query=token+thanks", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, leak) {
		t.Errorf("secret leaked into search response: %s", body)
	}
	if !strings.Contains(body, "REDACTED") {
		t.Errorf("expected REDACTED marker: %s", body)
	}
}

func TestSearch_NoMaskWhenDisabled(t *testing.T) {
	s, db := newServer(t)
	s.MaskSecrets = false
	leak := "sk-ant-AAAABBBBCCCCDDDDEEEEFFFFGGGGHHHH1234567890"
	seedTurn(t, db, "s1", 0, "user", "k", "token "+leak+" present")
	rec := doReq(t, s.Handler(), http.MethodGet, "/api/search?query=token+present", nil)
	if !strings.Contains(rec.Body.String(), leak) {
		t.Errorf("masking should be off; body=%s", rec.Body.String())
	}
}

// ─── /api/turn ────────────────────────────────────────────────────────

func TestTurn_HappyPath(t *testing.T) {
	s, db := newServer(t)
	seedTurn(t, db, "s1", 0, "user", "Choice", "the body of the turn")
	rec := doReq(t, s.Handler(), http.MethodGet,
		"/api/turn?session_id=s1&turn_index=0", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp turnResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.SessionID != "s1" || resp.TurnIndex != 0 || resp.Role != "user" {
		t.Errorf("metadata wrong: %+v", resp)
	}
	if resp.Title != "Choice" || !strings.Contains(resp.Content, "body of the turn") {
		t.Errorf("payload wrong: %+v", resp)
	}
}

func TestTurn_NotFound(t *testing.T) {
	s, _ := newServer(t)
	rec := doReq(t, s.Handler(), http.MethodGet,
		"/api/turn?session_id=missing&turn_index=0", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestTurn_MissingParams(t *testing.T) {
	s, _ := newServer(t)
	rec := doReq(t, s.Handler(), http.MethodGet, "/api/turn", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rec.Code)
	}
}

func TestTurn_BadIndex(t *testing.T) {
	s, _ := newServer(t)
	rec := doReq(t, s.Handler(), http.MethodGet,
		"/api/turn?session_id=s1&turn_index=not-an-int", nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rec.Code)
	}
}

// ─── /api/index ───────────────────────────────────────────────────────

func TestIndex_EmptyRoot(t *testing.T) {
	s, _ := newServer(t)
	rec := doReq(t, s.Handler(), http.MethodPost, "/api/index", map[string]bool{})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp indexResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.FilesScanned != 0 {
		t.Errorf("expected 0 scanned in empty projects dir, got %d", resp.FilesScanned)
	}
}

func TestIndex_RejectsUnknownField(t *testing.T) {
	s, _ := newServer(t)
	rec := doReq(t, s.Handler(), http.MethodPost, "/api/index",
		map[string]any{"force": true, "what": "ever"})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for unknown field, got %d", rec.Code)
	}
}

func TestIndex_LockConflict(t *testing.T) {
	s, _ := newServer(t)
	// Pre-create a fresh lock file to force ErrIndexInProgress.
	_ = os.MkdirAll(s.dataDir, 0o700)
	lockPath := filepath.Join(s.dataDir, "index.lock")
	if err := os.WriteFile(lockPath, []byte(time.Now().Format(time.RFC3339Nano)), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := doReq(t, s.Handler(), http.MethodPost, "/api/index", map[string]bool{})
	if rec.Code != http.StatusConflict {
		t.Errorf("expected 409 when lock present, got %d", rec.Code)
	}
}

// ─── /api/purge ───────────────────────────────────────────────────────

func TestPurge_RequiresConfirm(t *testing.T) {
	s, _ := newServer(t)
	rec := doReq(t, s.Handler(), http.MethodPost, "/api/purge", map[string]bool{})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 without confirm, got %d", rec.Code)
	}
}

func TestPurge_WithConfirm(t *testing.T) {
	s, db := newServer(t)
	seedTurn(t, db, "s1", 0, "user", "T", "to be purged")
	rec := doReq(t, s.Handler(), http.MethodPost, "/api/purge",
		map[string]bool{"confirm": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	stats, _ := db.Stats(context.Background())
	if stats != (store.Stats{}) {
		t.Errorf("purge incomplete: %+v", stats)
	}
}

// ─── SSE /api/events ──────────────────────────────────────────────────

func TestEvents_StreamsInitialStats(t *testing.T) {
	s, _ := newServer(t)
	s.PollInterval = 50 * time.Millisecond
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		srv.URL+"/api/events", http.NoBody)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get events: %v", err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Errorf("wrong content-type: %q", got)
	}
	br := bufio.NewReader(resp.Body)
	deadline := time.Now().Add(1500 * time.Millisecond)
	gotEvent, gotData := false, false
	for time.Now().Before(deadline) {
		line, err := br.ReadString('\n')
		if err != nil {
			break
		}
		line = strings.TrimRight(line, "\n")
		if strings.HasPrefix(line, "event: stats") {
			gotEvent = true
		}
		if gotEvent && strings.HasPrefix(line, "data:") {
			gotData = true
			break
		}
	}
	if !gotEvent || !gotData {
		t.Errorf("missing event/data frame: event=%v data=%v", gotEvent, gotData)
	}
}

func TestEvents_ClosesOnClientDisconnect(t *testing.T) {
	s, _ := newServer(t)
	s.PollInterval = 50 * time.Millisecond
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		srv.URL+"/api/events", http.NoBody)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get events: %v", err)
	}
	cancel()
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Errorf("SSE did not close after client disconnect")
	}
}

// ─── Run lifecycle ────────────────────────────────────────────────────

func TestRun_StartsAndShutsDown(t *testing.T) {
	s, _ := newServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	addrCh := make(chan net.Addr, 1)
	errc := make(chan error, 1)
	go func() {
		errc <- s.Run(ctx, "127.0.0.1:0", func(a net.Addr) { addrCh <- a })
	}()
	var addr net.Addr
	select {
	case addr = <-addrCh:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("Run never reported listen address")
	}
	resp, err := http.Get("http://" + addr.String() + "/healthz")
	if err != nil {
		cancel()
		t.Fatalf("healthz get: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("healthz status=%d", resp.StatusCode)
	}
	cancel()
	select {
	case err := <-errc:
		if err != nil {
			t.Errorf("Run err: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Errorf("Run did not return after ctx cancel")
	}
}

func TestRun_FailsOnBadAddr(t *testing.T) {
	s, _ := newServer(t)
	err := s.Run(context.Background(), "not-an-addr", nil)
	if err == nil {
		t.Errorf("expected listen error for bad addr")
	}
}
