-- +goose Up
-- Slack users whose messages are bot answers; read only by lead-mode graph reads.
CREATE TABLE IF NOT EXISTS graph.answer_bots (
  slack_user_id TEXT PRIMARY KEY,
  note          TEXT,
  created_at    TIMESTAMPTZ DEFAULT now()
);
INSERT INTO graph.answer_bots (slack_user_id, note) VALUES ('U0AG1DJP9K9', 'EnzoBot')
ON CONFLICT (slack_user_id) DO NOTHING;

-- +goose Down
DROP TABLE IF EXISTS graph.answer_bots;
