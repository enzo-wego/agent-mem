-- +goose Up
CREATE TABLE graph.person_distance (
  a_eeid    INTEGER NOT NULL,
  b_eeid    INTEGER NOT NULL,
  hops      SMALLINT NOT NULL,    -- distance in the reports_to tree
  lca_eeid  INTEGER NOT NULL,     -- lowest common ancestor
  refreshed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  PRIMARY KEY (a_eeid, b_eeid)
);
CREATE INDEX idx_person_distance_a_hops ON graph.person_distance(a_eeid, hops);

-- +goose Down
DROP TABLE IF EXISTS graph.person_distance;
