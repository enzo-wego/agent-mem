-- +goose Up
-- Dedup ledger for the "watch channels" notifier: every message in a watched
-- channel group (e.g. Payment Partners) is DM'd once. One row per notified node.
CREATE TABLE IF NOT EXISTS graph.channel_notifications (
  node_id     text PRIMARY KEY,
  notified_at timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE IF EXISTS graph.channel_notifications;
