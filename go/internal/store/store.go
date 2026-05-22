// Package store wraps modernc.org/sqlite (pure-Go) for the kvault
// knowledge base.
//
// Schema (PRAGMA user_version = current):
//
//	sessions          one row per Claude session jsonl file
//	turns             one row per turn (session_id, turn_index)
//	chunks            fts5(title, content, ...) tokenize='porter unicode61'
//	chunks_trigram    fts5(content, ...)        tokenize='trigram'
//
// The store layer is mechanical: open / migrate / CRUD / a thin raw
// MATCH wrapper. Ranking, snippet windowing, BM25 boosting, trigram
// fallback policy all live in internal/search (T-C.5), which keeps
// this package replaceable behind its Go-level interface.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers "sqlite" driver
)

// currentSchemaVersion is the highest PRAGMA user_version this binary
// knows how to produce. A DB opened with a lower version is migrated
// up step by step; a DB with a *higher* version is refused (the user
// has a newer kvault that we shouldn't downgrade).
const currentSchemaVersion = 1

// driverName is what modernc.org/sqlite registers itself as. Pinning
// here keeps the rest of the package framework-agnostic.
const driverName = "sqlite"

// DB is the kvault store handle. Safe for concurrent use — SQLite WAL
// mode allows many readers + one writer, and database/sql serialises
// writes through its connection pool.
type DB struct {
	sql  *sql.DB
	path string
}

// Session is one Claude Code conversation file's metadata row.
type Session struct {
	ID          string    // session_id from jsonl (also the file stem)
	ProjectPath string    // ~/.claude/projects/<dir>
	FilePath    string    // absolute path to jsonl
	FirstTS     time.Time // first turn timestamp
	LastTS      time.Time // last turn timestamp
	ContentHash string    // sha256 of jsonl file (mtime-gating fallback)
	LastMtime   time.Time // file mtime at last index
	TurnCount   int
}

// Turn is one conversation message.
type Turn struct {
	SessionID string
	TurnIndex int
	Role      string // "user" | "assistant" | "tool_result" | other
	TS        time.Time
	RawSize   int // length of the original text in bytes
}

// Chunk is one BM25-indexable unit. Title is short navigational text
// (heading or role+timestamp); Content is the chunk body.
type Chunk struct {
	SessionID string
	TurnIndex int
	Role      string
	TS        time.Time
	Title     string
	Content   string
}

// Stats is a cheap summary for the dashboard / kv_stats MCP tool.
type Stats struct {
	Sessions int
	Turns    int
	Chunks   int
}

// SearchResult is the raw row returned by Search; ranking and snippet
// extraction happen in internal/search.
type SearchResult struct {
	SessionID string
	TurnIndex int
	Role      string
	TS        time.Time
	Title     string
	Content   string
	// MatchRank is the raw fts5 rank value (lower = better). Search
	// layer converts to a positive BM25 score for the API.
	MatchRank float64
}

// SearchOpts limits and filters Search.
type SearchOpts struct {
	Limit  int       // ≤0 → 50
	Since  time.Time // zero → no lower bound
	Role   string    // empty → any
	Source string    // "bm25" (default) | "trigram"
}

// ─── lifecycle ────────────────────────────────────────────────────────

// Open opens (or creates) a vault DB at path. Applies the WAL +
// foreign-keys pragmas and runs Migrate up to currentSchemaVersion
// before returning.
func Open(ctx context.Context, path string) (*DB, error) {
	if path == "" {
		return nil, errors.New("store: path is required")
	}
	// `file:` URI with pragma query params is the modernc.org/sqlite
	// idiom for setting pragmas at connection time — applied to every
	// connection in the pool, not just the first.
	dsn := "file:" + path +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=foreign_keys(1)" +
		"&_pragma=busy_timeout(5000)"
	sqlDB, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, fmt.Errorf("store: sql.Open: %w", err)
	}
	// Cap to avoid the "many writers" anti-pattern; WAL still gives
	// readers concurrency.
	sqlDB.SetMaxOpenConns(4)
	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	db := &DB{sql: sqlDB, path: path}
	if err := db.Migrate(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}
	return db, nil
}

// Close releases the underlying connection pool. Idempotent.
func (db *DB) Close() error {
	if db == nil || db.sql == nil {
		return nil
	}
	return db.sql.Close()
}

// Path returns the on-disk path the DB was opened with.
func (db *DB) Path() string { return db.path }

// ─── migration ────────────────────────────────────────────────────────

// Migrate brings the DB up to currentSchemaVersion. Idempotent; safe
// to call from Open. Refuses to run if user_version > current
// (caller is on an older binary than the DB).
func (db *DB) Migrate(ctx context.Context) error {
	var v int
	if err := db.sql.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&v); err != nil {
		return fmt.Errorf("store: read user_version: %w", err)
	}
	if v > currentSchemaVersion {
		return fmt.Errorf("store: DB user_version=%d > supported=%d "+
			"(this kvault is older than the DB)", v, currentSchemaVersion)
	}
	for v < currentSchemaVersion {
		next := v + 1
		stmts, ok := migrations[next]
		if !ok {
			return fmt.Errorf("store: missing migration to v%d", next)
		}
		for _, s := range stmts {
			if _, err := db.sql.ExecContext(ctx, s); err != nil {
				return fmt.Errorf("store: migrate v%d→v%d: %w (stmt: %s)",
					v, next, err, firstLine(s))
			}
		}
		// PRAGMA user_version doesn't accept parameter binding — embed
		// the literal int. `next` is a small positive constant from our
		// migrations map so no injection surface.
		setVer := fmt.Sprintf(`PRAGMA user_version = %d`, next)
		if _, err := db.sql.ExecContext(ctx, setVer); err != nil {
			return fmt.Errorf("store: set user_version=%d: %w", next, err)
		}
		v = next
	}
	return nil
}

// migrations: version → ordered DDL statements to reach that version
// from the previous one. The initial schema lives entirely in v1 so
// fresh installs apply one migration.
var migrations = map[int][]string{
	1: {
		`CREATE TABLE sessions (
			id            TEXT PRIMARY KEY,
			project_path  TEXT NOT NULL,
			file_path     TEXT NOT NULL,
			first_ts      INTEGER NOT NULL,
			last_ts       INTEGER NOT NULL,
			content_hash  TEXT NOT NULL,
			last_mtime    INTEGER NOT NULL,
			turn_count    INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE INDEX sessions_last_ts ON sessions(last_ts)`,
		`CREATE INDEX sessions_file_path ON sessions(file_path)`,

		`CREATE TABLE turns (
			session_id  TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
			turn_index  INTEGER NOT NULL,
			role        TEXT NOT NULL,
			ts          INTEGER NOT NULL,
			raw_size    INTEGER NOT NULL,
			PRIMARY KEY (session_id, turn_index)
		)`,
		`CREATE INDEX turns_ts ON turns(ts)`,

		// chunks: primary FTS5 index. Porter stemming + unicode61
		// tokeniser match context-mode (PLAN.md §5 D2).
		`CREATE VIRTUAL TABLE chunks USING fts5(
			title,
			content,
			session_id UNINDEXED,
			turn_index UNINDEXED,
			role UNINDEXED,
			ts UNINDEXED,
			tokenize = 'porter unicode61'
		)`,

		// chunks_trigram: fallback for short / partial-word queries.
		// Smaller surface — only content, plus identifying columns.
		`CREATE VIRTUAL TABLE chunks_trigram USING fts5(
			content,
			session_id UNINDEXED,
			turn_index UNINDEXED,
			ts UNINDEXED,
			tokenize = 'trigram'
		)`,
	},
}

// ─── writes ───────────────────────────────────────────────────────────

// UpsertSession inserts or replaces a session row by primary key.
// Pointer arg avoids a 144-byte struct copy on the hot path.
func (db *DB) UpsertSession(ctx context.Context, s *Session) error {
	if s == nil {
		return errors.New("store: nil session")
	}
	const q = `INSERT INTO sessions
		(id, project_path, file_path, first_ts, last_ts,
		 content_hash, last_mtime, turn_count)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			project_path = excluded.project_path,
			file_path    = excluded.file_path,
			first_ts     = excluded.first_ts,
			last_ts      = excluded.last_ts,
			content_hash = excluded.content_hash,
			last_mtime   = excluded.last_mtime,
			turn_count   = excluded.turn_count`
	_, err := db.sql.ExecContext(ctx, q,
		s.ID, s.ProjectPath, s.FilePath,
		s.FirstTS.Unix(), s.LastTS.Unix(),
		s.ContentHash, s.LastMtime.Unix(), s.TurnCount,
	)
	if err != nil {
		return fmt.Errorf("store: upsert session %q: %w", s.ID, err)
	}
	return nil
}

// GetSession returns (session, true) or (zero, false) when absent.
func (db *DB) GetSession(ctx context.Context, id string) (Session, bool, error) {
	const q = `SELECT id, project_path, file_path, first_ts, last_ts,
		content_hash, last_mtime, turn_count
		FROM sessions WHERE id = ?`
	var s Session
	var first, last, mtime int64
	err := db.sql.QueryRowContext(ctx, q, id).Scan(
		&s.ID, &s.ProjectPath, &s.FilePath,
		&first, &last, &s.ContentHash, &mtime, &s.TurnCount,
	)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Session{}, false, nil
	case err != nil:
		return Session{}, false, fmt.Errorf("store: get session %q: %w", id, err)
	}
	s.FirstTS = time.Unix(first, 0).UTC()
	s.LastTS = time.Unix(last, 0).UTC()
	s.LastMtime = time.Unix(mtime, 0).UTC()
	return s, true, nil
}

// InsertTurn stores one turn row. Idempotent at the PK
// (session_id, turn_index): a re-insert is an INSERT OR REPLACE.
func (db *DB) InsertTurn(ctx context.Context, t Turn) error {
	const q = `INSERT OR REPLACE INTO turns
		(session_id, turn_index, role, ts, raw_size)
		VALUES (?, ?, ?, ?, ?)`
	_, err := db.sql.ExecContext(ctx, q,
		t.SessionID, t.TurnIndex, t.Role, t.TS.Unix(), t.RawSize,
	)
	if err != nil {
		return fmt.Errorf("store: insert turn %s/%d: %w",
			t.SessionID, t.TurnIndex, err)
	}
	return nil
}

// InsertChunk stores a chunk into both the primary fts5 index and the
// trigram side index. Both writes share a single transaction so a
// partial insert can't leave the two indexes out of sync.
// Pointer arg avoids a 96-byte struct copy when chunks are streamed
// in tight loops by indexer.
func (db *DB) InsertChunk(ctx context.Context, c *Chunk) error {
	if c == nil {
		return errors.New("store: nil chunk")
	}
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin chunk tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO chunks
			(title, content, session_id, turn_index, role, ts)
			VALUES (?, ?, ?, ?, ?, ?)`,
		c.Title, c.Content, c.SessionID, c.TurnIndex, c.Role, c.TS.Unix(),
	); err != nil {
		return fmt.Errorf("store: insert chunk (primary): %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO chunks_trigram
			(content, session_id, turn_index, ts)
			VALUES (?, ?, ?, ?)`,
		c.Content, c.SessionID, c.TurnIndex, c.TS.Unix(),
	); err != nil {
		return fmt.Errorf("store: insert chunk (trigram): %w", err)
	}
	return tx.Commit()
}

// DeleteSession removes a session and all dependent rows. Cascades
// via the foreign key on turns; chunks reference session_id as an
// UNINDEXED column, so delete them explicitly to keep both fts5
// tables in sync with the relational core.
func (db *DB) DeleteSession(ctx context.Context, id string) error {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin delete tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, q := range []string{
		`DELETE FROM chunks         WHERE session_id = ?`,
		`DELETE FROM chunks_trigram WHERE session_id = ?`,
		`DELETE FROM sessions       WHERE id = ?`, // cascades to turns
	} {
		if _, err := tx.ExecContext(ctx, q, id); err != nil {
			return fmt.Errorf("store: delete session %q (%s): %w",
				id, firstLine(q), err)
		}
	}
	return tx.Commit()
}

// ─── reads ────────────────────────────────────────────────────────────

// Stats returns row counts. Cheap (each is a single index scan).
func (db *DB) Stats(ctx context.Context) (Stats, error) {
	var s Stats
	row := db.sql.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM sessions),
		(SELECT COUNT(*) FROM turns),
		(SELECT COUNT(*) FROM chunks)`)
	if err := row.Scan(&s.Sessions, &s.Turns, &s.Chunks); err != nil {
		return Stats{}, fmt.Errorf("store: stats: %w", err)
	}
	return s, nil
}

// Search runs an FTS5 MATCH against the requested index and returns
// raw rows. Empty query → 0 rows + nil error (the caller pre-empts
// this in normal flow; here we keep the contract explicit so tests
// can prove the "harmless empty" case).
//
// This method does NOT do ranking, snippet windowing, or trigram
// fallback — those policies belong in internal/search (T-C.5).
func (db *DB) Search(ctx context.Context, query string, opts SearchOpts) ([]SearchResult, error) {
	if strings.TrimSpace(query) == "" {
		return nil, nil
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = 50
	}
	table := "chunks"
	if opts.Source == "trigram" {
		table = "chunks_trigram"
	}

	// Column list differs slightly between the two virtual tables;
	// branch the SELECT but keep the result-row scan single-shape.
	var (
		stmt string
		args []any
	)
	switch table {
	case "chunks":
		stmt = `SELECT
			title, content, session_id, turn_index, role, ts, rank
			FROM chunks WHERE chunks MATCH ?`
		args = append(args, query)
	default: // chunks_trigram
		stmt = `SELECT
			'', content, session_id, turn_index, '', ts, rank
			FROM chunks_trigram WHERE chunks_trigram MATCH ?`
		args = append(args, query)
	}
	if !opts.Since.IsZero() {
		stmt += ` AND ts >= ?`
		args = append(args, opts.Since.Unix())
	}
	if opts.Role != "" && table == "chunks" {
		stmt += ` AND role = ?`
		args = append(args, opts.Role)
	}
	stmt += ` ORDER BY rank LIMIT ?`
	args = append(args, limit)

	rows, err := db.sql.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, fmt.Errorf("store: search MATCH: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []SearchResult
	for rows.Next() {
		var (
			r  SearchResult
			ts int64
		)
		if err := rows.Scan(&r.Title, &r.Content, &r.SessionID,
			&r.TurnIndex, &r.Role, &ts, &r.MatchRank); err != nil {
			return nil, fmt.Errorf("store: scan search row: %w", err)
		}
		r.TS = time.Unix(ts, 0).UTC()
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: search rows: %w", err)
	}
	return out, nil
}

// Purge removes all conversation data without dropping the schema. The
// DB is reusable immediately afterwards. Resets WAL via a checkpoint
// so disk usage drops back to a small baseline.
func (db *DB) Purge(ctx context.Context) error {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin purge tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, q := range []string{
		`DELETE FROM chunks`,
		`DELETE FROM chunks_trigram`,
		`DELETE FROM turns`,
		`DELETE FROM sessions`,
	} {
		if _, err := tx.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("store: purge (%s): %w", firstLine(q), err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: purge commit: %w", err)
	}
	if _, err := db.sql.ExecContext(ctx,
		`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		// Best-effort — purge succeeded, WAL trim didn't.
		return fmt.Errorf("store: wal_checkpoint: %w", err)
	}
	return nil
}

// ─── helpers ──────────────────────────────────────────────────────────

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
