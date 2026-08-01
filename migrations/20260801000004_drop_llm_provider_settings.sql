-- +goose Up
-- Delete every provider key/model/rotation setting agent-mem no longer reads.
-- Standing decision (2026-08-01): agent-mem does memory logic and DB writes
-- only; all LLM egress goes through llm-gateway, which owns credentials, model
-- choice and failover. No reader is left in the Go code (internal/config lost
-- GeminiAPIKey, GeminiModel, GraphGeminiModel, GeminiEmbeddingModel, LLMProvider,
-- GoogleAPIKeys and LLMKeyRotateHours), so these rows are already inert; deleting
-- them means the OpenRouter secret is not sitting in the settings table either.
--
-- The OpenRouter key in gemini_api_key (…26ac) is preserved: it also lives in
-- llm-gateway's .env as OPENROUTER_API_KEY, confirmed before this deletion, so
-- dropping agent-mem's copy loses nothing.
--
-- Kept: gemini_embedding_dims (a property of THIS service's schema — the width
-- of observations.embedding), llm_gateway_url and llm_gateway_api_key (how the
-- gateway is reached).
DELETE FROM settings WHERE key IN (
    'gemini_api_key',
    'gemini_model',
    'graph_gemini_model',
    'gemini_embedding_model',
    'llm_provider',
    'google_api_keys',
    'llm_key_rotate_hours'
);

-- +goose Down
-- Deliberately empty. Restoring these rows would recreate a direct-provider path
-- (a key agent-mem would call OpenRouter/Google with) and cannot restore the
-- secret anyway. To change LLM routing, configure llm-gateway — never re-add a
-- provider key or model setting here.
SELECT 1;
