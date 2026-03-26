-- +goose Up
-- Auto-generate sync_id for new rows so they're visible to sync engine.
ALTER TABLE observations ALTER COLUMN sync_id SET DEFAULT gen_random_uuid()::text;
ALTER TABLE session_summaries ALTER COLUMN sync_id SET DEFAULT gen_random_uuid()::text;
ALTER TABLE user_prompts ALTER COLUMN sync_id SET DEFAULT gen_random_uuid()::text;
ALTER TABLE sdk_sessions ALTER COLUMN sync_id SET DEFAULT gen_random_uuid()::text;

-- Backfill existing rows missing sync_id (use machine_id from settings as sync_source).
UPDATE observations SET sync_id = gen_random_uuid()::text,
  sync_source = COALESCE((SELECT value FROM settings WHERE key = 'machine_id'), 'unknown')
  WHERE sync_id IS NULL;
UPDATE session_summaries SET sync_id = gen_random_uuid()::text,
  sync_source = COALESCE((SELECT value FROM settings WHERE key = 'machine_id'), 'unknown')
  WHERE sync_id IS NULL;
UPDATE user_prompts SET sync_id = gen_random_uuid()::text,
  sync_source = COALESCE((SELECT value FROM settings WHERE key = 'machine_id'), 'unknown')
  WHERE sync_id IS NULL;
UPDATE sdk_sessions SET sync_id = gen_random_uuid()::text,
  sync_source = COALESCE((SELECT value FROM settings WHERE key = 'machine_id'), 'unknown')
  WHERE sync_id IS NULL;

-- +goose Down
ALTER TABLE observations ALTER COLUMN sync_id DROP DEFAULT;
ALTER TABLE session_summaries ALTER COLUMN sync_id DROP DEFAULT;
ALTER TABLE user_prompts ALTER COLUMN sync_id DROP DEFAULT;
ALTER TABLE sdk_sessions ALTER COLUMN sync_id DROP DEFAULT;
