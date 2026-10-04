package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"

	"github.com/rs/zerolog"

	"github.com/agent-mem/agent-mem/internal/graph/jobs"
)

var reJiraProjectKey = regexp.MustCompile(`^[A-Z][A-Z0-9]+$`)

// backfillSubtreeIndexRequest is the request body for POST
// /api/graph/backfill/subtree-index. Body is optional.
type backfillSubtreeIndexRequest struct {
	// Project is the Jira project key whose subtree to index (default PAY).
	Project string `json:"project"`
	// Limit caps the nodes enqueued in this call (default 500, max 2000).
	Limit int `json:"limit"`
}

// backfillSubtreeIndexResponse is the response body.
type backfillSubtreeIndexResponse struct {
	Status    string `json:"status"`
	Project   string `json:"project"`
	Matched   int    `json:"matched"`
	Enqueued  int    `json:"enqueued"`
	Remaining int    `json:"remaining"`
	Limit     int    `json:"limit"`
}

const (
	backfillSubtreeIndexDefaultLimit = 500
	backfillSubtreeIndexMaxLimit     = 2000
	backfillSubtreeIndexPriority     = 3
)

// NewBackfillSubtreeIndexHandler returns an http.Handler for POST
// /api/graph/backfill/subtree-index. It enqueues index_artifact at priority 3
// (ahead of the priority-5 organic stream) for the nodes of one Jira project's
// subtree that have no embedding yet (round 0.3): every jira:<PROJECT>-* node,
// every node with a REFERENCES edge to one (Slack messages, PRs, docs), and
// every Slack thread root with a substantive thread summary. It is NOT an
// "embed everything" run: the remaining one-line Slack replies without an
// embedding are left alone. index_artifact is an LLM job, so the run is paced
// by llm_hourly_call_cap. Explicitly triggered, capped, deduped, and
// deliberately NOT wired into worker startup or any scheduled job.
func NewBackfillSubtreeIndexHandler(deps Deps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req backfillSubtreeIndexRequest
		// Optional body: ignore decode errors (empty/absent body -> defaults).
		_ = json.NewDecoder(r.Body).Decode(&req)

		project := req.Project
		if project == "" {
			project = "PAY"
		}
		if !reJiraProjectKey.MatchString(project) {
			writeError(w, http.StatusBadRequest, "project must be a Jira project key like PAY")
			return
		}
		limit := req.Limit
		if limit <= 0 {
			limit = backfillSubtreeIndexDefaultLimit
		}
		if limit > backfillSubtreeIndexMaxLimit {
			writeError(w, http.StatusBadRequest, "limit must be between 1 and 2000")
			return
		}

		matched, enqueued, remaining := BackfillSubtreeIndex(r.Context(), deps.DB, deps.Logger, deps.MachineID, project, limit)
		writeJSON(w, http.StatusAccepted, backfillSubtreeIndexResponse{
			Status:    "ok",
			Project:   project,
			Matched:   matched,
			Enqueued:  enqueued,
			Remaining: remaining,
			Limit:     limit,
		})
	})
}

// subtreeIndexCandidatesSQL selects subtree nodes that index_artifact can
// still make progress on, newest first, excluding those with an
// index_artifact job already queued/running. Backfill policy:
//  1. nodes with an empty body are skipped (index_artifact writes no row);
//  2. a non-empty-body node with no artifact_index row is a candidate;
//  3. a row with a NULL embedding is a candidate only when it is not
//     heuristic (a NULL embedding on a heuristic row is index_artifact's
//     deliberate shared-representative dedup, not a gap);
//  4. a heuristic Slack thread root is a candidate only when its thread
//     summary is newer than the index row, i.e. re-indexing can upgrade it.
//
// $1 = project key, $2 = limit (NULL for the remaining count).
const subtreeIndexCandidatesSQL = `
WITH issues AS (
  SELECT id FROM graph.nodes
  WHERE type = 'jira' AND deleted_at IS NULL AND id LIKE 'jira:' || $1 || '-%'
),
referrers AS (
  SELECT DISTINCT e.from_node_id AS id
  FROM graph.edges e JOIN issues i ON i.id = e.to_node_id
  WHERE e.kind = 'REFERENCES'
),
roots AS (
  SELECT n.id, ts.updated_at AS ts_updated_at
  FROM graph.nodes n
  JOIN graph.thread_summaries ts
    ON ts.channel_id = REPLACE(n.scope, 'slack:', '')
   AND ts.thread_ts = split_part(n.id, ':', 3)
  WHERE n.type = 'slack'
    AND COALESCE(NULLIF(n.metadata->>'thread_ts', ''), split_part(n.id, ':', 3)) = split_part(n.id, ':', 3)
    AND COALESCE(ts.kind, '') <> 'chatter'
    AND length(COALESCE(ts.summary, '')) > 0
),
cands AS (
  SELECT id FROM issues UNION SELECT id FROM referrers UNION SELECT id FROM roots
)
SELECT c.id
FROM cands c
JOIN graph.nodes n ON n.id = c.id AND n.deleted_at IS NULL
LEFT JOIN graph.artifact_bodies ab ON ab.node_id = c.id
LEFT JOIN graph.artifact_index ai ON ai.node_id = c.id
LEFT JOIN roots r ON r.id = c.id
WHERE COALESCE(ab.body_full, n.body, '') <> ''
  AND (ai.node_id IS NULL
       OR (ai.embedding IS NULL AND ai.summary_kind <> 'heuristic')
       OR (ai.summary_kind = 'heuristic' AND r.id IS NOT NULL AND r.ts_updated_at > ai.refreshed_at))
  AND NOT EXISTS (
    SELECT 1 FROM graph.jobs j
    WHERE j.type = 'index_artifact' AND j.status IN ('queued','running') AND j.payload->>'node_id' = c.id)
ORDER BY COALESCE(n.created_at, n.first_seen_at) DESC, c.id
LIMIT $2`

// BackfillSubtreeIndex enqueues index_artifact (priority 3) for up to limit
// nodes of project's subtree that have no embedding. Returns candidates seen,
// jobs enqueued, and the count still un-indexed after this page.
func BackfillSubtreeIndex(ctx context.Context, db jobs.DB, log zerolog.Logger, machineID, project string, limit int) (matched, enqueued, remaining int) {
	rows, err := db.Query(ctx, subtreeIndexCandidatesSQL, project, limit)
	if err != nil {
		log.Warn().Err(err).Msg("backfill_subtree_index: select failed")
		return 0, 0, 0
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			log.Warn().Err(err).Msg("backfill_subtree_index: scan failed")
			return 0, 0, 0
		}
		ids = append(ids, id)
	}
	rows.Close()
	matched = len(ids)

	for _, id := range ids {
		// force: the 24h freshness guard would otherwise return early for a
		// recently refreshed row whose embedding is still missing.
		if _, e := jobs.Enqueue(ctx, db, "index_artifact", map[string]any{
			"node_id": id,
			"force":   true,
		}, jobs.EnqueueOptions{
			Priority:  backfillSubtreeIndexPriority,
			MachineID: machineID,
		}); e != nil {
			log.Warn().Err(e).Str("node_id", id).Msg("backfill_subtree_index: enqueue index_artifact failed")
			continue
		}
		enqueued++
	}

	_ = db.QueryRow(ctx, `SELECT count(*) FROM (`+subtreeIndexCandidatesSQL+`) c`, project, nil).Scan(&remaining)

	log.Info().Str("project", project).Int("matched", matched).Int("enqueued", enqueued).Int("remaining", remaining).Msg("backfill_subtree_index: done")
	return matched, enqueued, remaining
}
