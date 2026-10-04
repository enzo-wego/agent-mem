package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/rs/zerolog"

	"github.com/agent-mem/agent-mem/internal/graph/jobs"
)

// backfillJiraMetadataRequest is the request body for POST
// /api/graph/backfill/jira-metadata. Body is optional.
type backfillJiraMetadataRequest struct {
	// Limit caps the nodes enqueued in this call (default 200, max 500).
	Limit int `json:"limit"`
	// SpacingSeconds spreads the jobs' available_at so the Jira fetcher is not
	// hit with the whole batch at once (default 2s → 500 jobs over ~17 min).
	SpacingSeconds int `json:"spacing_seconds"`
	// Force re-fetches nodes that already carry metadata.status.
	Force bool `json:"force"`
}

// backfillJiraMetadataResponse is the response body.
type backfillJiraMetadataResponse struct {
	Status    string `json:"status"`
	Matched   int    `json:"matched"`
	Enqueued  int    `json:"enqueued"`
	Remaining int    `json:"remaining"`
	Limit     int    `json:"limit"`
}

const (
	backfillJiraMetadataDefaultLimit   = 200
	backfillJiraMetadataDefaultSpacing = 2 * time.Second
)

// NewBackfillJiraMetadataHandler returns an http.Handler for POST
// /api/graph/backfill/jira-metadata. It re-enqueues fetch_body for Jira nodes
// whose metadata lacks the status/issuetype/… fields the normalizer now lifts
// (round 0.1), paced via available_at so the batch does not turn into a wall
// of ratelimited fetch failures. Explicitly triggered, capped, deduped, and
// deliberately NOT wired into worker startup or any scheduled job.
func NewBackfillJiraMetadataHandler(deps Deps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req backfillJiraMetadataRequest
		// Optional body: ignore decode errors (empty/absent body -> defaults).
		_ = json.NewDecoder(r.Body).Decode(&req)

		limit := req.Limit
		if limit <= 0 {
			limit = backfillJiraMetadataDefaultLimit
		}
		if limit > 500 {
			writeError(w, http.StatusBadRequest, "limit must be between 1 and 500")
			return
		}
		spacing := backfillJiraMetadataDefaultSpacing
		if req.SpacingSeconds > 0 {
			spacing = time.Duration(req.SpacingSeconds) * time.Second
		}

		matched, enqueued, remaining := BackfillJiraMetadata(r.Context(), deps.DB, deps.Logger, deps.MachineID, limit, spacing, req.Force)
		writeJSON(w, http.StatusAccepted, backfillJiraMetadataResponse{
			Status:    "ok",
			Matched:   matched,
			Enqueued:  enqueued,
			Remaining: remaining,
			Limit:     limit,
		})
	})
}

// BackfillJiraMetadata enqueues fetch_body for up to limit Jira nodes that do
// not yet carry metadata.status (all of them when force), oldest first, each
// job's available_at spaced by spacing. Nodes with a fetch_body already
// queued/running are skipped. Returns candidates seen, jobs enqueued, and the
// count still missing after this page.
func BackfillJiraMetadata(ctx context.Context, db jobs.DB, log zerolog.Logger, machineID string, limit int, spacing time.Duration, force bool) (matched, enqueued, remaining int) {
	rows, err := db.Query(ctx, `
SELECT id FROM graph.nodes
WHERE type = 'jira' AND deleted_at IS NULL
  AND ($2 OR NOT (metadata ? 'status'))
  AND NOT EXISTS (
    SELECT 1 FROM graph.jobs j
    WHERE j.type = 'fetch_body' AND j.status IN ('queued','running') AND j.payload->>'node_id' = graph.nodes.id)
ORDER BY first_seen_at ASC, id ASC
LIMIT $1`, limit, force)
	if err != nil {
		log.Warn().Err(err).Msg("backfill_jira_metadata: select failed")
		return 0, 0, 0
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			log.Warn().Err(err).Msg("backfill_jira_metadata: scan failed")
			return 0, 0, 0
		}
		ids = append(ids, id)
	}
	rows.Close()
	matched = len(ids)

	now := time.Now()
	for i, id := range ids {
		if _, e := jobs.Enqueue(ctx, db, "fetch_body", fetchBodyPayload{NodeID: id, SkipAttachments: true}, jobs.EnqueueOptions{
			Priority:    7,
			AvailableAt: now.Add(time.Duration(i) * spacing),
			MachineID:   machineID,
		}); e != nil {
			log.Warn().Err(e).Str("node_id", id).Msg("backfill_jira_metadata: enqueue fetch_body failed")
			continue
		}
		enqueued++
	}

	_ = db.QueryRow(ctx, `
SELECT count(*) FROM graph.nodes
WHERE type = 'jira' AND deleted_at IS NULL AND NOT (metadata ? 'status')
  AND NOT EXISTS (
    SELECT 1 FROM graph.jobs j
    WHERE j.type = 'fetch_body' AND j.status IN ('queued','running') AND j.payload->>'node_id' = graph.nodes.id)`).Scan(&remaining)

	log.Info().Int("matched", matched).Int("enqueued", enqueued).Int("remaining", remaining).Msg("backfill_jira_metadata: done")
	return matched, enqueued, remaining
}
