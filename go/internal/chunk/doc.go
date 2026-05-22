// Package chunk splits long turn text into BM25-friendly chunks. Rules:
// heading-aware split, code-block-atomic (never split inside a fenced
// block), turn-boundary hard split, and an 8 KB / ~2000 token size cap
// to keep FTS5 length-normalisation healthy.
//
// Ported from context-mode src/store.ts:4–149. Implementation lands in
// T-C.3.
package chunk
