-- +goose NO TRANSACTION
-- +goose Up
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_jobs_describe_failed_node
  ON graph.jobs ((payload->>'node_id'))
  WHERE type = 'describe_attachment' AND status = 'failed';

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS graph.idx_jobs_describe_failed_node;
