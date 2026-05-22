package chunk

import (
	"strings"
	"testing"
)

// ─── Split: trivial inputs ───────────────────────────────────────────

func TestSplit_EmptyReturnsNil(t *testing.T) {
	if got := Split("", Options{}); got != nil {
		t.Errorf("empty input: got %v", got)
	}
}

func TestSplit_WhitespaceOnlyReturnsNil(t *testing.T) {
	if got := Split("   \n\t\n", Options{}); got != nil {
		t.Errorf("whitespace-only: got %v", got)
	}
}

func TestSplit_SingleParagraphNoTitle(t *testing.T) {
	in := "first line\nsecond line"
	got := Split(in, Options{})
	if len(got) != 1 {
		t.Fatalf("want 1 chunk, got %d", len(got))
	}
	if got[0].Title != "" {
		t.Errorf("expected empty title in prelude, got %q", got[0].Title)
	}
	if got[0].Content != in {
		t.Errorf("content mismatch: %q", got[0].Content)
	}
}

// ─── ATX heading boundaries ─────────────────────────────────────────

func TestSplit_AtxHeadingBoundary(t *testing.T) {
	in := "prelude line\n" +
		"# First\n" +
		"under first\n" +
		"## Second\n" +
		"under second"
	got := Split(in, Options{})
	if len(got) != 3 {
		t.Fatalf("want 3 chunks (prelude + 2 sections), got %d: %+v", len(got), got)
	}
	if got[0].Title != "" || !strings.Contains(got[0].Content, "prelude") {
		t.Errorf("prelude wrong: %+v", got[0])
	}
	if got[1].Title != "First" || !strings.Contains(got[1].Content, "under first") {
		t.Errorf("first section wrong: %+v", got[1])
	}
	if got[2].Title != "Second" || !strings.Contains(got[2].Content, "under second") {
		t.Errorf("second section wrong: %+v", got[2])
	}
}

func TestSplit_AtxHeadingLevels(t *testing.T) {
	in := "# h1\nbody1\n## h2\nbody2\n### h3\nbody3"
	got := Split(in, Options{})
	if len(got) != 3 {
		t.Fatalf("want 3 chunks, got %d", len(got))
	}
	wantTitles := []string{"h1", "h2", "h3"}
	for i, c := range got {
		if c.Title != wantTitles[i] {
			t.Errorf("idx %d: title=%q, want %q", i, c.Title, wantTitles[i])
		}
	}
}

func TestSplit_AtxTrailingHashStripped(t *testing.T) {
	in := "## Decisions ##\nbody"
	got := Split(in, Options{})
	if len(got) != 1 || got[0].Title != "Decisions" {
		t.Errorf("trailing # not stripped: %+v", got)
	}
}

func TestSplit_AtxHashtagNotHeading(t *testing.T) {
	// "#hashtag" (no space after #) → not a heading per CommonMark.
	in := "#hashtag content\nmore content"
	got := Split(in, Options{})
	if len(got) != 1 {
		t.Fatalf("want 1 chunk (no heading split), got %d", len(got))
	}
	if got[0].Title != "" {
		t.Errorf("hashtag should not become a title: %q", got[0].Title)
	}
}

func TestSplit_AtxEmptyHeadingNotHeading(t *testing.T) {
	in := "###\nbody"
	got := Split(in, Options{})
	if len(got) != 1 || got[0].Title != "" {
		t.Errorf("empty heading should not split: %+v", got)
	}
}

func TestSplit_AtxLevel7NotHeading(t *testing.T) {
	// 7 hashes is not a valid ATX heading.
	in := "####### still body\nmore"
	got := Split(in, Options{})
	if len(got) != 1 || got[0].Title != "" {
		t.Errorf("7-hash should not be heading: %+v", got)
	}
}

// ─── code fence atomicity ───────────────────────────────────────────

func TestSplit_HeadingInsideFenceIgnored(t *testing.T) {
	in := "# Real heading\n" +
		"```go\n" +
		"// # not a heading\n" +
		"package main\n" +
		"```\n" +
		"trailing"
	got := Split(in, Options{})
	if len(got) != 1 {
		t.Fatalf("want 1 chunk (heading-in-fence ignored), got %d: %+v", len(got), got)
	}
	if got[0].Title != "Real heading" {
		t.Errorf("title wrong: %q", got[0].Title)
	}
	if !strings.Contains(got[0].Content, "// # not a heading") {
		t.Errorf("code body missing: %q", got[0].Content)
	}
}

func TestSplit_TildeFenceAlsoAtomic(t *testing.T) {
	in := "# heading\n" +
		"~~~\n" +
		"# inside\n" +
		"~~~"
	got := Split(in, Options{})
	if len(got) != 1 || got[0].Title != "heading" {
		t.Errorf("tilde fence not treated as fence: %+v", got)
	}
}

func TestSplit_LargeFenceNotSplit(t *testing.T) {
	// A 50 KiB fenced block should survive a small MaxBytes — fences
	// override the soft cap.
	body := strings.Repeat("payload-line\n", 5000) // ~65 KiB
	in := "# Big\n" +
		"```\n" +
		body +
		"```"
	got := Split(in, Options{MaxBytes: 1024})
	if len(got) != 1 {
		t.Errorf("fence atomicity broken: got %d chunks", len(got))
	}
	if len(got[0].Content) < len(body) {
		t.Errorf("fence body truncated: chunk=%d body=%d", len(got[0].Content), len(body))
	}
}

// ─── soft size cap ──────────────────────────────────────────────────

func TestSplit_OversizeOutsideFenceForcesSplit(t *testing.T) {
	// Build a single heading + 4 KiB of plain text, with MaxBytes 1024.
	// Expected: multiple chunks all carrying the same title.
	body := strings.Repeat("paragraph line filler text\n", 200) // ~5 KiB
	in := "# Section\n" + body
	got := Split(in, Options{MaxBytes: 1024})
	if len(got) < 2 {
		t.Fatalf("want ≥2 chunks for oversize content, got %d", len(got))
	}
	for i, c := range got {
		if c.Title != "Section" {
			t.Errorf("chunk %d lost title: %q", i, c.Title)
		}
	}
}

func TestSplit_DefaultMaxBytesUsedWhenZero(t *testing.T) {
	// 4 KiB body under default 8 KiB cap → single chunk.
	body := strings.Repeat("xx\n", 2000) // ~6 KiB
	in := "# S\n" + body
	got := Split(in, Options{})
	if len(got) != 1 {
		t.Errorf("4 KiB under default cap should be 1 chunk, got %d", len(got))
	}
}

// ─── multibyte / unicode safety ─────────────────────────────────────

func TestSplit_UnicodeContentPreserved(t *testing.T) {
	in := "# 한글 제목\n안녕하세요 친구\n## 두 번째\n어서 오세요"
	got := Split(in, Options{})
	if len(got) != 2 {
		t.Fatalf("want 2 chunks, got %d", len(got))
	}
	if got[0].Title != "한글 제목" {
		t.Errorf("unicode title: %q", got[0].Title)
	}
	if !strings.Contains(got[0].Content, "안녕하세요") {
		t.Errorf("unicode body missing: %q", got[0].Content)
	}
}

// ─── atxHeading direct unit ─────────────────────────────────────────

func TestAtxHeading_NonHeadings(t *testing.T) {
	for _, line := range []string{
		"",
		"plain text",
		"   ",
		"  not a heading",
		"#nospace",
		"#######  too many",
	} {
		if _, ok := atxHeading(line); ok {
			t.Errorf("expected non-heading: %q", line)
		}
	}
}

func TestAtxHeading_Valid(t *testing.T) {
	cases := map[string]string{
		"# foo":                 "foo",
		"## foo bar":            "foo bar",
		"###   spaces  ":        "spaces",
		"####### six max":       "", // 7 → invalid
		"  ## indented":         "indented",
		"## foo bar #":          "foo bar",
		"## foo bar ##":         "foo bar",
		"## ":                   "", // trailing whitespace only → no title
	}
	for in, want := range cases {
		got, ok := atxHeading(in)
		if want == "" {
			if ok {
				t.Errorf("%q: expected !ok, got title=%q", in, got)
			}
			continue
		}
		if !ok || got != want {
			t.Errorf("%q: got (%q, %v), want (%q, true)", in, got, ok, want)
		}
	}
}
