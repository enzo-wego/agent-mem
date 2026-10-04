package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/agent-mem/agent-mem/internal/graph/jobs"
)

const (
	backfillArtifactTSVDefaultBatch = 500
	backfillArtifactTSVMaxBatch     = 5000
	backfillArtifactTSVPause        = 2 * time.Second
)

type backfillArtifactTSVPayload struct {
	Batch int `json:"batch"`
}

// backfillArtifactTSVBatch fills tsv for up to batch rows that still have it
// NULL, in one statement (so the batch is atomic). The UPDATE takes ROW
// EXCLUSIVE on the table, which does not block reads or inserts.
func backfillArtifactTSVBatch(ctx context.Context, db jobs.DB, batch int) (int64, error) {
	tag, err := db.Exec(ctx, `
UPDATE graph.artifact_index
   SET tsv = graph.artifact_index_tsv_doc(summary, decisions_text, identifiers)
 WHERE node_id IN (SELECT node_id FROM graph.artifact_index WHERE tsv IS NULL ORDER BY node_id LIMIT $1)`, batch)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// NewBackfillArtifactTSVHandler returns the job entry for "backfill_artifact_tsv":
// fills graph.artifact_index.tsv for rows that predate the trigger. Payload
// {"batch": n} (default 500, 1-5000). It re-enqueues itself after a full batch
// and stops on the first short one. Duplicate chains are tolerated: every batch
// is idempotent over the same tsv IS NULL set.
func NewBackfillArtifactTSVHandler(deps Deps) jobs.Entry {
	return jobs.Entry{
		Handler: backfillArtifactTSVHandler(deps, func(ctx context.Context, payload []byte) error {
			_, err := jobs.EnqueueRaw(ctx, deps.DB, "backfill_artifact_tsv", payload, jobs.EnqueueOptions{
				AvailableAt:  time.Now().Add(backfillArtifactTSVPause),
				TargetRunner: deps.Runner,
				MachineID:    deps.MachineID,
			})
			return err
		}),
		PoolSize: 1,
		Lease:    300 * time.Second,
	}
}

// backfillArtifactTSVHandler takes the continuation enqueue as a seam.
func backfillArtifactTSVHandler(deps Deps, enqueue func(ctx context.Context, payload []byte) error) jobs.Handler {
	return func(ctx context.Context, raw []byte) error {
		// Absent batch defaults; an explicit 0 is out of range (ErrFatal below).
		p := backfillArtifactTSVPayload{Batch: backfillArtifactTSVDefaultBatch}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &p); err != nil {
				return fmt.Errorf("%w: backfill_artifact_tsv unmarshal: %v", jobs.ErrFatal, err)
			}
		}
		if p.Batch < 1 || p.Batch > backfillArtifactTSVMaxBatch {
			return fmt.Errorf("%w: backfill_artifact_tsv: batch must be 1-%d, got %d", jobs.ErrFatal, backfillArtifactTSVMaxBatch, p.Batch)
		}
		n, err := backfillArtifactTSVBatch(ctx, deps.DB, p.Batch)
		if err != nil {
			return fmt.Errorf("backfill_artifact_tsv: %w", err)
		}
		deps.Logger.Info().Int64("updated", n).Msg("backfill_artifact_tsv: batch")
		if n < int64(p.Batch) {
			deps.Logger.Info().Msg("backfill_artifact_tsv: done")
			return nil
		}
		next, _ := json.Marshal(p)
		if err := enqueue(ctx, next); err != nil {
			return fmt.Errorf("backfill_artifact_tsv: enqueue continuation: %w", err)
		}
		return nil
	}
}
