-- +goose Up
-- Exact-topic linking v1 intentionally rebuilds graph embeddings from scratch:
-- graph uses 3072-dimensional halfvec embeddings while core memory tables keep
-- their existing vector(768) columns.

DROP INDEX IF EXISTS graph.idx_artifact_index_embedding;

TRUNCATE graph.artifact_index, graph.artifact_bodies, graph.thread_summaries;

ALTER TABLE graph.artifact_index
  ALTER COLUMN embedding TYPE halfvec(3072) USING embedding::halfvec(3072);

CREATE INDEX idx_artifact_index_embedding ON graph.artifact_index
  USING hnsw (embedding halfvec_cosine_ops);

ALTER TABLE graph.edges ADD COLUMN IF NOT EXISTS metadata JSONB NOT NULL DEFAULT '{}'::jsonb;

CREATE TABLE IF NOT EXISTS graph.alert_fingerprints (
  channel_id          TEXT NOT NULL,
  fingerprint         TEXT NOT NULL,
  representative_text TEXT NOT NULL DEFAULT '',
  count_total         INTEGER NOT NULL DEFAULT 0,
  first_seen_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  last_seen_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  last_escalated_at   TIMESTAMPTZ,
  machine_id          TEXT NOT NULL,
  PRIMARY KEY (channel_id, fingerprint)
);
CREATE INDEX IF NOT EXISTS idx_alert_fingerprints_last_seen
  ON graph.alert_fingerprints(last_seen_at DESC);

CREATE TABLE IF NOT EXISTS graph.alert_fingerprint_events (
  id          BIGSERIAL PRIMARY KEY,
  channel_id  TEXT NOT NULL,
  fingerprint TEXT NOT NULL,
  seen_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  machine_id  TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_alert_fingerprint_events_recent
  ON graph.alert_fingerprint_events(channel_id, fingerprint, seen_at DESC);

CREATE TABLE IF NOT EXISTS graph.topic_link_judgments (
  source_node_id TEXT NOT NULL REFERENCES graph.nodes(id) ON DELETE CASCADE,
  target_node_id TEXT NOT NULL REFERENCES graph.nodes(id) ON DELETE CASCADE,
  content_hash   TEXT NOT NULL,
  same_topic     BOOLEAN NOT NULL,
  confidence     DOUBLE PRECISION NOT NULL DEFAULT 0,
  topic          TEXT NOT NULL DEFAULT '',
  why            TEXT NOT NULL DEFAULT '',
  judged_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  machine_id     TEXT NOT NULL,
  PRIMARY KEY (source_node_id, target_node_id)
);
CREATE INDEX IF NOT EXISTS idx_topic_link_judgments_hash
  ON graph.topic_link_judgments(content_hash);

-- +goose Down
DROP TABLE IF EXISTS graph.topic_link_judgments;
DROP TABLE IF EXISTS graph.alert_fingerprint_events;
DROP TABLE IF EXISTS graph.alert_fingerprints;

ALTER TABLE graph.edges
  DROP COLUMN IF EXISTS metadata;

DROP INDEX IF EXISTS graph.idx_artifact_index_embedding;

TRUNCATE graph.artifact_index, graph.artifact_bodies, graph.thread_summaries;

ALTER TABLE graph.artifact_index
  ALTER COLUMN embedding TYPE vector(768) USING embedding::vector(768);

CREATE INDEX idx_artifact_index_embedding ON graph.artifact_index
  USING hnsw (embedding vector_cosine_ops);
