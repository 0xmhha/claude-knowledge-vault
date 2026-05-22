// Package mcp is the in-house JSON-RPC over stdio implementation of
// the Model Context Protocol used by the kvault binary in --mcp mode.
// Forked verbatim from claude-env-sync internal/mcp (~280 LoC) — the
// only changes will be tool names and the Server name string.
//
// Implementation lands in T-C.8.
package mcp
