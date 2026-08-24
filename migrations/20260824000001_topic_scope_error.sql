-- +goose Up
ALTER TABLE graph.topic_subscriptions
  ADD COLUMN IF NOT EXISTS scope_error TEXT NOT NULL DEFAULT '';  -- last refresh's per-source failures

-- +goose Down
ALTER TABLE graph.topic_subscriptions
  DROP COLUMN IF EXISTS scope_error;
