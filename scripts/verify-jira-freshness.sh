#!/usr/bin/env bash
# Verification for the Jira freshness change (docs/ai/plan-jira-freshness.md).
# Needs DATABASE_URL pointing at the agentmem_test scratch database.
set -euo pipefail
L=$(mktemp -d /tmp/jira-freshness.XXXX); echo "logs: $L"
: "${DATABASE_URL:?}"; [[ "$DATABASE_URL" == */agentmem_test ]]
go build ./... 2>&1 | tee "$L/build.log"
go vet ./... 2>&1 | tee -a "$L/build.log"
go test -count=1 -v ./internal/graph/extractor/ 2>&1 | tee "$L/extractor.log"
go test -count=1 -v -run 'TestJira|TestEdgePersist' ./internal/graph/handlers/ 2>&1 | tee "$L/handlers.log"
go test -count=1 ./internal/graph/... ./internal/worker/... 2>&1 | tee "$L/all.log"
# TAP reporter: node's default reporter is spec, which has no "# pass N" lines.
(cd dashboard && node --test --test-reporter=tap scripts/jiraSyncState.test.ts) 2>&1 | tee "$L/node.log"
(cd dashboard && npm run build) 2>&1 | tee "$L/dash.log"
for t in TestReplyPermalink_RootOnly TestReplyPermalink_ReplyAddsRoot TestReplyPermalink_Reordered \
  TestReplyPermalink_SelfThread TestReplyPermalink_Invalid TestReplyPermalink_DuplicateParam \
  TestReplyPermalink_Delimiters TestEdgePersist_ReplyRoot TestJiraUpdates_Candidates \
  TestJiraUpdates_Pagination TestJiraUpdates_HTTPErrorKeepsCursor TestJiraUpdates_BadTimestampAborts \
  TestJiraUpdates_SuccessAdvances TestJiraUpdates_NoCredentials TestJiraUpdates_CursorMissing \
  TestJiraUpdates_FutureCursorClamped TestJiraUpdates_ExpiredContextRecordsError \
  TestJiraTicker_Disabled TestJiraTicker_EnqueueOnce TestJiraTicker_IntervalBoundary \
  TestJiraTicker_CorruptInterval TestJiraTicker_LoopCancels TestJiraBoard_SearchRequest \
  TestJiraUpdatesConfig_Endpoint TestJiraUpdatesMigration_UpDown; do
  grep -q -- "^--- PASS: $t " "$L/extractor.log" "$L/handlers.log" || { echo "MISSING PASS: $t"; exit 1; }
done
! grep -qE -- '--- (SKIP|FAIL)' "$L/extractor.log" "$L/handlers.log"
! grep -q '^FAIL' "$L/all.log"
grep -qx '# fail 0' "$L/node.log"; grep -qx '# pass 8' "$L/node.log"
echo "ALL CHECKS PASSED"
