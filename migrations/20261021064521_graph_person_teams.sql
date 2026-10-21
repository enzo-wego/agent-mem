-- +goose Up
CREATE TABLE graph.person_teams (
  person_id      BIGINT NOT NULL REFERENCES graph.people(id),
  team_node_id   TEXT   NOT NULL REFERENCES graph.nodes(id),
  basis          TEXT   NOT NULL CHECK (basis IN ('listed','manager','department','other')),
  source         TEXT   NOT NULL DEFAULT 'tenderloin',
  last_confirmed TEXT   NOT NULL DEFAULT '',   -- YYYY-MM-DD from the source, or ''
  first_seen_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (person_id, team_node_id)
);

-- +goose Down
DROP TABLE IF EXISTS graph.person_teams;
