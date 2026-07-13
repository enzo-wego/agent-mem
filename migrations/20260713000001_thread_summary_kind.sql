-- +goose Up
-- Thread kind from the summarizer: 'substantive' | 'chatter' (leave/on-call
-- notices, greetings, acks — threads with no work content). Chatter is hidden
-- from the topics panel and never topic-links. '' = classified before this
-- field existed; treated as substantive.
ALTER TABLE graph.thread_summaries ADD COLUMN IF NOT EXISTS kind TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE graph.thread_summaries DROP COLUMN IF EXISTS kind;
