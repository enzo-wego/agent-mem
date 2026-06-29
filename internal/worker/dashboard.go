package worker

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed all:dashboard
var dashboardFS embed.FS

// serveDashboard returns an http.Handler that serves the embedded dashboard files.
// Falls back to index.html for SPA client-side routing.
func serveDashboard() http.Handler {
	sub, err := fs.Sub(dashboardFS, "dashboard")
	if err != nil {
		panic("embedded dashboard not found: " + err.Error())
	}
	fileServer := http.FileServer(http.FS(sub))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Try serving the file directly
		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "" {
			path = "index.html"
		}

		if f, err := sub.Open(path); err == nil {
			f.Close()
			// index.html must never be cached, or a browser keeps loading an old
			// entry point that references a since-deleted bundle hash and white-pages.
			// Hashed assets under assets/ are immutable, so cache them hard.
			if path == "index.html" {
				w.Header().Set("Cache-Control", "no-cache")
			} else if strings.HasPrefix(path, "assets/") {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			fileServer.ServeHTTP(w, r)
			return
		}

		// A missing hashed asset must 404 — never fall back to index.html, or the
		// browser executes HTML as a JS/CSS module and white-pages. ponytail:
		// assets/ is the only hashed-bundle dir; SPA fallback stays for routes.
		if strings.HasPrefix(path, "assets/") {
			http.NotFound(w, r)
			return
		}

		// SPA fallback: serve index.html for unmatched client-side routes.
		w.Header().Set("Cache-Control", "no-cache")
		r.URL.Path = "/"
		fileServer.ServeHTTP(w, r)
	})
}
