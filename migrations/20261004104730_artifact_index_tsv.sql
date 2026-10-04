-- +goose Up
SET LOCAL lock_timeout = '5s';

-- Keyword arm of /api/graph/search. 'simple' config on purpose: identifiers
-- and ticket keys (PAY-2307, ORD-…) must not be stemmed.
--
-- tsv is a plain nullable column kept current by a trigger, NOT a generated
-- column: ADD COLUMN without a default is a catalog-only change, so
-- graph.artifact_index is not rewritten (a STORED generated column would hold
-- ACCESS EXCLUSIVE for 1-2 minutes on the hub).
--
-- Existing rows keep tsv NULL until the backfill_artifact_tsv job (R2.3) fills
-- them; the keyword arm does not see those rows until then. The rollout
-- readiness gate (enqueue backfill_artifact_tsv, wait for zero NULL rows) must
-- run before search depends on tsv.
--
-- array_to_string is only STABLE, so wrap it in an IMMUTABLE helper.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION graph.identifiers_text(ids text[]) RETURNS text
LANGUAGE sql IMMUTABLE PARALLEL SAFE STRICT AS $$
  SELECT array_to_string(ids, ' ')
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION graph.artifact_index_tsv_doc(summary text, decisions_text text, identifiers text[]) RETURNS tsvector
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
  SELECT to_tsvector('simple'::regconfig,
    coalesce(summary,'') || ' ' || coalesce(decisions_text,'') || ' ' || coalesce(graph.identifiers_text(identifiers),''))
$$;
-- +goose StatementEnd

ALTER TABLE graph.artifact_index ADD COLUMN IF NOT EXISTS tsv tsvector;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION graph.artifact_index_tsv_trg() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  NEW.tsv := graph.artifact_index_tsv_doc(NEW.summary, NEW.decisions_text, NEW.identifiers);
  RETURN NEW;
END
$$;
-- +goose StatementEnd

DROP TRIGGER IF EXISTS artifact_index_tsv_trg ON graph.artifact_index;
CREATE TRIGGER artifact_index_tsv_trg
  BEFORE INSERT OR UPDATE OF summary, decisions_text, identifiers ON graph.artifact_index
  FOR EACH ROW EXECUTE FUNCTION graph.artifact_index_tsv_trg();

-- +goose Down
SET LOCAL lock_timeout = '5s';
DROP TRIGGER IF EXISTS artifact_index_tsv_trg ON graph.artifact_index;
DROP FUNCTION IF EXISTS graph.artifact_index_tsv_trg();
ALTER TABLE graph.artifact_index DROP COLUMN IF EXISTS tsv;
DROP FUNCTION IF EXISTS graph.artifact_index_tsv_doc(text, text, text[]);
DROP FUNCTION IF EXISTS graph.identifiers_text(text[]);
