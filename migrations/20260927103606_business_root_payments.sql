-- +goose Up
-- The Payments business root: every board epic hangs off it via PART_OF, and
-- graph.epic_membership uses epic_key='business:payments' for the "everything
-- payments" tier. machine_id is a stable literal so the row is identical on
-- every instance (it is seeded, not synced).
INSERT INTO graph.nodes (id, type, natural_key, title, scope, machine_id)
VALUES ('business:payments', 'business', 'payments', 'Payments', 'jira', 'migration')
ON CONFLICT (id) DO NOTHING;

-- Jira project whose epics form the business root's subtree; read by
-- refresh_jira_board. Dashboard-editable (Settings → Business root).
INSERT INTO settings (key, value) VALUES ('graph.business_root_project', 'PAY')
ON CONFLICT (key) DO NOTHING;

-- +goose Down
DELETE FROM settings WHERE key = 'graph.business_root_project';
DELETE FROM graph.nodes WHERE id = 'business:payments';
