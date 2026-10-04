-- +goose Up
-- refresh_jira_updates: poll Jira for recently updated issues and re-fetch
-- stale nodes. The cursor is seeded at the 2026-09-29 manual bulk repair.
INSERT INTO settings (key, value) VALUES
    ('jira_updates_enabled', 'true'),
    ('jira_updates_interval_minutes', '15'),
    ('jira_updates_cursor', '2026-09-29T04:00:00Z')
ON CONFLICT (key) DO NOTHING;

-- +goose Down
DELETE FROM settings WHERE key LIKE 'jira_updates\_%';
