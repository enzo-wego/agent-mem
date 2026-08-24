-- +goose Up
CREATE TABLE IF NOT EXISTS graph.eligibility_decisions (
  id            BIGSERIAL PRIMARY KEY,
  channel_id    TEXT NOT NULL,
  message_ts    TEXT NOT NULL,
  score         DOUBLE PRECISION NOT NULL,
  decision      TEXT NOT NULL CHECK (decision IN ('eligible', 'ineligible')),
  mode          TEXT NOT NULL CHECK (mode IN ('dry_run', 'enforce')),
  scope_version TIMESTAMPTZ NOT NULL,
  decided_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS eligibility_decisions_channel_decided_idx
  ON graph.eligibility_decisions (channel_id, decided_at DESC);

UPDATE graph.topic_subscriptions
SET scope_refreshed_at = created_at
WHERE scope_refreshed_at IS NULL AND BTRIM(scope_definition) <> '';

-- These thresholds are uncalibrated placeholders used only for dry-run data
-- collection. They are not recommendations; calibration must precede enforce.
INSERT INTO settings (key, value) VALUES (
  'graph.eligibility_gate',
  '{"enabled":true,"mode":"dry_run","scope_subscription_id":1,"high_threshold":0.62,"low_threshold":0.45,"llm_adjudicate":false,"gated_channels":[],"exempt_channels":["C0597404MS6","CUV9EAYGY","C05RNSE8TBR","C06Q3JHUAUV"]}'
) ON CONFLICT (key) DO NOTHING;

-- +goose Down
DELETE FROM settings WHERE key = 'graph.eligibility_gate';
DROP TABLE IF EXISTS graph.eligibility_decisions;
