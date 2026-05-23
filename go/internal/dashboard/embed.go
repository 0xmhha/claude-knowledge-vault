package dashboard

import (
	"embed"
	"io/fs"
	"net/http"
)

// webFS holds the dashboard's HTML/CSS/JS assets. //go:embed inlines
// them at compile time, so the binary is the only artifact needed at
// runtime — `web/` does not have to exist on disk for kvault to
// serve the UI.
//
//go:embed web/*
var webFS embed.FS

// defaultStaticHandler returns an http.Handler that serves the
// embedded assets at the URL paths the HTML expects (/style.css,
// /app.js, /). New() injects this when StaticHandler is left nil.
//
// fs.Sub failure here would mean the package was built without the
// web/ subtree, which the //go:embed directive makes a compile-time
// guarantee. Panic to make a packaging bug loud rather than silently
// serving 404s.
func defaultStaticHandler() http.Handler {
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		panic("dashboard: embedded web/ subtree missing: " + err.Error())
	}
	return http.FileServerFS(sub)
}
