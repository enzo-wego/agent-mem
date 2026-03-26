-- +goose Up
-- Allow 'change' observation type (exists in claude-mem SQLite data)
-- and drop UNIQUE on session_summaries.memory_session_id (SQLite allows multiple per session)

-- Drop and recreate observations CHECK constraint to include 'change'
ALTER TABLE observations DROP CONSTRAINT IF EXISTS observations_type_check;
ALTER TABLE observations ADD CONSTRAINT observations_type_check
    CHECK (type IN ('decision', 'bugfix', 'feature', 'refactor', 'discovery', 'change'));

-- Drop UNIQUE on session_summaries.memory_session_id
-- (claude-mem creates multiple summaries per session, one per prompt batch)
ALTER TABLE session_summaries DROP CONSTRAINT IF EXISTS session_summaries_memory_session_id_key;

-- Unique index on (content_session_id, prompt_number) for idempotent prompt migration
CREATE UNIQUE INDEX IF NOT EXISTS idx_user_prompts_dedup
    ON user_prompts (content_session_id, prompt_number);

-- +goose Down
ALTER TABLE observations DROP CONSTRAINT IF EXISTS observations_type_check;
ALTER TABLE observations ADD CONSTRAINT observations_type_check
    CHECK (type IN ('decision', 'bugfix', 'feature', 'refactor', 'discovery'));

ALTER TABLE session_summaries ADD CONSTRAINT session_summaries_memory_session_id_key
    UNIQUE (memory_session_id);
