-- +goose Up
DELETE FROM graph.edges WHERE from_node_id = to_node_id;

-- +goose Down
-- No-op: invalid self-loop rows are not restored.
