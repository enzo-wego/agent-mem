-- +goose Up
-- Auto-set sync_source from machine_id in settings when NULL on insert.

CREATE OR REPLACE FUNCTION set_sync_source() RETURNS TRIGGER AS $$
BEGIN
    IF NEW.sync_source IS NULL THEN
        NEW.sync_source := COALESCE(
            (SELECT value FROM settings WHERE key = 'machine_id'),
            'unknown'
        );
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_observations_sync_source
    BEFORE INSERT ON observations
    FOR EACH ROW EXECUTE FUNCTION set_sync_source();

CREATE TRIGGER trg_session_summaries_sync_source
    BEFORE INSERT ON session_summaries
    FOR EACH ROW EXECUTE FUNCTION set_sync_source();

CREATE TRIGGER trg_user_prompts_sync_source
    BEFORE INSERT ON user_prompts
    FOR EACH ROW EXECUTE FUNCTION set_sync_source();

CREATE TRIGGER trg_sdk_sessions_sync_source
    BEFORE INSERT ON sdk_sessions
    FOR EACH ROW EXECUTE FUNCTION set_sync_source();

-- Backfill any rows still missing sync_source.
UPDATE observations SET sync_source = COALESCE((SELECT value FROM settings WHERE key = 'machine_id'), 'unknown') WHERE sync_source IS NULL;
UPDATE session_summaries SET sync_source = COALESCE((SELECT value FROM settings WHERE key = 'machine_id'), 'unknown') WHERE sync_source IS NULL;
UPDATE user_prompts SET sync_source = COALESCE((SELECT value FROM settings WHERE key = 'machine_id'), 'unknown') WHERE sync_source IS NULL;
UPDATE sdk_sessions SET sync_source = COALESCE((SELECT value FROM settings WHERE key = 'machine_id'), 'unknown') WHERE sync_source IS NULL;

-- +goose Down
DROP TRIGGER IF EXISTS trg_observations_sync_source ON observations;
DROP TRIGGER IF EXISTS trg_session_summaries_sync_source ON session_summaries;
DROP TRIGGER IF EXISTS trg_user_prompts_sync_source ON user_prompts;
DROP TRIGGER IF EXISTS trg_sdk_sessions_sync_source ON sdk_sessions;
DROP FUNCTION IF EXISTS set_sync_source();
