-- +goose Up
CREATE TABLE IF NOT EXISTS graph.subject_queries (
  node_id    TEXT PRIMARY KEY,
  signature  TEXT NOT NULL,
  queries    JSONB NOT NULL DEFAULT '[]'::jsonb,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- +goose Down
DROP TABLE IF EXISTS graph.subject_queries;
