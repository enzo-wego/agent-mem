-- +goose Up
-- Cached LLM cluster synthesis for the "open in Graph" overlay, keyed by the root
-- node. signature captures the cluster's content (node + message counts + latest
-- message ts) so the summary is reused verbatim across clicks/sessions and only
-- regenerated when the underlying discussion actually changes.
CREATE TABLE IF NOT EXISTS graph.cluster_summaries (
  node_id    TEXT PRIMARY KEY,
  signature  TEXT NOT NULL,
  overview   TEXT NOT NULL DEFAULT '',
  highlights JSONB NOT NULL DEFAULT '[]'::jsonb,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- +goose Down
DROP TABLE IF EXISTS graph.cluster_summaries;
