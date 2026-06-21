-- +goose Up

-- idx_artifact_index_node_type was a btree on (node_id) INCLUDE (embedding).
-- It is broken: a VECTOR(768) is 3072 bytes, so the index row (3128B) exceeds
-- the btree max of 2704B and every INSERT/UPSERT into artifact_index fails.
-- It is also redundant: node_id is the PK, and vector search uses the HNSW
-- index idx_artifact_index_embedding. Drop it.
DROP INDEX IF EXISTS graph.idx_artifact_index_node_type;

-- +goose Down
-- Intentionally not recreated — the index was non-functional.
