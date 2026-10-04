-- +goose NO TRANSACTION
-- +goose Up
-- Scoped reads (search/resolve/neighbors `epic=` filter) look up members of one
-- epic, newest first.
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_epic_membership_epic_last
  ON graph.epic_membership (epic_key, last_at DESC);

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS graph.idx_epic_membership_epic_last;
