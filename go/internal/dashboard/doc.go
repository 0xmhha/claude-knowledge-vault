// Package dashboard exposes the indexer + search over stdlib net/http
// on 127.0.0.1, with SSE for live index progress. Mirrors the layout
// of claude-env-sync internal/dashboard. Web assets (HTML/CSS/JS)
// ship inside the binary via //go:embed web/*.
//
// Implementation lands in T-D.3 (handlers) and T-D.4 (embed wire).
package dashboard
