package handlers

import (
	"context"
	"time"

	"github.com/agent-mem/agent-mem/internal/graph/jobs"
)

// fetchableForCreatedAt are node types whose real created_at we can recover by
// fetching the source artifact (gh_pr from the PR's created_at, jira from the
// issue's created, …). Slack is excluded: its created_at is the message ts,
// which is in the node id and needs no fetch (see backfillSlackCreatedAt).
// cf/cf_page cover both confluence id schemes.
var fetchableForCreatedAt = []string{
	"jira", "gh_pr", "cf", "cf_page", "confluence", "pagerduty", "datadog", "sentry", "gws", "gws_doc",
}

// backfillSlackCreatedAt fills created_at for Slack messages that still lack
// one (rows ingested before created_at was set at ingest) from the ts segment
// of the id, slack:<channel>:<ts>. Pure SQL, no fetch. Returns rows updated.
func backfillSlackCreatedAt(ctx context.Context, deps Deps) (int64, error) {
	tag, err := deps.DB.Exec(ctx, `
UPDATE graph.nodes
SET created_at = to_timestamp(split_part(id, ':', 3)::numeric)
WHERE type = 'slack' AND created_at IS NULL
  AND split_part(id, ':', 3) ~ '^[0-9]+(\.[0-9]+)?$'`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// NewBackfillCreatedAtHandler returns the job entry for "backfill_created_at": it
// finds nodes still missing a real created_at (mostly referenced-but-never-fetched
// stubs) and enqueues fetch_body for each, which fetches the artifact and fills in
// created_at (plus title/body). One pass enqueues up to a bounded batch.
func NewBackfillCreatedAtHandler(deps Deps) jobs.Entry {
	return jobs.Entry{
		Handler:  backfillCreatedAtHandler(deps),
		PoolSize: 1,
		Lease:    300 * time.Second,
	}
}

func backfillCreatedAtHandler(deps Deps) jobs.Handler {
	return func(ctx context.Context, _ []byte) error {
		slackFilled, err := backfillSlackCreatedAt(ctx, deps)
		if err != nil {
			return err
		}

		rows, err := deps.DB.Query(ctx, `
SELECT id FROM graph.nodes
WHERE created_at IS NULL AND deleted_at IS NULL AND type = ANY($1)
ORDER BY first_seen_at DESC
LIMIT 1000`, fetchableForCreatedAt)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		enqueued := 0
		for _, id := range ids {
			// Dedup: skip if a fetch_body for this node is already queued/running.
			var exists bool
			_ = deps.DB.QueryRow(ctx, `
SELECT EXISTS(
  SELECT 1 FROM graph.jobs
  WHERE type='fetch_body' AND status IN ('queued','running') AND payload->>'node_id'=$1)`,
				id).Scan(&exists)
			if exists {
				continue
			}
			if _, e := jobs.Enqueue(ctx, deps.DB, "fetch_body", fetchBodyPayload{NodeID: id},
				jobs.EnqueueOptions{Priority: 7, MachineID: deps.MachineID}); e != nil {
				deps.Logger.Warn().Err(e).Str("node_id", id).Msg("backfill_created_at: enqueue fetch_body failed")
				continue
			}
			enqueued++
		}
		deps.Logger.Info().Int64("slack_filled", slackFilled).Int("candidates", len(ids)).Int("enqueued", enqueued).Msg("backfill_created_at: done")
		return nil
	}
}
