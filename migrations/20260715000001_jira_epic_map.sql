-- +goose Up
-- Jira issue → parent epic, for the /live 📌 PINS "board" section (threads
-- referencing a PAY ticket, grouped by epic). Filled by the refresh_jira_board
-- job from the issue keys already present in graph.nodes (type='jira').
-- Per-instance like graph.slack_channels — not part of cloud/local sync.
CREATE TABLE IF NOT EXISTS graph.jira_epic_map (
  issue_key     TEXT PRIMARY KEY,          -- 'PAY-2227'
  issue_summary TEXT NOT NULL DEFAULT '',
  issue_status  TEXT NOT NULL DEFAULT '',
  epic_key      TEXT NOT NULL DEFAULT '',  -- 'PAY-2197'; '' = no epic
  epic_summary  TEXT NOT NULL DEFAULT '',
  epic_status   TEXT NOT NULL DEFAULT '',
  refreshed_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  machine_id    TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_jira_epic_map_epic ON graph.jira_epic_map(epic_key);

-- +goose Down
DROP TABLE IF EXISTS graph.jira_epic_map;
