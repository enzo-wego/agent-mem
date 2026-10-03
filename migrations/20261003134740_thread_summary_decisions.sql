-- +goose Up
-- Decisions and open questions extracted by summarize_thread (agent-mem-8689).
-- Both are NULL for rows summarized before this column existed. The handler
-- always writes a JSON array ('[]' when there are none), so NULL means "not
-- yet extracted". The targeted backfill selects on decisions IS NULL, which is
-- what makes it fill a row at most once. Adding the columns re-summarizes
-- nothing: the signature version stays v9.
ALTER TABLE graph.thread_summaries ADD COLUMN IF NOT EXISTS decisions JSONB;
ALTER TABLE graph.thread_summaries ADD COLUMN IF NOT EXISTS open_questions JSONB;

-- +goose Down
ALTER TABLE graph.thread_summaries DROP COLUMN IF EXISTS open_questions;
ALTER TABLE graph.thread_summaries DROP COLUMN IF EXISTS decisions;
