-- +goose Up
-- Pinned threads: user-bookmarked Slack threads surfaced in the /live 📌 PINS
-- panel with their latest activity. Single-user dashboard — no user column
-- (same trust model as the graph_continents settings blob).
CREATE TABLE IF NOT EXISTS graph.pinned_threads (
  channel_id TEXT NOT NULL,
  thread_ts  TEXT NOT NULL,
  pinned_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  PRIMARY KEY (channel_id, thread_ts)
);

-- +goose Down
DROP TABLE IF EXISTS graph.pinned_threads;
