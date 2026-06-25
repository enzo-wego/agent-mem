-- +goose Up
-- Slack profile real_name (e.g. "Ross Veitch"), used to bridge a Slack identity to
-- its BambooHR identity by exact full-name match when no shared email is available.
ALTER TABLE graph.slack_users ADD COLUMN IF NOT EXISTS real_name TEXT NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE graph.slack_users DROP COLUMN IF EXISTS real_name;
