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

ALTER TABLE graph.artifact_index
  ADD COLUMN IF NOT EXISTS tsv tsvector
  GENERATED ALWAYS AS (
    to_tsvector('simple'::regconfig,
                coalesce(summary, '') || ' ' || graph.identifiers_text(identifiers))
  ) STORED;

-- +goose Down
ALTER TABLE graph.artifact_index DROP COLUMN IF EXISTS tsv;
DROP FUNCTION IF EXISTS graph.identifiers_text(text[]);
