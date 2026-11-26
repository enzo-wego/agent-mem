-- +goose NO TRANSACTION
-- +goose Up
-- Serves the enqueue dedupe checks: EXISTS (... WHERE type = $1 AND status IN
-- ('queued','running') ...). Without it each check seq-scans graph.jobs.
CREATE INDEX CONCURRENTLY IF NOT EXISTS graph_jobs_active_type_idx
  ON graph.jobs (type)
  WHERE status IN ('queued', 'running');

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS graph.graph_jobs_active_type_idx;
