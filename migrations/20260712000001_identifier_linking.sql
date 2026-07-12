-- +goose Up
-- Plan 15 (C1): concrete identifiers (payment refs, Jira keys, PR refs,
-- request UUIDs) extracted from RAW thread/body text — not summaries, which
-- drop them. Shared rare identifiers become topic-link candidates that the
-- embedding shortlist structurally cannot recall (verified case: pxx6xgkdtl,
-- cosine 0.679 vs cutoff 0.724, rank #128).
ALTER TABLE graph.artifact_index
  ADD COLUMN IF NOT EXISTS identifiers TEXT[] NOT NULL DEFAULT '{}';
CREATE INDEX IF NOT EXISTS idx_artifact_index_identifiers
  ON graph.artifact_index USING gin (identifiers);

-- +goose Down
DROP INDEX IF EXISTS graph.idx_artifact_index_identifiers;
ALTER TABLE graph.artifact_index DROP COLUMN IF EXISTS identifiers;
