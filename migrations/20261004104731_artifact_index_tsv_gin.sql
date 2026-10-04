-- +goose NO TRANSACTION
-- +goose Up
-- Keyword arm: websearch_to_tsquery('simple', q) @@ tsv.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_artifact_index_tsv
  ON graph.artifact_index USING gin (tsv);

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS graph.idx_artifact_index_tsv;
