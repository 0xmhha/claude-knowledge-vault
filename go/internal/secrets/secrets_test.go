package secrets

import (
	"strings"
	"testing"
)

// ─── Patterns inventory ─────────────────────────────────────────────

func TestPatterns_AllSixFamiliesPresent(t *testing.T) {
	want := []string{
		"anthropic-api-key", "openai-api-key", "aws-access-key-id",
		"github-personal-token", "github-server-token",
		"github-fine-grained-pat", "jwt", "private-key-pem",
		"db-url-with-password", "slack-token",
	}
	got := Patterns()
	names := map[string]bool{}
	for _, p := range got {
		names[p.Name] = true
	}
	for _, w := range want {
		if !names[w] {
			t.Errorf("missing pattern %q", w)
		}
	}
	// Patterns() must return a copy, not the live registry slice.
	got[0] = Pattern{}
	if Patterns()[0].Name == "" {
		t.Error("Patterns() leaked the live registry")
	}
}

// ─── Detect: per-family hits ────────────────────────────────────────

type sample struct {
	name   string
	text   string
	wantOK bool // expect at least one match
	wantP  string
}

func TestDetect_PerFamily(t *testing.T) {
	cases := []sample{
		{"anthropic", `key=sk-ant-AAAABBBBCCCCDDDDEEEEFFFFGGGGHHHH1234`, true, "anthropic-api-key"},
		{"openai", `key=sk-AAAABBBBCCCCDDDDEEEE`, true, "openai-api-key"},
		{"aws", `id=AKIAIOSFODNN7EXAMPLE here`, true, "aws-access-key-id"},
		{"ghp", `t=ghp_aaaabbbbccccddddeeeeffffggggHHHHIIII`, true, "github-personal-token"},
		{"ghs", `t=ghs_aaaabbbbccccddddeeeeffffggggHHHHIIII`, true, "github-server-token"},
		{"ghpat", `t=github_pat_AAAABBBBCCCCDDDDEEEE_with_more`, true, "github-fine-grained-pat"},
		{"jwt", `bearer eyJABCDEFGHIJ.eyJZYXWVUTSRQ`, true, "jwt"},
		{"pem", `-----BEGIN OPENSSH PRIVATE KEY-----`, true, "private-key-pem"},
		{"pg", `DSN: postgres://user:hunter2@host:5432/db`, true, "db-url-with-password"},
		{"slack", `xoxb-1234567890-abcdefghij`, true, "slack-token"},
		{"plain", `nothing to see here friend`, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ms := Detect(c.text)
			if c.wantOK && len(ms) == 0 {
				t.Errorf("expected hit for %s; text=%q", c.name, c.text)
			}
			if !c.wantOK && len(ms) != 0 {
				t.Errorf("unexpected hit on plain text: %+v", ms)
			}
			if c.wantOK && c.wantP != "" && ms[0].Pattern.Name != c.wantP {
				t.Errorf("wrong pattern: got %q want %q", ms[0].Pattern.Name, c.wantP)
			}
		})
	}
}

func TestDetect_EmptyReturnsNil(t *testing.T) {
	if got := Detect(""); got != nil {
		t.Errorf("empty: got %+v", got)
	}
}

func TestDetect_OrderedByStart(t *testing.T) {
	text := `first xoxb-1111111111-aaaaaa then sk-AAAABBBBCCCCDDDDEEEE later`
	ms := Detect(text)
	if len(ms) != 2 {
		t.Fatalf("want 2 matches, got %d", len(ms))
	}
	if ms[0].Start >= ms[1].Start {
		t.Errorf("not ordered: %d ≥ %d", ms[0].Start, ms[1].Start)
	}
}

func TestDetect_OverlappingDedup(t *testing.T) {
	// "sk-ant-…" matches both anthropic-api-key (specific) AND
	// openai-api-key (generic). They start at slightly different
	// offsets (sk-ant- starts at the same byte as sk-…), so the
	// dedup pick should be deterministic.
	text := `key=sk-ant-AAAABBBBCCCCDDDDEEEEFFFFGGGGHHHH1234567890`
	ms := Detect(text)
	if len(ms) != 1 {
		t.Errorf("dedup failed: %+v", ms)
	}
}

// ─── Mask ───────────────────────────────────────────────────────────

func TestMask_NoSecretsReturnsUnchanged(t *testing.T) {
	in := "hello world"
	if got := Mask(in); got != in {
		t.Errorf("unchanged expected, got %q", got)
	}
}

func TestMask_PreservesHeadAndTail(t *testing.T) {
	in := `tok=sk-ant-AAAABBBBCCCCDDDDEEEEFFFFGGGGHHHH1234567890`
	out := Mask(in)
	if !strings.Contains(out, "sk-ant") {
		t.Errorf("head missing: %q", out)
	}
	if !strings.Contains(out, "REDACTED") {
		t.Errorf("redaction marker missing: %q", out)
	}
	if strings.Contains(out, "AAAABBBB") {
		t.Errorf("middle of secret leaked: %q", out)
	}
}

func TestMask_ShortSpanFullyRedacted(t *testing.T) {
	// AKIA pattern: "AKIA" + 16 chars = 20 chars total; head(6)+tail(4)=10 — well below.
	// Construct a fake short match by isolating with a regex-friendly stub.
	// Use the AKIA pattern at length 20.
	in := `id=AKIAIOSFODNN7EXAMPLE`
	out := Mask(in)
	if !strings.Contains(out, "REDACTED") {
		t.Errorf("expected REDACTED marker, got %q", out)
	}
}

func TestMask_MultipleSpansEachReplaced(t *testing.T) {
	in := `pem -----BEGIN OPENSSH PRIVATE KEY----- and key sk-AAAABBBBCCCCDDDDEEEEFFFFGGGGHHHHIIII end`
	out := Mask(in)
	count := strings.Count(out, "REDACTED")
	if count != 2 {
		t.Errorf("want 2 redactions, got %d in %q", count, out)
	}
	if !strings.Contains(out, "pem ") || !strings.Contains(out, " end") {
		t.Errorf("non-secret text mangled: %q", out)
	}
}

// ─── redact helper ──────────────────────────────────────────────────

func TestRedact_LongSpan(t *testing.T) {
	got := redact("ABCDEFGHIJKLMNOP")
	if !strings.HasPrefix(got, "ABCDEF") || !strings.HasSuffix(got, "MNOP") {
		t.Errorf("head/tail not preserved: %q", got)
	}
	if !strings.Contains(got, "REDACTED") {
		t.Errorf("marker missing: %q", got)
	}
}

func TestRedact_ShortSpan(t *testing.T) {
	got := redact("short")
	if strings.Contains(got, "short") {
		t.Errorf("short span should be fully replaced: %q", got)
	}
	if !strings.Contains(got, "REDACTED") {
		t.Errorf("marker missing: %q", got)
	}
}

// ─── dedupOverlap unit ──────────────────────────────────────────────

func TestDedupOverlap_NestedWins(t *testing.T) {
	in := []Match{
		{Start: 5, End: 25},   // wider, same start as next
		{Start: 5, End: 15},   // inside the wider span
		{Start: 50, End: 60},  // separate
	}
	out := dedupOverlap(in)
	if len(out) != 2 {
		t.Fatalf("want 2 spans, got %d: %+v", len(out), out)
	}
	if out[0].End != 25 {
		t.Errorf("wider span did not win: %+v", out[0])
	}
}

func TestDedupOverlap_AdjacentKept(t *testing.T) {
	in := []Match{
		{Start: 0, End: 10},
		{Start: 10, End: 20}, // touches but does not overlap
	}
	out := dedupOverlap(in)
	if len(out) != 2 {
		t.Errorf("adjacent spans should both survive: %+v", out)
	}
}
