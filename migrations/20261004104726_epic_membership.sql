-- +goose Up
-- Node → epic membership, rebuilt by refresh_jira_board every run (6h).
-- One row per (node, epic); `via` says how the node joined:
--   epic_self  the epic itself, or an issue whose jira_epic_map epic is E   (1.0)
--   key        thread root / PR / doc with a REFERENCES edge to E or to one
--              of E's issues                                                 (1.0)
--   topic_link SAME_TOPIC edge (confidence ≥ 0.8) to a key/epic_self member;
--              one hop, never chained                                        (edge confidence)
--   eligible   business root only: message with an eligible gate decision   (gate score)
-- first_at/last_at are the epic's activity window (min/max of members'
-- COALESCE(created_at, first_seen_at)), stored on the epic's own row.
-- Per-instance like graph.jira_epic_map — not part of cloud/local sync.
CREATE TABLE IF NOT EXISTS graph.epic_membership (
  node_id      TEXT NOT NULL REFERENCES graph.nodes(id) ON DELETE CASCADE,
  epic_key     TEXT NOT NULL,             -- 'PAY-2307' or 'business:payments'
  via          TEXT NOT NULL,
  confidence   DOUBLE PRECISION NOT NULL DEFAULT 1.0,
  first_at     TIMESTAMPTZ,
  last_at      TIMESTAMPTZ,
  refreshed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  PRIMARY KEY (node_id, epic_key)
);

-- +goose Down
DROP TABLE IF EXISTS graph.epic_membership;
