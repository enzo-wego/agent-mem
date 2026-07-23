-- +goose Up
-- Seed the LLM provider toggle. Default "openrouter" preserves current behavior
-- (GeminiAPIKey holds the sk-or… key). To fail over to the direct Google Gemini
-- API: set google_api_key to an AIza… key, set llm_provider='google', restart
-- the worker. ON CONFLICT DO NOTHING so it never clobbers a hand-set value.
INSERT INTO settings (key, value) VALUES ('llm_provider', 'openrouter') ON CONFLICT (key) DO NOTHING;
INSERT INTO settings (key, value) VALUES ('google_api_key', '') ON CONFLICT (key) DO NOTHING;

-- +goose Down
DELETE FROM settings WHERE key IN ('llm_provider', 'google_api_key');
