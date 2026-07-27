-- +goose Up
-- The single google_api_key was only ever the fallback for an empty pool, i.e. a
-- pool of one. Fold it into google_api_keys (only when the pool is empty, so a
-- configured pool is never touched) and drop the setting.
UPDATE settings SET value = (SELECT value FROM settings WHERE key = 'google_api_key')
WHERE key = 'google_api_keys'
  AND btrim(value) = ''
  AND btrim(coalesce((SELECT value FROM settings WHERE key = 'google_api_key'), '')) <> '';

DELETE FROM settings WHERE key = 'google_api_key';

-- +goose Down
-- Restore the row empty: the key itself now lives in the pool, and copying the
-- pool's first entry back out would just duplicate it.
INSERT INTO settings (key, value) VALUES ('google_api_key', '') ON CONFLICT (key) DO NOTHING;
