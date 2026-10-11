-- +goose Up
CREATE TABLE graph.context_traces (
  id TEXT PRIMARY KEY,
  created_at TIMESTAMPTZ NOT NULL,
  route TEXT NOT NULL,
  build_rev TEXT NOT NULL,
  caller TEXT NOT NULL,
  status INTEGER NOT NULL,
  error TEXT,
  asker_input JSONB NOT NULL,
  acl_mode TEXT NOT NULL,
  request JSONB NOT NULL,
  items JSONB NOT NULL,
  items_truncated BOOLEAN NOT NULL DEFAULT FALSE,
  response_sha256 TEXT NOT NULL,
  latency_ms INTEGER NOT NULL
);
CREATE INDEX context_traces_created_at_idx ON graph.context_traces(created_at);

-- +goose Down
DROP TABLE graph.context_traces;
