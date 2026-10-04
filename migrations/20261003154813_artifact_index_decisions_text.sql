-- +goose Up
-- The decisions block index_artifact embeds with a Slack thread root's summary,
-- stored apart from summary so every summary reader is unchanged. NULL: not a
-- thread root, or not re-indexed since this column existed. '': a thread root
-- with no decisions. The hybrid keyword side matches it.
ALTER TABLE graph.artifact_index ADD COLUMN IF NOT EXISTS decisions_text TEXT;

-- +goose Down
ALTER TABLE graph.artifact_index DROP COLUMN IF EXISTS decisions_text;
