package handlers

import "github.com/go-chi/chi/v5"

// Mount wires the graph ingest and admin endpoints onto r.
// The caller is responsible for adding any auth middleware before calling Mount.
func Mount(r chi.Router, deps Deps) {
	r.Post("/api/graph/ingest/content", NewIngestContentHandler(deps).ServeHTTP)
	r.Post("/api/graph/ingest/url", NewIngestURLHandler(deps).ServeHTTP)
	r.Get("/api/graph/jobs", NewJobsListHandler(deps).ServeHTTP)
	r.Delete("/api/graph/jobs/{id}", NewJobsDeleteHandler(deps).ServeHTTP)
	r.Post("/api/graph/jobs/{id}/retry", NewJobsRetryHandler(deps).ServeHTTP)
}
