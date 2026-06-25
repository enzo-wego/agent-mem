-- +goose Up
-- Canonical creation/event time of the source artifact (Slack message posted-at,
-- Jira created, PR opened, …), as opposed to first_seen_at (when WE ingested it).
-- Nullable: NULL means "real date unknown yet" (e.g. a referenced-but-not-fetched
-- stub) and read paths fall back to first_seen_at. Populated at ingest from the
-- source event time and by fetchers/backfill from the fetched artifact.
ALTER TABLE graph.nodes ADD COLUMN IF NOT EXISTS created_at TIMESTAMPTZ;

-- Ordering index: newest-first over the effective time (created_at, else first_seen_at).
CREATE INDEX IF NOT EXISTS idx_nodes_effective_time
  ON graph.nodes ((COALESCE(created_at, first_seen_at)) DESC);

-- +goose Down
DROP INDEX IF EXISTS graph.idx_nodes_effective_time;
ALTER TABLE graph.nodes DROP COLUMN IF EXISTS created_at;
