-- +goose Up
-- Board rank of each issue's parent epic, so the /live 📌 PINS board section can
-- order epic swimlanes exactly like the PAY board (board 193). Filled by
-- refresh_jira_board from GET /rest/agile/1.0/board/193/epic (epics returned in
-- board order). Sentinel 2147483647 = epic not on the board / no epic → sorts last.
ALTER TABLE graph.jira_epic_map
  ADD COLUMN IF NOT EXISTS epic_rank INT NOT NULL DEFAULT 2147483647;

-- +goose Down
ALTER TABLE graph.jira_epic_map DROP COLUMN IF EXISTS epic_rank;
