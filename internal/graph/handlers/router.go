package handlers

import "github.com/go-chi/chi/v5"

// Mount wires the graph ingest, admin, and read endpoints onto r.
//
// Auth/trust model: the caller MUST gate these routes with auth middleware
// (the worker mounts them behind the API-key middleware). The API key is the
// privilege boundary — any key-bearing caller is trusted internal infra
// (EnzoBot, the admin dashboard). The per-request asker identity used for ACL
// (`X-Asker-User` header on search, `asker_eeid` in the resolve body) is an
// ADVISORY hint asserted by that trusted caller on a user's behalf; it is NOT
// independently authenticated. Read endpoints treat "no asker asserted"
// (eeid 0) as the trusted/unfiltered view and always filter a real asker
// (eeid != 0) — so a real user, even with zero memberships, can never read the
// whole graph. Hardening this into a real per-user boundary requires
// authenticating the asker (e.g. binding eeid to a verified principal).
func Mount(r chi.Router, deps Deps) {
	r.Post("/api/graph/ingest/content", NewIngestContentHandler(deps).ServeHTTP)
	r.Post("/api/graph/ingest/url", NewIngestURLHandler(deps).ServeHTTP)
	r.Get("/api/graph/jobs", NewJobsListHandler(deps).ServeHTTP)
	r.Delete("/api/graph/jobs/{id}", NewJobsDeleteHandler(deps).ServeHTTP)
	r.Post("/api/graph/jobs/{id}/retry", NewJobsRetryHandler(deps).ServeHTTP)
	r.Post("/api/graph/jobs/enqueue", NewJobsEnqueueHandler(deps).ServeHTTP)
	r.Post("/api/graph/backfill/slack", NewBackfillSlackHandler(deps).ServeHTTP)
	r.Post("/api/graph/backfill/attachments", NewBackfillAttachmentsHandler(deps).ServeHTTP)
	r.Get("/api/graph/topic-rules", NewTopicRulesHandler().ServeHTTP)

	// Read endpoints (Phase 3).
	node := NewNode(deps.DB)
	r.Method("GET", "/api/graph/node", node)

	person := NewPerson(deps.DB)
	r.Method("GET", "/api/graph/person", person)

	search, _ := NewSearchWithEmbedder(deps.DB, deps.Gemini)
	r.Method("GET", "/api/graph/search", search)

	resolve, _ := NewResolve(deps.DB)
	r.Method("POST", "/api/graph/resolve", resolve)

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

	r.Mount("/api/graph", NewNeighbors(deps.DB))
}
