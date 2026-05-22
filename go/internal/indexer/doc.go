// Package indexer wires source → chunk → store into a single pipeline
// runnable from CLI / MCP / dashboard. Holds the writer lock file
// (5-min stale, env-sync acquireLock pattern ported), implements
// incremental indexing via (file_path, content_hash, last_mtime), and
// exposes a progress channel for SSE.
//
// Implementation lands in T-C.7.
package indexer
