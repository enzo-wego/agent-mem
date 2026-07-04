-- +goose Up

-- Slack nodes hydrated through fetch_body got scope='slack' (bare source name)
-- because the slack fetcher returned no channel_id metadata for deriveScope.
-- A bare scope breaks the channel-name join, the thread_summaries join, ACL
-- scoping, and leaked DM threads past the "slack:D%" exclusion in SIMILAR.
-- The fetcher now sets channel_id/thread_ts; repair existing rows from the
-- node id, which embeds the channel ("slack:<channel>:<ts>").
UPDATE graph.nodes
SET scope = 'slack:' || split_part(id, ':', 2), updated_at = NOW()
WHERE type IN ('slack', 'slack_thread')
  AND scope = 'slack'
  AND split_part(id, ':', 2) ~ '^[CDG]';

-- +goose Down
-- Intentionally irreversible — the bare scope was a defect, not a state.
