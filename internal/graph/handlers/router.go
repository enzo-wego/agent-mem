package handlers

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
)

// Mount wires the graph ingest, admin, and read endpoints onto r.
//
// Auth/trust model: the caller MUST gate these routes with auth middleware
// (the worker mounts them behind the API-key middleware). The API key is the
// privilege boundary: any key-bearing caller can send X-Graph-Admin: 1, so the
// ACL protects against mistakes, not against a hostile key holder.
//
// Read policy: the five filtered routes (search, node, neighbors, epic, resolve)
// need a named asker (X-Asker-User, or asker_slack_id / asker_eeid on resolve)
// and return only what that asker may see, or X-Graph-Admin: 1 for the
// unfiltered view. Every other GET under /api/graph/ is admin-only by default
// (see graphReadPolicy), so a GET route added later fails closed.
func Mount(r chi.Router, deps Deps) {
	r.Group(func(r chi.Router) {
		r.Use(graphReadPolicy)
		mountRoutes(r, deps)
	})
}

// askerFilteredGET reports whether a GET path (as chi routes it: RawPath when
// set) is one of the asker-filtered read routes.
func askerFilteredGET(path string) bool {
	rest, ok := strings.CutPrefix(path, "/api/graph/")
	if !ok {
		return false
	}
	switch rest {
	case "search", "node":
		return true
	}
	if key, ok := strings.CutPrefix(rest, "epic/"); ok {
		return key != "" && !strings.Contains(key, "/")
	}
	if id, ok := strings.CutPrefix(rest, "node/"); ok {
		if id, ok = strings.CutSuffix(id, "/neighbors"); ok {
			return id != "" && !strings.Contains(id, "/")
		}
	}
	return false
}

// graphReadPolicy makes every GET/HEAD under /api/graph/ that is not an
// asker-filtered route require X-Graph-Admin: 1.
func graphReadPolicy(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			path := r.URL.RawPath
			if path == "" {
				path = r.URL.Path
			}
			if strings.HasPrefix(path, "/api/graph/") && !askerFilteredGET(path) && !isAdminRequest(r) {
				writeAskerError(w, &askerError{http.StatusForbidden, "admin_required"})
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func mountRoutes(r chi.Router, deps Deps) {
	r.Post("/api/graph/ingest/content", NewIngestContentHandler(deps).ServeHTTP)
	r.Post("/api/graph/ingest/url", NewIngestURLHandler(deps).ServeHTTP)
	r.Get("/api/graph/jobs", NewJobsListHandler(deps).ServeHTTP)
	r.Delete("/api/graph/jobs/{id}", NewJobsDeleteHandler(deps).ServeHTTP)
	r.Post("/api/graph/jobs/{id}/retry", NewJobsRetryHandler(deps).ServeHTTP)
	r.Post("/api/graph/jobs/enqueue", NewJobsEnqueueHandler(deps).ServeHTTP)
	r.Post("/api/graph/backfill/slack", NewBackfillSlackHandler(deps).ServeHTTP)
	r.Post("/api/graph/backfill/attachments", NewBackfillAttachmentsHandler(deps).ServeHTTP)
	r.Post("/api/graph/backfill/stale-summaries", NewBackfillStaleSummariesHandler(deps).ServeHTTP)
	r.Post("/api/graph/backfill/jira-metadata", NewBackfillJiraMetadataHandler(deps).ServeHTTP)
	r.Post("/api/graph/backfill/subtree-index", NewBackfillSubtreeIndexHandler(deps).ServeHTTP)
	r.Post("/api/graph/backfill/thread-decisions", NewBackfillThreadDecisionsHandler(deps).ServeHTTP)
	r.Get("/api/graph/topic-rules", NewTopicRulesHandler().ServeHTTP)

	// Read endpoints (Phase 3).
	node := NewNode(deps.DB)
	r.Method("GET", "/api/graph/node", node)

	person := NewPerson(deps.DB)
	r.Method("GET", "/api/graph/person", person)

	search, _ := NewSearchWithEmbedder(deps.DB, deps.Gemini)
	r.Method("GET", "/api/graph/search", withContextTrace(search, "search", deps.BuildRev, deps.TraceRecorder))

	resolve, _ := NewResolve(deps.DB)
	r.Method("POST", "/api/graph/resolve", withContextTrace(resolve, "resolve", deps.BuildRev, deps.TraceRecorder))
	if deps.TraceRecorder != nil {
		r.Get("/api/graph/traces/stats", traceStatsHandler(deps.TraceRecorder))
	}

	// Epic (or business root) members grouped by type, with the activity window.
	r.Method("GET", "/api/graph/epic/{key}", NewEpic(deps.DB))

	r.Method("GET", "/api/graph/slack-users", NewSlackUsersHandler(deps))
	r.Method("GET", "/api/graph/slack-user", NewSlackUserHandler(deps))

	// Globe feature: per-channel volume + channel→continent config.
	channels := NewChannels(deps.DB)
	r.Get("/api/graph/channels", channels.list)
	r.Get("/api/graph/channels/recent", channels.recentActivity)
	r.Get("/api/graph/channel", channels.recent)
	r.Get("/api/graph/channel/topics", channels.topics)
	r.Get("/api/graph/cluster/summary", NewClusterSummary(deps))
	r.Get("/api/graph/continents", channels.getContinents)
	r.Put("/api/graph/continents", channels.putContinents)
	r.Get("/api/graph/channel-filters", channels.getChannelFilters)
	r.Put("/api/graph/channel-filters", channels.putChannelFilters)
	r.Get("/api/graph/eligibility-gate", channels.getEligibilityGate)
	r.Put("/api/graph/eligibility-gate", channels.putEligibilityGate)
	r.Get("/api/graph/business-root", channels.getBusinessRoot)
	r.Put("/api/graph/business-root", channels.putBusinessRoot)
	r.Get("/api/graph/temporal-timezone", channels.getTemporalTimezone)
	r.Put("/api/graph/temporal-timezone", channels.putTemporalTimezone)
	r.Get("/api/graph/boost-alphas", channels.getBoostAlphas)
	r.Put("/api/graph/boost-alphas", channels.putBoostAlphas)
	r.Get("/api/graph/epic-briefs", channels.getEpicBriefsConfig)
	r.Put("/api/graph/epic-briefs", channels.putEpicBriefsConfig)
	r.Get("/api/graph/purpose", channels.getPurposeConfig)
	r.Put("/api/graph/purpose", channels.putPurposeConfig)
	r.Get("/api/graph/slack-members", channels.getSlackMembersConfig)
	r.Put("/api/graph/slack-members", channels.putSlackMembersConfig)
	jiraUpdates := NewJiraUpdatesHandler(deps)
	r.Method("GET", "/api/graph/jira-updates", jiraUpdates)
	r.Method("PUT", "/api/graph/jira-updates", jiraUpdates)
	ghKeySearch := NewGHKeySearchHandler(deps)
	r.Method("GET", "/api/graph/gh-key-search", ghKeySearch)
	r.Method("PUT", "/api/graph/gh-key-search", ghKeySearch)

	// Topic subscriptions (hot-topic enzobot alerts).
	subs := NewSubscriptions(deps)
	r.Get("/api/graph/subscriptions", subs.list)
	r.Post("/api/graph/subscriptions", subs.create)
	r.Post("/api/graph/subscriptions/{id}/refresh", subs.refresh)
	r.Patch("/api/graph/subscriptions/{id}", subs.update)
	r.Delete("/api/graph/subscriptions/{id}", subs.delete)

	// Pinned threads (📌 quick-access panel on /live).
	pins := NewPins(deps.DB)
	r.Get("/api/graph/pins", pins.list)
	r.Post("/api/graph/pins", pins.create)
	r.Delete("/api/graph/pins", pins.delete)
	r.Get("/api/graph/pins/board", pins.board)

	r.Get("/api/graph/node/{id}/subject-queries", NewSubjectQueries(deps))
	r.Mount("/api/graph", NewNeighbors(deps.DB))
}
