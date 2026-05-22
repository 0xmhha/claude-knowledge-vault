// Package chunk splits one turn's text into BM25-indexable chunks.
//
// Rules (PLAN.md §5 D4, ported from context-mode src/store.ts:4–149):
//
//   - Split at ATX heading boundaries (^#{1,6}\s+...).
//   - Never split inside a fenced code block (``` or ~~~). Headings
//     inside code are not headings.
//   - Apply a soft size cap (default 8 KiB) — when crossed *outside*
//     a code fence, emit the in-flight chunk and start a fresh one
//     under the same title. Inside a fence we keep accumulating;
//     a 50 KiB code block ships as one chunk rather than getting
//     cut mid-syntax.
//   - Empty / whitespace-only chunks are dropped at emit time.
//
// The package operates on raw text, no jsonl / SQL coupling — the
// turn-boundary rule from PLAN.md is enforced by the caller (the
// indexer feeds one turn at a time).
package chunk

import (
	"strings"
)

// DefaultMaxBytes is the soft size cap. Long chunks hurt fts5 length
// normalisation; ~8 KiB ≈ 2 K tokens which is comfortable for BM25.
const DefaultMaxBytes = 8 * 1024

// Chunk is one indexable unit.
type Chunk struct {
	// Title is the heading text the chunk sits under (no leading #).
	// Empty when the chunk is in the prelude before any heading, or
	// when the soft cap forced a continuation chunk.
	Title string
	// Content is the chunk body — the lines under (and including
	// the line after) the heading, joined by "\n".
	Content string
}

// Options controls Split.
type Options struct {
	// MaxBytes is the soft size cap. ≤0 → DefaultMaxBytes.
	MaxBytes int
}

// Split runs the chunker over a single turn's text.
//
// Output ordering is stable (matches source order). Empty input
// returns nil.
func Split(text string, opts Options) []Chunk {
	if text == "" {
		return nil
	}
	maxBytes := opts.MaxBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}

	var (
		out         []Chunk
		curTitle    string
		curLines    []string
		curBytes    int
		insideFence bool
	)

	// flush emits the in-flight chunk (if non-empty) and resets the
	// buffer, keeping curTitle so a forced split continues under the
	// same heading.
	flush := func() {
		if len(curLines) == 0 {
			return
		}
		body := strings.Join(curLines, "\n")
		if strings.TrimSpace(body) != "" {
			out = append(out, Chunk{Title: curTitle, Content: body})
		}
		curLines = curLines[:0]
		curBytes = 0
	}

	for _, raw := range strings.Split(text, "\n") {
		// Detect fenced code boundary. We toggle on lines whose
		// trimmed-left form starts with ``` or ~~~ — the same
		// recognition CommonMark uses. We do NOT track the fence
		// character or info string; that's fine for chunking because
		// pairing happens line-to-line regardless of the fence
		// character (we never see asymmetric fences in Claude output).
		stripped := strings.TrimLeft(raw, " \t")
		if strings.HasPrefix(stripped, "```") || strings.HasPrefix(stripped, "~~~") {
			curLines = append(curLines, raw)
			curBytes += len(raw) + 1
			insideFence = !insideFence
			continue
		}

		// Heading detection: ATX-style, outside a fence only.
		if !insideFence {
			if h, ok := atxHeading(raw); ok {
				// Heading starts a fresh chunk. Flush whatever was
				// accumulating under the previous heading first.
				flush()
				curTitle = h
				curLines = append(curLines, raw)
				curBytes += len(raw) + 1
				continue
			}
		}

		curLines = append(curLines, raw)
		curBytes += len(raw) + 1

		// Soft cap: only honoured outside a fence so we don't split
		// mid-code-block.
		if !insideFence && curBytes >= maxBytes {
			flush()
		}
	}
	flush()
	return out
}

// atxHeading returns (title, true) when line is an ATX heading
// per CommonMark: 1–6 '#' followed by at least one space, then text.
// Strips trailing # decorations and surrounding whitespace from the
// title. Lines like "###" alone (no text) are not treated as headings.
func atxHeading(line string) (title string, ok bool) {
	s := strings.TrimLeft(line, " \t")
	if s == "" || s[0] != '#' {
		return "", false
	}
	// Count leading #s (max 6).
	n := 0
	for n < len(s) && s[n] == '#' {
		n++
	}
	if n == 0 || n > 6 {
		return "", false
	}
	// Must be followed by a space — "#hashtag" is not a heading.
	if n >= len(s) || (s[n] != ' ' && s[n] != '\t') {
		return "", false
	}
	rest := strings.TrimSpace(s[n+1:])
	// CommonMark allows a trailing run of # characters as decoration
	// (e.g. "## foo ##"); strip them when separated by whitespace.
	rest = strings.TrimRight(rest, " \t")
	for strings.HasSuffix(rest, "#") {
		trimmed := strings.TrimRight(rest, "#")
		// Only strip when the run was preceded by whitespace; "abc##"
		// is intentional content, not a decoration.
		if trimmed == rest {
			break
		}
		if trimmed == "" || trimmed[len(trimmed)-1] == ' ' || trimmed[len(trimmed)-1] == '\t' {
			rest = strings.TrimRight(trimmed, " \t")
			continue
		}
		break
	}
	if rest == "" {
		return "", false
	}
	return rest, true
}
