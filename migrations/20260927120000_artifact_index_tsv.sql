-- +goose Up
-- Keyword arm of /api/graph/search. 'simple' config on purpose: identifiers
-- and ticket keys (PAY-2307, ORD-…) must not be stemmed. Titles live on
-- graph.nodes and join in the query rather than here, so this stays a
-- single-table generated column.
--
-- array_to_string is only STABLE (element output can depend on session
-- settings), which a generated column rejects; for text[] it is in fact
-- deterministic, so wrap it in an IMMUTABLE helper.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION graph.identifiers_text(ids text[]) RETURNS text
LANGUAGE sql IMMUTABLE PARALLEL SAFE STRICT AS $$
  SELECT array_to_string(ids, ' ')
$$;
-- +goose StatementEnd

-- WARNING: the ALTER below rewrites graph.artifact_index and rebuilds its HNSW
-- index under ACCESS EXCLUSIVE. lock_timeout only limits the WAIT for that
-- lock (the migration fails instead of queueing behind a long-running query
-- and blocking every reader); it does not bound the rewrite itself. Run this
-- migration in a quiet window.
SET lock_timeout = '5s';
ALTER TABLE graph.artifact_index
  ADD COLUMN IF NOT EXISTS tsv tsvector
  GENERATED ALWAYS AS (
    to_tsvector('simple'::regconfig,
                coalesce(summary, '') || ' ' || graph.identifiers_text(identifiers))
  ) STORED;
RESET lock_timeout;

-- +goose Down
SET lock_timeout = '5s';
ALTER TABLE graph.artifact_index DROP COLUMN IF EXISTS tsv;
RESET lock_timeout;
DROP FUNCTION IF EXISTS graph.identifiers_text(text[]);
