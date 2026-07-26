-- +goose Up
-- Key pool + rotation for the google provider, and the block list that takes a
-- dead key out of rotation. Keys are never stored here — only a sha256
-- fingerprint (first 16 hex) and the last 4 chars for display; the keys
-- themselves stay in settings.google_api_keys.
INSERT INTO settings (key, value) VALUES ('google_api_keys', '') ON CONFLICT (key) DO NOTHING;
INSERT INTO settings (key, value) VALUES ('llm_key_rotate_hours', '6') ON CONFLICT (key) DO NOTHING;

CREATE TABLE IF NOT EXISTS llm_key_blocks (
    fingerprint TEXT PRIMARY KEY,
    key_tail    TEXT NOT NULL,
    provider    TEXT NOT NULL,
    reason      TEXT NOT NULL,
    blocked_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- NULL = permanent (key rejected); a timestamp = quota block that self-clears.
    expires_at  TIMESTAMPTZ
);

-- +goose Down
DROP TABLE IF EXISTS llm_key_blocks;
DELETE FROM settings WHERE key IN ('google_api_keys', 'llm_key_rotate_hours');
