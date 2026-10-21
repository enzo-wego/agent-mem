-- +goose NO TRANSACTION
-- +goose Up
-- Serves "who is on team X"; the primary key serves "which teams is person P on".
CREATE INDEX CONCURRENTLY IF NOT EXISTS graph_person_teams_team_idx
  ON graph.person_teams (team_node_id);

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS graph.graph_person_teams_team_idx;
