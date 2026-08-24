-- +goose Up
-- Topic knowledge sources: a subscription can point at Confluence pages (whole
-- trees) and GitHub repos that DEFINE the topic. A Refresh reads + ingests them
-- and distills a scope_definition (used by the LLM relevance judge) plus a
-- human-readable scope_summary shown back to the user.
ALTER TABLE graph.topic_subscriptions
  ADD COLUMN IF NOT EXISTS sources           JSONB       NOT NULL DEFAULT '[]'::jsonb,  -- [{type:'confluence'|'github', url:'...'}]
  ADD COLUMN IF NOT EXISTS scope_definition  TEXT        NOT NULL DEFAULT '',           -- judge guidance (distilled)
  ADD COLUMN IF NOT EXISTS scope_summary     TEXT        NOT NULL DEFAULT '',           -- human-readable summary
  ADD COLUMN IF NOT EXISTS scope_status      TEXT        NOT NULL DEFAULT '',           -- '' | 'queued' | 'refreshing' | 'ready' | 'error'
  ADD COLUMN IF NOT EXISTS scope_refreshed_at TIMESTAMPTZ;

-- +goose Down
ALTER TABLE graph.topic_subscriptions
  DROP COLUMN IF EXISTS scope_refreshed_at,
  DROP COLUMN IF EXISTS scope_status,
  DROP COLUMN IF EXISTS scope_summary,
  DROP COLUMN IF EXISTS scope_definition,
  DROP COLUMN IF EXISTS sources;
