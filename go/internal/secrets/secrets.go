// Package secrets re-marks well-known credential shapes in text that
// is about to be rendered to the user (dashboard / MCP output).
//
// The regex set is the one claude-env-sync's internal/exclude already
// ships and gitleaks already blocks at commit time. We re-use it here
// for a different purpose: knowledge-vault is the only kvault surface
// that legitimately reads back text the user may have *pasted into a
// Claude chat* — a chat turn quoting "here is my OPENAI_API_KEY: …"
// will sit in the jsonl forever, get indexed, and could land in a
// search result. Masking before render keeps that string from
// appearing again in a screenshot / shoulder-surf / shared dashboard
// session.
//
// Masking policy: keep the first 6 and last 4 characters of the
// matched span and replace the middle with "…XX-redacted-XX…". Six
// leading characters are enough to identify the provider
// ("sk-ant", "ghp_", "AKIA…") without exposing the token.
//
// Detect returns positions so callers that need to highlight (rather
// than rewrite) can do so without re-running regex.
package secrets

import (
	"regexp"
	"strings"
)

// Pattern names one regex family. Identical structure to env-sync's
// SecretPattern so a future shared-library extraction is mechanical.
type Pattern struct {
	Name        string
	Description string
	Regex       *regexp.Regexp
}

// patterns is the registry. Order matters only for tie-breaking when
// two regexes match the same span — the earlier one wins. Patterns are
// intentionally narrow; broad detection lives in gitleaks at CI time.
var patterns = []Pattern{
	{
		Name:        "anthropic-api-key",
		Description: "Anthropic Claude API key",
		Regex:       regexp.MustCompile(`sk-ant-[a-zA-Z0-9_-]{32,}`),
	},
	{
		Name:        "openai-api-key",
		Description: "OpenAI / OpenAI-compatible API key",
		Regex:       regexp.MustCompile(`\bsk-[a-zA-Z0-9_-]{20,}`),
	},
	{
		Name:        "aws-access-key-id",
		Description: "AWS access key ID",
		Regex:       regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
	},
	{
		Name:        "github-personal-token",
		Description: "GitHub personal access token (classic)",
		Regex:       regexp.MustCompile(`\bghp_[A-Za-z0-9]{36}\b`),
	},
	{
		Name:        "github-server-token",
		Description: "GitHub server-to-server token",
		Regex:       regexp.MustCompile(`\bghs_[A-Za-z0-9]{36}\b`),
	},
	{
		Name:        "github-fine-grained-pat",
		Description: "GitHub fine-grained personal access token",
		Regex:       regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}\b`),
	},
	{
		Name:        "jwt",
		Description: "JSON Web Token (header.payload)",
		Regex:       regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.eyJ[A-Za-z0-9_-]{10,}`),
	},
	{
		Name:        "private-key-pem",
		Description: "PEM-encoded private key",
		//nolint:gocritic // regexpSimplify: literal 5-dash mirrors RFC 7468
		Regex: regexp.MustCompile(`-----BEGIN (RSA|EC|OPENSSH|DSA|PRIVATE) (PRIVATE )?KEY-----`),
	},
	{
		Name:        "db-url-with-password",
		Description: "Database URL with embedded password",
		Regex:       regexp.MustCompile(`\b(postgres(ql)?|mysql|mongodb(\+srv)?)://[^:/\s]+:[^@/\s]+@[^\s/]+`),
	},
	{
		Name:        "slack-token",
		Description: "Slack bot/user token",
		Regex:       regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`),
	},
}

// Match is one detected span.
type Match struct {
	// Pattern is the rule that fired.
	Pattern Pattern
	// Start / End are byte offsets into the original text.
	Start, End int
}

// Patterns returns a copy of the registry — useful for tests and
// for tools that want to enumerate what we detect.
func Patterns() []Pattern {
	out := make([]Pattern, len(patterns))
	copy(out, patterns)
	return out
}

// Detect returns every secret match in text, ordered by start offset.
// Overlapping matches are de-duplicated — the first (leftmost, then
// earliest-registered) wins, mirroring the precedence Mask applies.
func Detect(text string) []Match {
	if text == "" {
		return nil
	}
	var raw []Match
	for _, p := range patterns {
		locs := p.Regex.FindAllStringIndex(text, -1)
		for _, l := range locs {
			raw = append(raw, Match{Pattern: p, Start: l[0], End: l[1]})
		}
	}
	if len(raw) == 0 {
		return nil
	}
	return dedupOverlap(raw)
}

// Mask returns text with every detected span replaced by a redacted
// stub that preserves the first 6 and last 4 characters of the span.
// Returns text unchanged when nothing matches.
func Mask(text string) string {
	matches := Detect(text)
	if len(matches) == 0 {
		return text
	}
	var b strings.Builder
	b.Grow(len(text))
	cursor := 0
	for _, m := range matches {
		if m.Start < cursor {
			// Shouldn't happen after dedupOverlap, but guard against
			// pathological input rather than panic.
			continue
		}
		b.WriteString(text[cursor:m.Start])
		b.WriteString(redact(text[m.Start:m.End]))
		cursor = m.End
	}
	b.WriteString(text[cursor:])
	return b.String()
}

// redact builds the masked stub. Spans short enough that head+tail
// would overlap are replaced with a fixed-width placeholder so we
// never accidentally print most of the original.
func redact(s string) string {
	const head = 6
	const tail = 4
	const sep = "…REDACTED…"
	if len(s) <= head+tail {
		return sep
	}
	return s[:head] + sep + s[len(s)-tail:]
}

// dedupOverlap removes overlapping matches. After sorting by Start
// (then by End descending so the wider match wins on tie), any
// match whose Start sits inside the previous accepted match's span
// is dropped.
func dedupOverlap(in []Match) []Match {
	// Insertion sort — secrets in any one render are rare.
	for i := 1; i < len(in); i++ {
		for j := i; j > 0 && less(in[j], in[j-1]); j-- {
			in[j], in[j-1] = in[j-1], in[j]
		}
	}
	out := in[:0]
	var lastEnd int
	for i := range in {
		if i > 0 && in[i].Start < lastEnd {
			continue // overlaps the previous accepted span
		}
		out = append(out, in[i])
		lastEnd = in[i].End
	}
	return out
}

func less(a, b Match) bool {
	if a.Start != b.Start {
		return a.Start < b.Start
	}
	// On equal Start, prefer the wider span so a generic regex
	// doesn't shadow a more specific one's full extent.
	return a.End > b.End
}
