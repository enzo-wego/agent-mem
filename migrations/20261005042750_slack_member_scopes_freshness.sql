-- +goose Up
ALTER TABLE graph.member_scopes ADD COLUMN IF NOT EXISTS refreshed_at timestamptz NOT NULL DEFAULT now();
UPDATE graph.member_scopes SET refreshed_at = 'epoch'
WHERE scope LIKE 'slack:C%' OR scope LIKE 'slack:G%';

-- +goose Down
DELETE FROM graph.member_scopes WHERE scope LIKE 'slack:C%' OR scope LIKE 'slack:G%';
ALTER TABLE graph.member_scopes DROP COLUMN refreshed_at;
