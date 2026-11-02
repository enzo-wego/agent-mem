-- +goose Up
ALTER TABLE graph.thread_summaries ADD COLUMN IF NOT EXISTS human_signature text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE graph.thread_summaries DROP COLUMN IF EXISTS human_signature;
