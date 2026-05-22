// Package store wraps modernc.org/sqlite to hold the FTS5 vault.
// Tables: sessions, turns, chunks (fts5 porter+unicode61), chunks_trigram
// (fts5 trigram). WAL journal mode. Schema migrations via PRAGMA
// user_version.
//
// Implementation lands in T-C.4.
package store
