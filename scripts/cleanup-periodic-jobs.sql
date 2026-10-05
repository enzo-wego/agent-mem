-- Run only after every queue writer against this database has stopped.
-- Preview queued/running counts separately, then supply the queued total:
--   psql -X -v expected=<previewed queued total> -f scripts/cleanup-periodic-jobs.sql
\set ON_ERROR_STOP 1
BEGIN;
SELECT set_config('agentmem.cleanup_expected', :'expected', true);
DO $$
DECLARE
  n bigint;
  exp bigint := current_setting('agentmem.cleanup_expected')::bigint;
BEGIN
  DELETE FROM graph.jobs
   WHERE type IN ('notify_watch_channels','detect_hot_topics','derive_person_roles','refresh_jira_board')
     AND status = 'queued';
  GET DIAGNOSTICS n = ROW_COUNT;
  IF n <> exp THEN
    RAISE EXCEPTION 'deleted % rows, expected %', n, exp;
  END IF;
  DELETE FROM public.settings WHERE key IN (
    'graph.periodic.notify_watch_channels.last_enqueued_at',
    'graph.periodic.detect_hot_topics.last_enqueued_at',
    'graph.periodic.derive_person_roles.last_enqueued_at',
    'graph.periodic.refresh_jira_board.last_enqueued_at');
END $$;
COMMIT;
