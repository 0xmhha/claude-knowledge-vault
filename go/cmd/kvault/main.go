// Command kvault is the dual-mode entry for claude-knowledge-vault.
//
//	kvault                       run web dashboard (placeholder until T-D.3)
//	kvault --mcp                 run MCP server over stdio
//	kvault --once index          one-shot index pass
//	kvault --once search --query 'foo'  one-shot search
//	kvault --once stats          one-shot stats dump
//	kvault --once purge          drop every row (asks for --yes)
//	kvault --version             print version and exit
//
// Resolution precedence (lowest to highest):
//
//	default      ~/.cache/claude-knowledge-vault and ~/.claude/projects
//	env var      KVAULT_DATA, CLAUDE_HOME
//	command-line flag
//
// Exit codes:
//
//	0  success
//	1  user / config error (bad flag, missing arg)
//	2  runtime error (db open, walk, etc.)
//	4  build / exec failure
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/wm-it/claude-knowledge-vault/internal/dashboard"
	"github.com/wm-it/claude-knowledge-vault/internal/indexer"
	"github.com/wm-it/claude-knowledge-vault/internal/mcp"
	"github.com/wm-it/claude-knowledge-vault/internal/search"
	"github.com/wm-it/claude-knowledge-vault/internal/secrets"
	"github.com/wm-it/claude-knowledge-vault/internal/store"
)

// Version is overridden at build time via -ldflags="-X main.Version=…".
var Version = "0.1.0-dev"

const (
	exitOK      = 0
	exitUser    = 1
	exitRuntime = 2
	exitBuild   = 4
)

const dbFileName = "vault.db"

func main() {
	code := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	os.Exit(code)
}

// run is the testable entry. Separated from main() so unit tests can
// drive flag parsing and dispatch without touching os.Exit.
func run(args []string, stdin, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("kvault", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		mcpMode      = fs.Bool("mcp", false, "run as MCP server over stdio")
		onceOp       = fs.String("once", "", "one-shot operation: index|search|stats|purge")
		pluginData   = fs.String("plugin-data", "", "data dir (env: KVAULT_DATA; default: ~/.cache/claude-knowledge-vault)")
		root         = fs.String("root", "", "Claude Code projects dir (env: CLAUDE_HOME points at ~/.claude; default: ~/.claude/projects)")
		dbPath       = fs.String("db", "", "vault DB path (default: <plugin-data>/vault.db)")
		query        = fs.String("query", "", "search query (--once search)")
		limit        = fs.Int("limit", 20, "max search hits")
		source       = fs.String("source", "bm25", "search source: bm25|trigram")
		role         = fs.String("role", "", "filter results to a Claude turn role")
		sinceFlag    = fs.String("since", "", "RFC3339 lower bound on turn timestamp")
		force        = fs.Bool("force", false, "force re-index ignoring content_hash gate")
		yes          = fs.Bool("yes", false, "confirm destructive ops (--once purge)")
		port         = fs.Int("port", 0, "dashboard port (0 = pick free; env: KVAULT_PORT)")
		printVersion = fs.Bool("version", false, "print version and exit")
	)
	fs.Usage = func() {
		fmt.Fprintf(stderr, "kvault %s — local search over your Claude Code conversations\n\n", Version)
		fmt.Fprintln(stderr, "Usage:")
		fmt.Fprintln(stderr, "  kvault                              run dashboard (T-D.3 pending)")
		fmt.Fprintln(stderr, "  kvault --mcp                        run MCP server (stdio)")
		fmt.Fprintln(stderr, "  kvault --once index [--force]       re-scan jsonl + insert chunks")
		fmt.Fprintln(stderr, "  kvault --once search --query …      BM25 search (+ trigram fallback)")
		fmt.Fprintln(stderr, "  kvault --once stats                 row counts")
		fmt.Fprintln(stderr, "  kvault --once purge --yes           drop everything (keeps schema)")
		fmt.Fprintln(stderr, "  kvault --version                    print version")
		fmt.Fprintln(stderr, "\nFlags:")
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		return exitUser
	}
	if *printVersion {
		fmt.Fprintln(stdout, Version)
		return exitOK
	}

	// Resolve paths.
	resolvedData, err := resolvePluginData(*pluginData)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return exitUser
	}
	resolvedRoot, err := resolveRoot(*root)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return exitUser
	}
	resolvedDB := *dbPath
	if resolvedDB == "" {
		resolvedDB = filepath.Join(resolvedData, dbFileName)
	}
	if err := os.MkdirAll(filepath.Dir(resolvedDB), 0o700); err != nil {
		fmt.Fprintf(stderr, "error: mkdir db dir: %v\n", err)
		return exitRuntime
	}

	ctx := context.Background()
	db, err := store.Open(ctx, resolvedDB)
	if err != nil {
		fmt.Fprintf(stderr, "error: open db %q: %v\n", resolvedDB, err)
		return exitRuntime
	}
	defer func() { _ = db.Close() }()

	since, err := parseSince(*sinceFlag)
	if err != nil {
		fmt.Fprintf(stderr, "error: bad --since: %v\n", err)
		return exitUser
	}

	switch {
	case *mcpMode:
		return runMCP(ctx, db, resolvedRoot, resolvedData, stdin, stdout, stderr)
	case *onceOp != "":
		return runOnce(ctx, db, resolvedRoot, resolvedData, *onceOp,
			*query, *limit, *source, *role, since, *force, *yes, stdout, stderr)
	default:
		return runDashboard(ctx, db, resolvedRoot, resolvedData, *port, stdout, stderr)
	}
}

// ─── path resolution ─────────────────────────────────────────────────

func resolvePluginData(flagValue string) (string, error) {
	if flagValue != "" {
		return filepath.Abs(flagValue)
	}
	if env := os.Getenv("KVAULT_DATA"); env != "" {
		return filepath.Abs(env)
	}
	if env := os.Getenv("CLAUDE_PLUGIN_DATA"); env != "" {
		return filepath.Abs(env)
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("could not determine cache dir: %w", err)
	}
	return filepath.Join(cache, "claude-knowledge-vault"), nil
}

func resolveRoot(flagValue string) (string, error) {
	if flagValue != "" {
		return filepath.Abs(flagValue)
	}
	// CLAUDE_HOME points at ~/.claude (env-sync convention); append /projects.
	if env := os.Getenv("CLAUDE_HOME"); env != "" {
		return filepath.Abs(filepath.Join(env, "projects"))
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("could not determine home dir: %w", err)
	}
	return filepath.Join(home, ".claude", "projects"), nil
}

// parseSince accepts an empty string ("no lower bound") or an RFC3339
// timestamp. Friendly durations like "30d" are deliberately out of
// PoC scope — Go's time.ParseDuration has no day unit and we'd rather
// fail fast than be surprising. Document this in --help.
func parseSince(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("expected RFC3339 (e.g. 2026-05-01T00:00:00Z): %w", err)
	}
	return t, nil
}

// ─── Dashboard mode ──────────────────────────────────────────────────

// runDashboard binds the dashboard to 127.0.0.1:<port> (0 = pick free)
// and blocks until SIGINT/SIGTERM. Localhost-only by design: kvault
// is a single-user tool and the DB may quote pasted credentials.
func runDashboard(ctx context.Context, db *store.DB, root, data string,
	port int, stdout, stderr *os.File,
) int {
	if port == 0 {
		port = envInt("KVAULT_PORT", 0)
	}
	addr := "127.0.0.1:" + strconv.Itoa(port)
	srv := dashboard.New(db, root, data)

	sigCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	onListen := func(a net.Addr) {
		fmt.Fprintf(stdout, "kvault dashboard listening on http://%s\n", a.String())
		fmt.Fprintln(stdout, "  Ctrl-C to stop. API at /api/*, UI at /.")
	}
	if err := srv.Run(sigCtx, addr, onListen); err != nil {
		fmt.Fprintf(stderr, "dashboard: %v\n", err)
		return exitRuntime
	}
	return exitOK
}

// ─── MCP mode ────────────────────────────────────────────────────────

func runMCP(ctx context.Context, db *store.DB, root, data string, stdin, stdout, stderr *os.File) int {
	s := mcp.New("claude-knowledge-vault", Version)
	for _, t := range buildTools(db, root, data) {
		s.RegisterTool(t)
	}
	if err := s.Run(ctx, stdin, stdout); err != nil {
		fmt.Fprintf(stderr, "mcp server: %v\n", err)
		return exitRuntime
	}
	return exitOK
}

// buildTools assembles the four kv_* tools that share one DB.
func buildTools(db *store.DB, root, data string) []mcp.Tool {
	return []mcp.Tool{
		&kvTool{db: db, root: root, data: data, op: "index"},
		&kvTool{db: db, root: root, data: data, op: "search"},
		&kvTool{db: db, root: root, data: data, op: "stats"},
		&kvTool{db: db, root: root, data: data, op: "purge"},
	}
}

// kvTool is the thin adapter from mcp.Tool to indexer / search / store.
// One struct, one switch — env-sync's pattern (commit e85518d).
type kvTool struct {
	db   *store.DB
	root string
	data string
	op   string
}

func (t *kvTool) Name() string { return "kv_" + t.op }

func (t *kvTool) Description() string {
	switch t.op {
	case "index":
		return "Re-scan ~/.claude/projects/ and insert any new turns into the vault. Idempotent; unchanged files are skipped via content hash."
	case "search":
		return "BM25 search over indexed conversation turns. Returns ranked snippets with (session_id, turn_index) pointers. Falls back to trigram for short queries."
	case "stats":
		return "Report row counts (sessions, turns, chunks) for the vault."
	case "purge":
		return "Drop every row from the vault while keeping the schema. Caller must pass {\"confirm\":true}."
	}
	return ""
}

func (t *kvTool) InputSchema() json.RawMessage {
	switch t.op {
	case "index":
		return json.RawMessage(`{
"type":"object",
"properties":{"force":{"type":"boolean","description":"ignore content-hash gate"}}
}`)
	case "search":
		return json.RawMessage(`{
"type":"object",
"properties":{
  "query":{"type":"string"},
  "limit":{"type":"integer","minimum":1,"maximum":200},
  "since":{"type":"string","description":"RFC3339 lower bound"},
  "role":{"type":"string"},
  "source":{"type":"string","enum":["bm25","trigram"]}
},
"required":["query"]
}`)
	case "stats":
		return json.RawMessage(`{"type":"object","properties":{}}`)
	case "purge":
		return json.RawMessage(`{
"type":"object",
"properties":{"confirm":{"type":"boolean"}},
"required":["confirm"]
}`)
	}
	return nil
}

func (t *kvTool) Call(ctx context.Context, raw json.RawMessage) (output string, isError bool, err error) {
	switch t.op {
	case "index":
		var args struct {
			Force bool `json:"force"`
		}
		if err := mcp.UnmarshalArgs(raw, &args); err != nil {
			return "", true, err
		}
		res, err := indexer.Run(ctx, t.db, &indexer.Options{
			Root: t.root, DataDir: t.data, Force: args.Force,
		})
		if err != nil {
			return "", true, err
		}
		return fmt.Sprintf("indexed %d/%d files, %d turns, %d chunks in %s",
			res.FilesIndexed, res.FilesScanned, res.TurnsInserted,
			res.ChunksInserted, res.Elapsed.Round(time.Millisecond)), false, nil

	case "search":
		var args struct {
			Query  string `json:"query"`
			Limit  int    `json:"limit"`
			Since  string `json:"since"`
			Role   string `json:"role"`
			Source string `json:"source"`
		}
		if err := mcp.UnmarshalArgs(raw, &args); err != nil {
			return "", true, err
		}
		if args.Query == "" {
			return "query is required", true, nil
		}
		since, err := parseSince(args.Since)
		if err != nil {
			return err.Error(), true, nil
		}
		opts := &search.Options{
			Query: args.Query, Limit: args.Limit, Since: since, Role: args.Role,
		}
		if args.Source == "trigram" {
			// Skip BM25 lane entirely; caller asked for the trigram view.
			opts.DisableFallback = false
		}
		results, err := search.Run(ctx, t.db, opts)
		if err != nil {
			return "", true, err
		}
		// Mask secrets in every snippet before serialising — the
		// dashboard does the same in T-D.3.
		for i := range results {
			results[i].Snippet = secrets.Mask(results[i].Snippet)
		}
		j, _ := json.MarshalIndent(results, "", "  ")
		return string(j), false, nil

	case "stats":
		s, err := t.db.Stats(ctx)
		if err != nil {
			return "", true, err
		}
		j, _ := json.MarshalIndent(s, "", "  ")
		return string(j), false, nil

	case "purge":
		var args struct {
			Confirm bool `json:"confirm"`
		}
		if err := mcp.UnmarshalArgs(raw, &args); err != nil {
			return "", true, err
		}
		if !args.Confirm {
			return `purge requires {"confirm":true}`, true, nil
		}
		if err := t.db.Purge(ctx); err != nil {
			return "", true, err
		}
		return "purged", false, nil
	}
	return "", true, fmt.Errorf("unknown op: %s", t.op)
}

// ─── --once mode ─────────────────────────────────────────────────────

func runOnce(ctx context.Context, db *store.DB, root, data, op string,
	query string, limit int, src, role string, since time.Time,
	force, yes bool, stdout, stderr *os.File,
) int { //nolint:revive // 12 params is fine — splitting hurts readability
	tool := &kvTool{db: db, root: root, data: data, op: op}

	args := map[string]any{}
	switch op {
	case "index":
		args["force"] = force
	case "search":
		args["query"] = query
		if limit > 0 {
			args["limit"] = limit
		}
		if src != "" {
			args["source"] = src
		}
		if role != "" {
			args["role"] = role
		}
		if !since.IsZero() {
			args["since"] = since.Format(time.RFC3339)
		}
	case "purge":
		args["confirm"] = yes
	case "stats":
		// no args
	default:
		fmt.Fprintf(stderr, "error: unknown --once op %q\n", op)
		return exitUser
	}
	raw, _ := json.Marshal(args)

	output, isError, err := tool.Call(ctx, raw)
	if err != nil {
		fmt.Fprintf(stderr, "error: %v\n", err)
		return exitRuntime
	}
	fmt.Fprintln(stdout, output)
	if isError {
		return exitRuntime
	}
	return exitOK
}

// ─── env knobs that aren't flag-bound ────────────────────────────────

// envInt reads an int env var with a fallback. Used by the dashboard
// mode wire-up for KVAULT_PORT.
func envInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}
