// Package source walks ~/.claude/projects/, decodes line-delimited
// jsonl turn records, applies mtime gating for incremental indexing,
// and yields normalised Turn{role, text, session_id, turn_index, ts}
// values via a streaming API.
//
// Implementation lands in T-C.2.
package source
