-- +goose Up
-- Seed per-channel ingest filters (graph.channel_filters), read at the pre-LLM
-- chokepoint in ingest_content.go. Editable live via SQL; ON CONFLICT DO NOTHING
-- so this only sets the initial default and never clobbers hand-tuned config.
--
--   ignore        drop every message (staging / private / off-domain alert noise)
--   incident_only keep only messages from the named authors (PagerDuty incidents)
--   keep_regex    keep only messages whose body matches (topic allow-list)
--   drop_regex    drop matches even if kept (routine success heartbeats)
--
-- Channels: C01T60D80JV payments-alerts-staging, C0A7D29E5ED alerts-itops-tech-and-ai-news,
-- C0B1BR522F5 payments-staging, C0AJ3JPRA9L enzo-private, C02AD7A21UH disputes-flights-production,
-- C029TRHS5HU disputes-hotels-production, C08S954G2LX payments-alerts, CPP5EH3A8 task-alerts-production.
INSERT INTO settings (key, value) VALUES (
  'graph.channel_filters',
  '{"ignore":["C01T60D80JV","C0A7D29E5ED","C0B1BR522F5","C0AJ3JPRA9L","C02AD7A21UH","C029TRHS5HU"],"incident_only":{"C08S954G2LX":["PagerDuty"]},"keep_regex":{"CPP5EH3A8":"(?i)pending.?payment|process[- ]?taxes"},"drop_regex":{"CPP5EH3A8":"(?i)white_check_mark[\\s\\S]*->\\s*200"}}'
) ON CONFLICT (key) DO NOTHING;

-- +goose Down
DELETE FROM settings WHERE key = 'graph.channel_filters';
