-- +goose Up
-- Delta bookkeeping for the refresh_epic_brief job.
--   fed            member node id -> "<sig version>|<node id>|<summary version>"
--                  as of the last time that member was sent to the LLM; a
--                  member is pending when absent or when its current value
--                  differs. NULL on rows built before this column existed
--                  (the next run is then a full build).
--   built_version  epicBriefSigVersion of the last FULL build behind the
--                  stored brief; NULL on legacy rows.
ALTER TABLE graph.epic_briefs
  ADD COLUMN fed jsonb,
  ADD COLUMN built_version text;

-- +goose Down
ALTER TABLE graph.epic_briefs
  DROP COLUMN IF EXISTS fed,
  DROP COLUMN IF EXISTS built_version;
