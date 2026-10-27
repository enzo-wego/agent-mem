-- +goose Up
ALTER TABLE graph.thread_summaries ADD COLUMN IF NOT EXISTS markets text[];

-- +goose Down
ALTER TABLE graph.thread_summaries DROP COLUMN IF EXISTS markets;
