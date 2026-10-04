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
	// Force re-fetches nodes that already carry metadata.status. Force pages
	// by node id: follow next_after_id until it comes back empty.
	Force bool `json:"force"`
	// AfterID (force mode only) returns only nodes with id > after_id.
	AfterID string `json:"after_id"`
	// RetryFailed re-queues nodes whose latest fetch_body job failed; without
	// it those nodes are skipped (they would otherwise be re-picked forever).
	RetryFailed bool `json:"retry_failed"`
}

// backfillJiraMetadataResponse is the response body.
type backfillJiraMetadataResponse struct {
	Status        string `json:"status"`
	Matched       int    `json:"matched"`
	Enqueued      int    `json:"enqueued"`
	Remaining     int    `json:"remaining"`
	Limit         int    `json:"limit"`
	NextAfterID   string `json:"next_after_id"`
	EnqueueErrors int    `json:"enqueue_errors,omitempty"`
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

		res := BackfillJiraMetadata(r.Context(), deps.DB, deps.Logger, deps.MachineID, jiraBackfillParams{
			Limit:       limit,
			Spacing:     spacing,
			Force:       req.Force,
			AfterID:     req.AfterID,
			RetryFailed: req.RetryFailed,
		})
		writeJSON(w, http.StatusAccepted, backfillJiraMetadataResponse{
			Status:        "ok",
			Matched:       res.Matched,
			Enqueued:      res.Enqueued,
			Remaining:     res.Remaining,
			Limit:         limit,
			NextAfterID:   res.NextAfterID,
			EnqueueErrors: res.EnqueueErrors,
		})
	})
}

// jiraBackfillParams are the inputs of BackfillJiraMetadata.
type jiraBackfillParams struct {
	Limit       int
	Spacing     time.Duration
	Force       bool
	AfterID     string // force mode only
	RetryFailed bool
}

// jiraBackfillResult is what BackfillJiraMetadata reports.
type jiraBackfillResult struct {
	Matched, Enqueued, Remaining, EnqueueErrors int
	// NextAfterID is the force-mode cursor: the last id of the page, or "" when
	// the page was short (done). Empty outside force mode.
	NextAfterID string
}

// enqueueFetchBodyForBackfill is a seam so a test can fail one enqueue.
var enqueueFetchBodyForBackfill = func(ctx context.Context, db jobs.DB, nodeID, machineID string, availableAt time.Time) error {
	_, err := jobs.Enqueue(ctx, db, "fetch_body", fetchBodyPayload{NodeID: nodeID, SkipAttachments: true}, jobs.EnqueueOptions{
		Priority:    7,
		AvailableAt: availableAt,
		MachineID:   machineID,
	})
	return err
}

// jiraBackfillQueuedOrRunning excludes nodes with a fetch_body in flight.
// jiraBackfillLatestNotFailed excludes nodes whose most recent fetch_body job
// (highest id) failed; callers OR it with retry_failed to opt back in.
const (
	jiraBackfillQueuedOrRunning = `NOT EXISTS (
    SELECT 1 FROM graph.jobs j
    WHERE j.type = 'fetch_body' AND j.status IN ('queued','running') AND j.payload->>'node_id' = n.id)`
	jiraBackfillLatestNotFailed = `NOT EXISTS (
    SELECT 1 FROM (
      SELECT j.status FROM graph.jobs j
      WHERE j.type = 'fetch_body' AND j.payload->>'node_id' = n.id
      ORDER BY j.id DESC LIMIT 1) lj
    WHERE lj.status = 'failed')`
)

// $1 limit, $2 force, $3 after_id, $4 retry_failed. The cursor applies only in
// force mode; non-force pages order by first_seen_at, force pages by id only.
const jiraBackfillPageSQL = `
SELECT n.id FROM graph.nodes n
WHERE n.type = 'jira' AND n.deleted_at IS NULL
  AND ($2 OR NOT (n.metadata ? 'status'))
  AND (NOT $2 OR n.id > $3)
  AND ` + jiraBackfillQueuedOrRunning + `
  AND ($4 OR ` + jiraBackfillLatestNotFailed + `)
ORDER BY CASE WHEN $2 THEN NULL ELSE n.first_seen_at END ASC, n.id ASC
LIMIT $1`

const jiraBackfillRemainingSQL = `
SELECT count(*) FROM graph.nodes n
WHERE n.type = 'jira' AND n.deleted_at IS NULL AND NOT (n.metadata ? 'status')
  AND ` + jiraBackfillQueuedOrRunning + `
  AND ($1 OR ` + jiraBackfillLatestNotFailed + `)`

// BackfillJiraMetadata enqueues fetch_body for up to p.Limit Jira nodes that do
// not yet carry metadata.status (all of them when p.Force), each job's
// available_at spaced by p.Spacing. Non-force pages are oldest-first; force
// pages are by id with a cursor (see jiraBackfillResult.NextAfterID). Nodes
// with a fetch_body queued/running are skipped, and so are nodes whose latest
// fetch_body failed unless p.RetryFailed.
func BackfillJiraMetadata(ctx context.Context, db jobs.DB, log zerolog.Logger, machineID string, p jiraBackfillParams) jiraBackfillResult {
	var res jiraBackfillResult
	rows, err := db.Query(ctx, jiraBackfillPageSQL, p.Limit, p.Force, p.AfterID, p.RetryFailed)
	if err != nil {
		log.Warn().Err(err).Msg("backfill_jira_metadata: select failed")
		return res
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			log.Warn().Err(err).Msg("backfill_jira_metadata: scan failed")
			return res
		}
		ids = append(ids, id)
	}
	rows.Close()
	res.Matched = len(ids)

	lastEnqueued := p.AfterID
	stopped := false
	now := time.Now()
	for i, id := range ids {
		if e := enqueueFetchBodyForBackfill(ctx, db, id, machineID, now.Add(time.Duration(i)*p.Spacing)); e != nil {
			log.Warn().Err(e).Str("node_id", id).Msg("backfill_jira_metadata: enqueue fetch_body failed")
			if p.Force {
				// Stop here so the cursor retries this node.
				res.EnqueueErrors = 1
				stopped = true
				break
			}
			continue
		}
		res.Enqueued++
		lastEnqueued = id
	}

	if p.Force {
		switch {
		case stopped:
			res.NextAfterID = lastEnqueued
		case len(ids) >= p.Limit:
			res.NextAfterID = ids[len(ids)-1]
		}
	}

	_ = db.QueryRow(ctx, jiraBackfillRemainingSQL, p.RetryFailed).Scan(&res.Remaining)

	log.Info().Int("matched", res.Matched).Int("enqueued", res.Enqueued).Int("remaining", res.Remaining).Msg("backfill_jira_metadata: done")
	return res
}
