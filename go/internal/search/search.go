// Package search turns a raw user query into ranked, snippeted, fallback-
// aware results on top of an internal/store DB.
//
// Responsibilities (per PLAN.md §5 D2):
//   - Safe FTS5 MATCH expression building (no user-supplied operators)
//   - Score normalisation: store returns the raw fts5 rank (negative,
//     lower-is-better); we emit a positive Score (higher-is-better)
//   - Snippet windowing: a 240-char window centred on the first hit
//   - Trigram fallback: when the primary BM25 lane returns 0 rows,
//     re-run against chunks_trigram so partial-word queries land
//
// Nothing in this package opens DB connections or builds SQL — the
// store package keeps that monopoly so the schema can evolve without
// breaking callers here.
package search

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/wm-it/claude-knowledge-vault/internal/store"
)

// defaultLimit caps a single Run call when caller leaves Options.Limit
// unset. The dashboard happily renders 20; AI agents asking "what did I
// decide?" rarely want more than ~10.
const defaultLimit = 20

// defaultSnippetChars is the per-result snippet window width. A user-
// facing taste decision exposed at Phase-4 (T-C.5 C4 — see PLAN.md
// §14). 240 ≈ two terminal lines.
const defaultSnippetChars = 240

// Source labels which virtual table produced the hit. The dashboard
// uses this to render a "did you mean" lane separately.
type Source string

// Source values — emitted on each Result so the dashboard can render
// "did you mean" trigram fallbacks distinctly from the primary lane.
const (
	SourceBM25    Source = "bm25"
	SourceTrigram Source = "trigram"
)

// Result is the user-facing hit shape.
type Result struct {
	SessionID string    `json:"session_id"`
	TurnIndex int       `json:"turn_index"`
	Role      string    `json:"role,omitempty"`
	TS        time.Time `json:"ts"`
	Title     string    `json:"title,omitempty"`
	Snippet   string    `json:"snippet"`
	Score     float64   `json:"score"`
	Source    Source    `json:"source"`
}

// Options controls Run. All fields optional; zero values give sensible
// defaults.
type Options struct {
	// Query is the raw user input. Empty / whitespace → 0 rows.
	Query string
	// Limit caps results. ≤0 → defaultLimit.
	Limit int
	// Since restricts to ts >= Since. Zero → no lower bound.
	Since time.Time
	// Role restricts to a specific Claude turn role (e.g. "user").
	// Empty → any role.
	Role string
	// SnippetChars overrides the snippet window width. ≤0 → default.
	SnippetChars int
	// DisableFallback skips the trigram lane even when BM25 returns 0.
	// Useful for benchmarks; user surface keeps fallback on.
	DisableFallback bool
}

// Run executes a search against the store and returns ranked results.
// Errors are limited to DB / context issues — an empty query, an empty
// store, or a zero-hit query all return (nil, nil) so the dashboard
// can render "no results" without an error banner.
// Pointer arg avoids the 80-byte Options copy on the hot path.
func Run(ctx context.Context, db *store.DB, opts *Options) ([]Result, error) {
	if db == nil {
		return nil, errors.New("search: nil store")
	}
	if opts == nil {
		return nil, errors.New("search: nil options")
	}
	q := strings.TrimSpace(opts.Query)
	if q == "" {
		return nil, nil
	}
	limit := opts.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	snipChars := opts.SnippetChars
	if snipChars <= 0 {
		snipChars = defaultSnippetChars
	}

	matchExpr, terms := buildMATCH(q)
	if matchExpr == "" {
		// Nothing usable after escape (query was punctuation-only).
		return nil, nil
	}

	primary, err := db.Search(ctx, matchExpr, store.SearchOpts{
		Limit:  limit,
		Since:  opts.Since,
		Role:   opts.Role,
		Source: "bm25",
	})
	if err != nil {
		return nil, fmt.Errorf("search: bm25: %w", err)
	}
	if len(primary) > 0 {
		return finalise(primary, terms, snipChars, SourceBM25), nil
	}
	if opts.DisableFallback {
		return nil, nil
	}

	// Fallback lane: trigram virtual table. Same MATCH expression
	// works because we wrap each term in a phrase — trigram tokeniser
	// still finds substrings within those phrases.
	trigram, err := db.Search(ctx, matchExpr, store.SearchOpts{
		Limit:  limit,
		Since:  opts.Since,
		Source: "trigram",
	})
	if err != nil {
		return nil, fmt.Errorf("search: trigram: %w", err)
	}
	return finalise(trigram, terms, snipChars, SourceTrigram), nil
}

// ─── MATCH building ──────────────────────────────────────────────────

// buildMATCH turns user input into a safe fts5 MATCH expression.
// Strategy: split on whitespace, drop non-alphanumeric leading /
// trailing characters, escape any embedded double-quote by doubling,
// wrap each term in double quotes (phrase syntax), and AND them
// implicitly by space concatenation.
//
// Why per-word phrase wrap: fts5 phrase quoting suppresses every
// fts5 operator inside the quotes (* + - ^ ( ) NEAR : etc.), which
// is the only safe way to accept arbitrary user input without a
// dedicated parser. Implicit AND matches the user's mental model of
// "give me hits with all these words".
//
// Returns the MATCH string + the term slice (lowercased, used for
// in-memory snippet centring).
func buildMATCH(query string) (matchExpr string, terms []string) {
	raw := strings.Fields(query)
	terms = make([]string, 0, len(raw))
	parts := make([]string, 0, len(raw))
	for _, w := range raw {
		clean := stripBoundaryPunct(w)
		if clean == "" {
			continue
		}
		// Escape any embedded " by doubling. Then wrap in quotes.
		esc := strings.ReplaceAll(clean, `"`, `""`)
		parts = append(parts, `"`+esc+`"`)
		terms = append(terms, strings.ToLower(clean))
	}
	if len(parts) == 0 {
		return "", nil
	}
	return strings.Join(parts, " "), terms
}

// stripBoundaryPunct removes ASCII punctuation from both ends of a
// token. We keep internal punctuation (e.g. "HMAC-SHA256") because
// hyphen-bearing words are common in technical conversation and fts5
// phrase quoting will protect them.
func stripBoundaryPunct(s string) string {
	for s != "" && !isWordRune(rune(s[0])) {
		s = s[1:]
	}
	for s != "" && !isWordRune(rune(s[len(s)-1])) {
		s = s[:len(s)-1]
	}
	return s
}

func isWordRune(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
		(r >= '0' && r <= '9') || r >= 0x80
}

// ─── snippet + score ─────────────────────────────────────────────────

// finalise turns raw store rows into user-facing Results with a
// snippet, a positive score, and a Source label.
func finalise(rows []store.SearchResult, terms []string, snipChars int, source Source) []Result {
	out := make([]Result, 0, len(rows))
	for i := range rows {
		r := rows[i]
		out = append(out, Result{
			SessionID: r.SessionID,
			TurnIndex: r.TurnIndex,
			Role:      r.Role,
			TS:        r.TS,
			Title:     r.Title,
			Snippet:   extractSnippet(r.Content, terms, snipChars),
			// fts5 returns rank as a negative double (lower = better).
			// Convert to a positive score for the API (higher = better).
			Score:  -r.MatchRank,
			Source: source,
		})
	}
	return out
}

// extractSnippet returns a snipChars-wide window of content centred
// on the first term hit, with ellipsis prefix / suffix when the
// window doesn't cover the whole content.
//
// All matching is case-insensitive but cuts on the original-cased
// content boundary so the visible snippet preserves the user's
// original prose.
func extractSnippet(content string, terms []string, snipChars int) string {
	if content == "" {
		return ""
	}
	if utf8.RuneCountInString(content) <= snipChars {
		return content
	}

	pos := firstHit(content, terms)
	half := snipChars / 2
	start := pos - half
	if start < 0 {
		start = 0
	}
	end := start + snipChars
	if end > len(content) {
		end = len(content)
		start = end - snipChars
		if start < 0 {
			start = 0
		}
	}
	// Don't slice mid-rune — back up to the previous rune boundary.
	start = backToRune(content, start)
	end = backToRune(content, end)

	var b strings.Builder
	if start > 0 {
		b.WriteString("… ")
	}
	b.WriteString(content[start:end])
	if end < len(content) {
		b.WriteString(" …")
	}
	return b.String()
}

// firstHit returns the byte offset of the first case-insensitive
// occurrence of any term in content; returns 0 when none matches
// (we still want a window from the start of the content).
func firstHit(content string, terms []string) int {
	lower := strings.ToLower(content)
	best := -1
	for _, t := range terms {
		if t == "" {
			continue
		}
		if idx := strings.Index(lower, t); idx >= 0 {
			if best == -1 || idx < best {
				best = idx
			}
		}
	}
	if best < 0 {
		return 0
	}
	return best
}

// backToRune snaps i back to the start of the rune that byte index i
// falls inside. Required when slicing a UTF-8 string by byte offset.
func backToRune(s string, i int) int {
	if i <= 0 {
		return 0
	}
	if i >= len(s) {
		return len(s)
	}
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return i
}
