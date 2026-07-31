-- +goose Up
-- Delete the Anthropic API key and model settings. Standing decision
-- (2026-08-01): the metered Anthropic API is never called from agent-mem again.
--
-- Why, concretely: graph summaries routed to claude-sonnet-5 through this key,
-- and a summarize_thread amplification bug pushed 1,335 calls/hour through it —
-- ~$11/hour, with no spend ceiling on the key to stop it. OpenRouter's $50 cap
-- at least failed closed; a raw API key just keeps billing.
--
-- Claude is reached only via llm-gateway, which authenticates with a Claude
-- subscription seat. A seat cannot run a bill up: it rate-limits instead of
-- charging. With no reader left in the Go code these rows are already inert;
-- deleting them means the secret is not sitting in the settings table either.
--
-- The key itself still has to be revoked in the Anthropic console — a migration
-- cannot do that, and until it is revoked the secret remains valid wherever it
-- was copied.
DELETE FROM settings WHERE key IN ('anthropic_api_key', 'anthropic_model');

-- +goose Down
-- Deliberately empty. Restoring these rows would recreate a metered billing path
-- and cannot restore the secret anyway. To route summaries to Claude again, wire
-- llm-gateway in — do not re-add a key setting.
SELECT 1;
