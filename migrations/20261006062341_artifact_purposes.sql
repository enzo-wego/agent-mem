-- +goose Up
-- One model-written sentence per Jira ticket, Confluence page or PR saying what
-- it is for (summarize_purpose job). Display-only: never touches
-- artifact_index.summary, embeddings or ranking.
--   signature        input the stored purpose was made from ("v1:" + sha256)
--   failed_signature last input whose model output was invalid (not re-sent)
-- Per-instance like graph.epic_briefs — not part of cloud/local sync.
CREATE TABLE IF NOT EXISTS graph.artifact_purposes (
  node_id          TEXT PRIMARY KEY REFERENCES graph.nodes(id) ON DELETE CASCADE,
  purpose          TEXT NOT NULL DEFAULT '',
  signature        TEXT NOT NULL DEFAULT '',
  failed_signature TEXT NOT NULL DEFAULT '',
  updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE IF EXISTS graph.artifact_purposes;
