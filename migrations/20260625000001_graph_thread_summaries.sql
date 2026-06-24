-- +goose Up
-- Caches one-line LLM topic summaries per Slack thread for the /live panel's
-- topic view. signature = "<msg_count>:<last_ts>" so a stale summary is
-- regenerated when the thread grows.
CREATE TABLE IF NOT EXISTS graph.thread_summaries (
  channel_id TEXT NOT NULL,
  thread_ts  TEXT NOT NULL,
  signature  TEXT NOT NULL,
  summary    TEXT NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  PRIMARY KEY (channel_id, thread_ts)
);

-- +goose Down
DROP TABLE IF EXISTS graph.thread_summaries;
