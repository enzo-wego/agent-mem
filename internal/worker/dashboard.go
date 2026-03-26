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
			fileServer.ServeHTTP(w, r)
			return
		}

		// SPA fallback: serve index.html for unmatched routes
		r.URL.Path = "/"
		fileServer.ServeHTTP(w, r)
	})
}
