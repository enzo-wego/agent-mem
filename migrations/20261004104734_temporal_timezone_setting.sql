-- +goose Up
-- IANA zone that time windows ("yesterday", "this month", date-only since/until)
-- are resolved in. Dashboard-editable (Settings → Time zone).
INSERT INTO settings (key, value) VALUES ('graph.temporal.timezone', 'Asia/Ho_Chi_Minh')
ON CONFLICT (key) DO NOTHING;

-- +goose Down
DELETE FROM settings WHERE key = 'graph.temporal.timezone';
