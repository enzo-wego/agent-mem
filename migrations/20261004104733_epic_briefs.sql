-- +goose Up
-- Standing per-epic brief, rebuilt by the refresh_epic_brief job (enqueued by
-- refresh_jira_board for on-board epics whose member set or member summaries
-- changed). A DB read serves it: /api/graph/epic/{key}, the /live board
-- swimlane header, session-start context and partner DMs.
--   member_signature  sha256 over the sorted member ids + each member's summary
--                     version (thread_summaries.signature for Slack roots,
--                     artifact_index.refreshed_at otherwise); the job skips
--                     when unchanged, so it is idempotent per membership state
--   sources           member node ids the brief was built from (delta runs
--                     feed only members changed since updated_at plus this
--                     list decides which members are new)
--   previous          the brief this one replaced, for diffing
-- Per-instance like graph.jira_epic_map — not part of cloud/local sync.
CREATE TABLE IF NOT EXISTS graph.epic_briefs (
  epic_key         TEXT PRIMARY KEY,
  brief            TEXT NOT NULL DEFAULT '',
  highlights       JSONB NOT NULL DEFAULT '[]'::jsonb,
  open_items       JSONB NOT NULL DEFAULT '[]'::jsonb,
  member_signature TEXT NOT NULL DEFAULT '',
  sources          TEXT[] NOT NULL DEFAULT '{}'::text[],
  updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  previous         TEXT,
  previous_at      TIMESTAMPTZ
);

-- Off by default: the job is canaried (dry_run) before the lead enables it.
-- Both are dashboard-editable (Settings → Epic briefs).
INSERT INTO settings (key, value) VALUES
  ('graph.epic_briefs.enabled', 'false'),
  ('graph.epic_briefs.min_interval_minutes', '60')
ON CONFLICT (key) DO NOTHING;

-- +goose Down
DELETE FROM settings WHERE key IN ('graph.epic_briefs.enabled', 'graph.epic_briefs.min_interval_minutes');
DROP TABLE IF EXISTS graph.epic_briefs;
