-- +goose NO TRANSACTION
-- +goose Up
-- Lead-mode thread aggregate: max(updated_at) and bot presence per Slack thread.
CREATE INDEX CONCURRENTLY IF NOT EXISTS graph_nodes_thread_key_idx
  ON graph.nodes (scope, (COALESCE(NULLIF(metadata->>'thread_ts',''), split_part(id,':',3))))
  WHERE deleted_at IS NULL;

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS graph.graph_nodes_thread_key_idx;
