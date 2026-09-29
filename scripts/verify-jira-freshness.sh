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
# Serial packages (-p 1): they share the agentmem_test DB. Known failures make
# go test exit non-zero, so this pipeline is allowed to fail and the explicit
# KNOWN_FAILING check below decides.
go test -count=1 -p 1 ./internal/graph/... ./internal/worker/... 2>&1 | tee "$L/all.log" || true
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
# fail on main, agent-mem-y827
KNOWN_FAILING=(TestE2E_IngestContent_TRYThread TestImportBambooHR_CSVBytes_ParsesAndUpserts TestIngestURL_AlreadyFresh)
FAILED=$(sed -n 's/^--- FAIL: \([^ ]*\) .*/\1/p' "$L/all.log" | sort -u)
for k in "${KNOWN_FAILING[@]}"; do
  grep -qx -- "$k" <<<"$FAILED" || echo "note: now passing: $k"
done
UNEXPECTED=$(grep -vxF -f <(printf '%s\n' "${KNOWN_FAILING[@]}") <<<"$FAILED" || true)
if [[ -n "$UNEXPECTED" ]]; then echo "UNEXPECTED FAIL: $UNEXPECTED"; exit 1; fi
grep -qx '# fail 0' "$L/node.log"; grep -qx '# pass 8' "$L/node.log"
echo "ALL CHECKS PASSED"
