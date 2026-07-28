-- +goose Up
-- Job title from the BambooHR org-chart page graph (orgchart.php?id=<eeid> embeds the
-- whole tree as JSON, 100% title coverage — the CSV export only reveals your own row).
-- Gives real seniority ("Senior Director, Engineering") instead of inferring it from
-- subtree size. Nullable: people who only exist via Slack have no title.
ALTER TABLE graph.people ADD COLUMN IF NOT EXISTS job_title text;

-- Leavers: people who vanish from BambooHR must NOT be deleted — their authored
-- graph.nodes still have to resolve — so they are retired instead.
ALTER TABLE graph.people ADD COLUMN IF NOT EXISTS active boolean NOT NULL DEFAULT true;

-- One-off backfill: identity merges kept the survivor row but dropped the name, leaving
-- 201 live rows with a blank display_name (Lei Zheng among them) while the row that was
-- merged away still held it. Survivors inherit the longest name from what they absorbed.
-- mergePersonInto now carries the name forward, so this is a repair, not a recurring job.
UPDATE graph.people s
SET display_name = m.display_name
FROM (
	SELECT DISTINCT ON (merged_into) merged_into, display_name
	FROM graph.people
	WHERE merged_into IS NOT NULL AND COALESCE(display_name, '') <> ''
	ORDER BY merged_into, length(display_name) DESC
) m
WHERE s.id = m.merged_into AND COALESCE(s.display_name, '') = '';

-- Remaining blanks that have a Slack identity: fall back to the Slack profile name.
UPDATE graph.people p
SET display_name = COALESCE(NULLIF(su.real_name, ''), su.display_name)
FROM graph.slack_users su
WHERE su.slack_user_id = p.slack_user_id
  AND COALESCE(p.display_name, '') = ''
  AND COALESCE(NULLIF(su.real_name, ''), su.display_name, '') <> '';

-- +goose Down
ALTER TABLE graph.people DROP COLUMN IF EXISTS job_title;
ALTER TABLE graph.people DROP COLUMN IF EXISTS active;
