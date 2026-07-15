-- +goose Up
-- on_board = the epic has at least one issue in board 193's active sprint
-- (board 193 is Scrum, so its Epics panel = active-sprint epics). The /live
-- 📌 PINS board section hides off-board epics so it matches the Jira board
-- instead of showing every project epic a Slack thread happens to mention.
-- DEFAULT true so a not-yet-refreshed row stays visible (and a transient Jira
-- failure can never blank the panel).
ALTER TABLE graph.jira_epic_map
  ADD COLUMN IF NOT EXISTS on_board BOOLEAN NOT NULL DEFAULT true;

-- +goose Down
ALTER TABLE graph.jira_epic_map DROP COLUMN IF EXISTS on_board;
