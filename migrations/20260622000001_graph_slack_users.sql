-- +goose Up
-- Slack user-id → display name, populated by the refresh_slack_users job from
-- Slack users.list. Used to render <@U…> mentions as readable names.
CREATE TABLE IF NOT EXISTS graph.slack_users (
  slack_user_id TEXT PRIMARY KEY,
  display_name  TEXT NOT NULL DEFAULT '',
  is_bot        BOOLEAN NOT NULL DEFAULT false,
  refreshed_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  machine_id    TEXT NOT NULL
);

-- +goose Down
DROP TABLE IF EXISTS graph.slack_users;
