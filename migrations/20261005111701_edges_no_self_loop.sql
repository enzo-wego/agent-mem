-- +goose Up
ALTER TABLE graph.edges ADD CONSTRAINT edges_no_self_loop CHECK (from_node_id <> to_node_id);

-- +goose Down
ALTER TABLE graph.edges DROP CONSTRAINT IF EXISTS edges_no_self_loop;
