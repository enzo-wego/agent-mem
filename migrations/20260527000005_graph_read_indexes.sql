-- +goose Up

-- Search needs to filter by type + author + time + scope.
CREATE INDEX IF NOT EXISTS idx_nodes_type_scope_updated
  ON graph.nodes(type, scope, updated_at DESC)
  WHERE deleted_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_nodes_author_updated
  ON graph.nodes(author_person_id, updated_at DESC)
  WHERE deleted_at IS NULL;

-- BFS traversal walks edges by source node, kind.
CREATE INDEX IF NOT EXISTS idx_edges_from_kind_to
  ON graph.edges(from_node_id, kind, to_node_id);
CREATE INDEX IF NOT EXISTS idx_edges_to_kind_from
  ON graph.edges(to_node_id, kind, from_node_id);

-- pgvector HNSW already created in Phase 1 (idx_artifact_index_embedding).
-- (Removed: a btree on (node_id) INCLUDE (embedding) is redundant — node_id is
-- the PK and vector search uses the HNSW index — and broken: a VECTOR(768) is
-- 3072B, exceeding the 2704B btree row limit. See 20260621000001.)

-- graph.member_scopes: denormalised ACL table — each row grants eeid access to scope.
-- Populated by refresh jobs (Slack: from slack_groups; Jira/GH/CF: from membership APIs).
CREATE TABLE IF NOT EXISTS graph.member_scopes (
  eeid  INTEGER NOT NULL,
  scope TEXT    NOT NULL,
  PRIMARY KEY (eeid, scope)
);
CREATE INDEX IF NOT EXISTS idx_member_scopes_eeid ON graph.member_scopes(eeid);

-- +goose Down
DROP TABLE IF EXISTS graph.member_scopes;
DROP INDEX IF EXISTS graph.idx_artifact_index_node_type;
DROP INDEX IF EXISTS graph.idx_edges_to_kind_from;
DROP INDEX IF EXISTS graph.idx_edges_from_kind_to;
DROP INDEX IF EXISTS graph.idx_nodes_author_updated;
DROP INDEX IF EXISTS graph.idx_nodes_type_scope_updated;
