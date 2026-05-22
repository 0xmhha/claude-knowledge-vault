// Package search builds FTS5 MATCH queries, ranks via BM25 with title /
// role boosting, extracts 240-char snippet windows centred on the match,
// and falls back to the trigram virtual table for short / partial-word
// queries.
//
// Implementation lands in T-C.5.
package search
