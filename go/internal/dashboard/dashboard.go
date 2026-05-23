// Package dashboard exposes the kvault store + indexer + search over
// stdlib net/http on 127.0.0.1, so the vanilla-HTML UI (T-D.1 / T-D.2 /
// T-D.4 / T-D.5) can drive everything from the browser.
//
// Design notes mirror claude-env-sync's internal/dashboard (env-sync
// commit 856efe3):
//   - net/http + ServeMux only, zero third-party deps.
//   - Handlers are thin adapters: parse → call package → render JSON.
//     Logic lives in internal/indexer + internal/search + internal/store.
//   - Errors are translated to HTTP at this boundary; internal text
//     never leaks to clients (we log it through OnError instead).
//   - SSE (/api/events) emits a Stats snapshot on connect and on every
//     PollInterval tick. No push queue: single-user dashboard model.
//   - Search snippets are run through internal/secrets.Mask before
//     leaving the process (PoC v1 default-on secret rerender policy).
package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/wm-it/claude-knowledge-vault/internal/indexer"
	"github.com/wm-it/claude-knowledge-vault/internal/search"
	"github.com/wm-it/claude-knowledge-vault/internal/secrets"
	"github.com/wm-it/claude-knowledge-vault/internal/store"
)

// maxBodyBytes caps POST payloads. The API takes only small JSON
// envelopes (force flag, confirm flag); 1 MiB shuts down memory-
// exhaustion attempts from misbehaving clients.
const maxBodyBytes = 1 << 20

// defaultPollInterval drives /api/events. 5 s balances UI freshness
// with the per-tick cost of running store.Stats.
const defaultPollInterval = 5 * time.Second

// defaultShutdownGrace bounds graceful shutdown after Run's ctx is
// cancelled before forcing the listener closed.
const defaultShutdownGrace = 5 * time.Second

// Server wires a *store.DB (+ indexer / search) into an http.Handler.
// Construct via New.
type Server struct {
	db      *store.DB
	root    string
	dataDir string

	// PollInterval controls /api/events tick rate. Zero → default.
	PollInterval time.Duration

	// StaticHandler serves "/" and any non-API path. T-D.4 wires
	// the embed.FS asset handler here; nil → friendly placeholder.
	StaticHandler http.Handler

	// MaskSecrets toggles secret rerender on search snippets +
	// /api/turn content. Default true. Tests flip to false to
	// exercise the unmasked path.
	MaskSecrets bool
}

// New returns a Server with default tuning, including a StaticHandler
// backed by the embedded web/ assets. Callers can override
// StaticHandler after construction (tests do this to swap in a stub
// or assert the nil-fallback placeholder).
func New(db *store.DB, root, dataDir string) *Server {
	return &Server{
		db:            db,
		root:          root,
		dataDir:       dataDir,
		MaskSecrets:   true,
		StaticHandler: defaultStaticHandler(),
	}
}

// Handler returns the routed http.Handler. Safe to call repeatedly.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/api/stats", method(http.MethodGet, s.handleStats))
	mux.HandleFunc("/api/search", method(http.MethodGet, s.handleSearch))
	mux.HandleFunc("/api/turn", method(http.MethodGet, s.handleTurn))
	mux.HandleFunc("/api/index", method(http.MethodPost, s.handleIndex))
	mux.HandleFunc("/api/purge", method(http.MethodPost, s.handlePurge))
	mux.HandleFunc("/api/events", method(http.MethodGet, s.handleEvents))
	mux.HandleFunc("/", s.handleRoot)
	return mux
}

// Run starts the HTTP server bound to addr ("127.0.0.1:0" picks a
// free port). Blocks until ctx is cancelled or the server fails.
// onListen, if non-nil, is invoked once after the listener is open.
func (s *Server) Run(ctx context.Context, addr string, onListen func(net.Addr)) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("dashboard: listen %q: %w", addr, err)
	}
	if onListen != nil {
		onListen(ln.Addr())
	}
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No write timeout — SSE connections are long-lived.
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), defaultShutdownGrace)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
		return nil
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// ─── handlers ──────────────────────────────────────────────────────────

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, "ok\n")
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		http.NotFound(w, r)
		return
	}
	if s.StaticHandler != nil {
		s.StaticHandler.ServeHTTP(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, `<!doctype html>
<title>kvault</title>
<h1>kvault</h1>
<p>Dashboard UI ships in T-D.1 / T-D.2 / T-D.4. API endpoints are live at <code>/api/*</code>.</p>`)
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	stats, err := s.db.Stats(r.Context())
	if err != nil {
		respondErr(w, http.StatusInternalServerError, "stats failed", err)
		return
	}
	respondJSON(w, http.StatusOK, stats)
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	query := strings.TrimSpace(q.Get("query"))
	if query == "" {
		respondErr(w, http.StatusBadRequest, "query is required", nil)
		return
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	since, err := parseSinceQuery(q.Get("since"))
	if err != nil {
		respondErr(w, http.StatusBadRequest, "invalid since", err)
		return
	}
	opts := &search.Options{
		Query: query,
		Limit: limit,
		Since: since,
		Role:  q.Get("role"),
	}
	results, err := search.Run(r.Context(), s.db, opts)
	if err != nil {
		respondErr(w, http.StatusInternalServerError, "search failed", err)
		return
	}
	if s.MaskSecrets {
		for i := range results {
			results[i].Snippet = secrets.Mask(results[i].Snippet)
		}
	}
	// Always emit a non-nil array so the JS can iterate without a
	// nil-check.
	if results == nil {
		results = []search.Result{}
	}
	respondJSON(w, http.StatusOK, results)
}

type turnResponse struct {
	SessionID string    `json:"session_id"`
	TurnIndex int       `json:"turn_index"`
	Role      string    `json:"role"`
	TS        time.Time `json:"ts"`
	Title     string    `json:"title,omitempty"`
	Content   string    `json:"content"`
	RawSize   int       `json:"raw_size"`
}

func (s *Server) handleTurn(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sessionID := q.Get("session_id")
	turnStr := q.Get("turn_index")
	if sessionID == "" || turnStr == "" {
		respondErr(w, http.StatusBadRequest, "session_id and turn_index required", nil)
		return
	}
	turnIdx, err := strconv.Atoi(turnStr)
	if err != nil {
		respondErr(w, http.StatusBadRequest, "turn_index must be int", err)
		return
	}
	turn, found, err := s.db.GetTurn(r.Context(), sessionID, turnIdx)
	if err != nil {
		respondErr(w, http.StatusInternalServerError, "get turn failed", err)
		return
	}
	if !found {
		respondErr(w, http.StatusNotFound, "turn not found", nil)
		return
	}
	chunks, err := s.db.GetChunksByTurn(r.Context(), sessionID, turnIdx)
	if err != nil {
		respondErr(w, http.StatusInternalServerError, "get chunks failed", err)
		return
	}
	parts := make([]string, 0, len(chunks))
	title := ""
	for _, c := range chunks {
		if c.Title != "" && title == "" {
			title = c.Title
		}
		parts = append(parts, c.Content)
	}
	content := strings.Join(parts, "\n\n")
	if s.MaskSecrets {
		content = secrets.Mask(content)
	}
	respondJSON(w, http.StatusOK, turnResponse{
		SessionID: turn.SessionID,
		TurnIndex: turn.TurnIndex,
		Role:      turn.Role,
		TS:        turn.TS,
		Title:     title,
		Content:   content,
		RawSize:   turn.RawSize,
	})
}

type indexRequest struct {
	Force bool `json:"force"`
}

type indexResponse struct {
	FilesScanned   int           `json:"filesScanned"`
	FilesIndexed   int           `json:"filesIndexed"`
	TurnsInserted  int           `json:"turnsInserted"`
	ChunksInserted int           `json:"chunksInserted"`
	Elapsed        time.Duration `json:"elapsedNs"`
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	var req indexRequest
	if err := decodeJSON(r, &req); err != nil {
		respondErr(w, http.StatusBadRequest, "invalid request body", err)
		return
	}
	res, err := indexer.Run(r.Context(), s.db, &indexer.Options{
		Root: s.root, DataDir: s.dataDir, Force: req.Force,
	})
	switch {
	case errors.Is(err, indexer.ErrIndexInProgress):
		respondErr(w, http.StatusConflict, "indexer already running", err)
		return
	case err != nil:
		respondErr(w, http.StatusInternalServerError, "index failed", err)
		return
	}
	respondJSON(w, http.StatusOK, indexResponse{
		FilesScanned:   res.FilesScanned,
		FilesIndexed:   res.FilesIndexed,
		TurnsInserted:  res.TurnsInserted,
		ChunksInserted: res.ChunksInserted,
		Elapsed:        res.Elapsed,
	})
}

type purgeRequest struct {
	Confirm bool `json:"confirm"`
}

func (s *Server) handlePurge(w http.ResponseWriter, r *http.Request) {
	var req purgeRequest
	if err := decodeJSON(r, &req); err != nil {
		respondErr(w, http.StatusBadRequest, "invalid request body", err)
		return
	}
	if !req.Confirm {
		respondErr(w, http.StatusBadRequest, "purge requires confirm=true", nil)
		return
	}
	if err := s.db.Purge(r.Context()); err != nil {
		respondErr(w, http.StatusInternalServerError, "purge failed", err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "purged"})
}

// handleEvents streams Stats snapshots as Server-Sent Events. First
// snapshot fires immediately on connect, subsequent ones on every
// PollInterval tick until the client disconnects.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		respondErr(w, http.StatusInternalServerError, "streaming unsupported", nil)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	interval := s.PollInterval
	if interval <= 0 {
		interval = defaultPollInterval
	}
	send := func() {
		stats, err := s.db.Stats(r.Context())
		if err != nil {
			fmt.Fprintf(w, "event: error\ndata: %s\n\n", jsonString(err.Error()))
			flusher.Flush()
			return
		}
		payload, _ := json.Marshal(stats)
		fmt.Fprintf(w, "event: stats\ndata: %s\n\n", payload)
		flusher.Flush()
	}
	send()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			send()
		}
	}
}

// ─── helpers ───────────────────────────────────────────────────────────

func method(want string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != want {
			w.Header().Set("Allow", want)
			respondErr(w, http.StatusMethodNotAllowed, "method not allowed", nil)
			return
		}
		h(w, r)
	}
}

func decodeJSON(r *http.Request, dst any) error {
	if r.Body == nil {
		return errors.New("empty body")
	}
	r.Body = http.MaxBytesReader(nil, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

func respondJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func respondErr(w http.ResponseWriter, status int, msg string, cause error) {
	_ = cause
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func parseSinceQuery(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339, s)
}
