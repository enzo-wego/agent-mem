-- +goose Up
-- Slack channel-id → name, populated by the refresh_slack_channels job from
-- Slack conversations.list. Used so the map shows channel names instead of raw
-- ids for channels not in the curated continents config.
CREATE TABLE IF NOT EXISTS graph.slack_channels (
  slack_channel_id TEXT PRIMARY KEY,
  name             TEXT NOT NULL DEFAULT '',
  refreshed_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  machine_id       TEXT NOT NULL
);

-- +goose Down
DROP TABLE IF EXISTS graph.slack_channels;
