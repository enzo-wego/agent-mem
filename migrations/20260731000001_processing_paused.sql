-- +goose Up
-- Seed the processing pause switch. Default "false" preserves current behavior.
--
-- When true, every graph dispatcher and the flat-memory pending-message loop
-- stop CLAIMING work, while the HTTP API stays up and keeps accepting ingest.
-- Jobs continue to be enqueued and simply queue up; flipping it back drains the
-- backlog. Checked per claim, so no restart is needed either way.
--
-- The point is to survive a spent LLM budget without losing data. Stopping the
-- worker also stops the API, and inbound Slack webhooks arrive from a separate
-- service that has nothing to retry into — so they are lost, not queued.
-- Pausing trades a growing queue for keeping every message.
INSERT INTO settings (key, value) VALUES ('processing_paused', 'false') ON CONFLICT (key) DO NOTHING;

-- +goose Down
DELETE FROM settings WHERE key = 'processing_paused';
