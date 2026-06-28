-- +goose Up
-- Deep per-thread summary: in addition to the one-line topic (summary), store a
-- 2-3 sentence overview + chronological highlights so a thread can be understood
-- quickly and deeply without opening the cross-resource cluster view.
ALTER TABLE graph.thread_summaries ADD COLUMN IF NOT EXISTS overview   TEXT  NOT NULL DEFAULT '';
ALTER TABLE graph.thread_summaries ADD COLUMN IF NOT EXISTS highlights JSONB NOT NULL DEFAULT '[]'::jsonb;

-- +goose Down
ALTER TABLE graph.thread_summaries DROP COLUMN IF EXISTS highlights;
ALTER TABLE graph.thread_summaries DROP COLUMN IF EXISTS overview;
