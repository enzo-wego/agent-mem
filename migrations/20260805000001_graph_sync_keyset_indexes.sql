-- +goose Up

-- Composite (timestamp, pk) indexes backing the sync pull keyset queries in
-- internal/database/graph_sync.go (the six GetGraph*ForPull). Each now walks
-- "WHERE (<timestamp>, <pk>) > ($2, $3) ORDER BY <timestamp> ASC, <pk> ASC",
-- a row-value comparison Postgres drives from these ascending btree indexes.
-- graph.nodes previously had only idx_nodes_updated_at (single-column, DESC),
-- which does not serve the composite ascending walk.
CREATE INDEX IF NOT EXISTS idx_nodes_sync_keyset            ON graph.nodes(updated_at, id);
CREATE INDEX IF NOT EXISTS idx_artifact_index_sync_keyset   ON graph.artifact_index(refreshed_at, node_id);
CREATE INDEX IF NOT EXISTS idx_artifact_bodies_sync_keyset  ON graph.artifact_bodies(fetched_at, node_id);
CREATE INDEX IF NOT EXISTS idx_slack_groups_sync_keyset     ON graph.slack_groups(refreshed_at, id);
CREATE INDEX IF NOT EXISTS idx_entities_sync_keyset         ON graph.entities(first_seen_at, id);
CREATE INDEX IF NOT EXISTS idx_affinity_sync_keyset         ON graph.user_affinity_config(updated_at, eeid);

-- +goose Down
DROP INDEX IF EXISTS graph.idx_affinity_sync_keyset;
DROP INDEX IF EXISTS graph.idx_entities_sync_keyset;
DROP INDEX IF EXISTS graph.idx_slack_groups_sync_keyset;
DROP INDEX IF EXISTS graph.idx_artifact_bodies_sync_keyset;
DROP INDEX IF EXISTS graph.idx_artifact_index_sync_keyset;
DROP INDEX IF EXISTS graph.idx_nodes_sync_keyset;
