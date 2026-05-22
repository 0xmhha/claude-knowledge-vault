// Package secrets re-marks well-known secret patterns (sk-*, AKIA*,
// JWT shape, ghp_*, postgres://user:pass@, BEGIN PRIVATE KEY) in
// rendered search results so a pasted-into-Claude credential is not
// shown verbatim in the dashboard or MCP output.
//
// Ported from claude-env-sync internal/exclude/patterns.go.
// Implementation lands in T-C.6.
package secrets
