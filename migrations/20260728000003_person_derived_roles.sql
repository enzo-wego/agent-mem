-- +goose Up
-- Evidence-backed role inference. This is deliberately separate from graph.people:
-- BambooHR owns department/title, while domain and role_label are derived from Slack
-- usergroups, org edges, and activity and may change on every recompute.
CREATE TABLE graph.person_derived_roles (
  eeid        INTEGER PRIMARY KEY,
  domain      TEXT NOT NULL,
  role_label  TEXT NOT NULL,
  confidence  DOUBLE PRECISION NOT NULL CHECK (confidence >= 0 AND confidence <= 1),
  evidence    JSONB NOT NULL DEFAULT '{}'::jsonb,
  computed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  machine_id  TEXT NOT NULL
);

CREATE INDEX idx_person_derived_roles_domain
  ON graph.person_derived_roles(domain);

-- +goose Down
DROP TABLE IF EXISTS graph.person_derived_roles;
