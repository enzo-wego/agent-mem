-- +goose NO TRANSACTION
-- +goose Up
-- Keyword search over node bodies (Jira descriptions and comments, Slack text).
-- Indexes only the first 20,000 characters: that is below the Jira p99 (22,253)
-- and below the Slack maximum (48,521). Terms after the cutoff are deliberately
-- not keyword-searchable; those documents still match through their summary and
-- embedding.
-- ponytail: first 20k chars only; raise N if eval shows body recall loss on long docs
-- A query must repeat this exact expression to use the index.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_nodes_body_tsv
  ON graph.nodes USING gin (to_tsvector('simple'::regconfig, left(coalesce(body, ''), 20000)));

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS graph.idx_nodes_body_tsv;
