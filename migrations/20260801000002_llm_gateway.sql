-- +goose Up
-- Wire graph summaries to llm-gateway (Claude on a subscription seat).
--
-- Empty URL is the off switch: with no URL the worker falls back to the graph
-- Gemini client, which is exactly today's behaviour. So this migration changes
-- nothing on its own — set llm_gateway_url from the dashboard (Settings →
-- Claude via llm-gateway) to turn it on, clear it to turn it off. Both take
-- effect on the next settings save; no restart.
--
-- Deliberately seeded empty rather than pre-filled with the VPS URL: a migration
-- that silently starts routing summaries to a new backend on deploy is the kind
-- of change nobody remembers making. Turning it on stays an explicit act.
--
-- The URL is the container→host bridge, not localhost: the worker runs in Docker
-- and the gateway binds 172.18.0.1 (the agent-mem_default bridge, NOT docker0).
-- ufw must also allow that subnet to the gateway port — it drops container→host
-- traffic with no error logged anywhere, which cost an afternoon to find once.
INSERT INTO settings (key, value) VALUES
  ('llm_gateway_url', ''),
  ('llm_gateway_api_key', '')
ON CONFLICT (key) DO NOTHING;

-- +goose Down
DELETE FROM settings WHERE key IN ('llm_gateway_url', 'llm_gateway_api_key');
