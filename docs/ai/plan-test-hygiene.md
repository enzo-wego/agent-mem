# Plan: green `go vet` and graph tests on main (agent-mem-hurt, agent-mem-j1nn)

REVIEWER: pi (auto: GPT weekly 32% left)

## Problem

1. **hurt.** `go vet ./...` fails on main: "import cycle not allowed in test".
   `internal/llmgateway/client_test.go` is package `llmgateway` (an internal
   test) and imports `internal/graph/handlers` (line 16). It does so for two
   checks: `var _ handlers.GeminiClient = (*Client)(nil)` (line 21) and
   `handlers.SummaryLease <= RequestTimeout` (line 295). `handlers` imports
   `llmgateway` (`subject_queries.go:14`, for `llmgateway.ErrCapped`).
2. **j1nn.** On a fresh migrated scratch DB, these fail on main (verified by the
   conductor on 06f97f1 and again on 73b9b64):
   - `TestE2E_IngestContent_TRYThread` (package `internal/graph`): POST outcome
     `skipped_non_incident`, want `created`. That outcome comes from
     `internal/graph/handlers/ingest_content.go:247`.
   - `TestImportBambooHR_CSVBytes_ParsesAndUpserts`: expected 3 people rows, got
     1. Recent changes to `import_bamboohr.go`: 751f896 ("stop merging
     strangers"), 8f08f1a.
   - `TestIngestURL_AlreadyFresh`: outcome `queued_for_fetch`, want
     `already_fresh`, and 2 jobs instead of 0.
   - In a full serialized run (`GOFLAGS=-p=1 go test ./internal/graph/...`) only:
     `TestE2E_StuckJobRecoveredAndCompleted` ("janitor did not recover stuck
     job; calls=1") and `TestHeartbeat_ExtendsLease` ("no rows in result set").
     Both pass in isolation.

## Goal

`go vet ./...` exits 0, and `go test ./internal/graph/...` passes on a fresh
scratch DB, both in a normal run and with `GOFLAGS=-p=1`. Each fix matches what
the product is supposed to do.

## The rule that matters most

For each failing test, first decide which one is wrong: **the test or the
code.** Use `git log -p` on the code path and the test, and read the commit
messages. The test is stale when a commit deliberately changed the behaviour
(e.g. "incident-only author guard", "stop merging strangers") and nobody updated
the test. Then update the test's fixture or expectation to match the intended
behaviour, and cite the commit. The code is wrong when the behaviour change was
not intended. Then **STOP and report** with the evidence. Do not change product
code in this round, and never weaken an assertion just to make it pass.

## Approach

1. **hurt.** Remove the cycle without losing the two checks. Preferred: move
   both into a test in package `handlers_test`, which may import `llmgateway`
   because `llmgateway` does not import `handlers_test`. Name it
   `internal/graph/handlers/llmgateway_contract_test.go`. If `client_test.go`
   uses nothing else from `handlers`, drop the import there. `go vet ./...` then
   exits 0.
2. **j1nn, three deterministic failures.** Classify each one as above, with the
   commit that explains it. Fix the stale tests only.
3. **j1nn, the two flakes.** Find why they fail only in a full serialized run.
   Likely candidates: shared tables left dirty by an earlier package, or a
   janitor/heartbeat timing that depends on a global clock or lease constant. Fix
   the test isolation (truncate/setup) rather than raise timeouts. A timeout
   increase is acceptable only if you show the root cause is wall time, and it is
   marked with a `ponytail:` comment naming the ceiling.

## Files expected to change

- `internal/llmgateway/client_test.go`, and the new
  `internal/graph/handlers/llmgateway_contract_test.go`
- the test files of the failing tests and their shared test helpers, only
- `docs/ai/report-test-hygiene.md`
- No product code. If a fix seems to need product code, STOP and report.

## Acceptance criteria

1. `go vet ./...` exits 0 (output pasted).
2. On a fresh scratch DB (throwaway `pgvector/pgvector:pg16` on a free port,
   database `agentmem_test`, `go run ./cmd/agent-mem migrate`; never the dev DB
   on 5433):
   - `go test -count=1 ./internal/graph/...` passes;
   - `GOFLAGS=-p=1 go test -count=1 ./internal/graph/...` passes twice in a row.

   Paste all three outputs.
3. The report lists each test: test wrong or code wrong, the commit that proves
   it, and what changed.
4. No assertion is removed or loosened without a cited behaviour change. No new
   skips, no TODOs.

## Shipping

Branch `fix/test-hygiene` off `origin/main`. Commit and push. No PR, merge or
deploy.

## Revision 2 (answers pi review 1, 2 findings). These rules override the sections above where they differ.

1. **Prove the tests ran.** Some graph tests read `DATABASE_URL` and some read
   `AGENT_MEM_TEST_DATABASE_URL` (e.g. `handlers/refresh_slack_users_test.go:21`).
   For every verification run, set **both** to the scratch DB, and run migrations
   against that DB. Use `go test -json` (or `-v`) and paste a per-package count of
   PASS / FAIL / SKIP. List every remaining SKIP with its reason. A SKIP for a
   missing DB variable means the run doesn't count.
2. **Cross-package isolation: serialized is the supported mode.** Graph test
   packages share one DB and delete the same tables (`jobs/queue_test.go:38` and
   `handlers/testdb_test.go:61` both clear `graph.jobs`), so a package-parallel
   run can wipe another package's fixtures. Don't redesign that here. Instead:
   - add a Makefile target `test-db` that runs `go test -p=1 -count=1
     ./internal/graph/...` with both variables taken from `TEST_DATABASE_URL`;
   - add one comment line in the main shared helper (`testdb_test.go` or
     `helpers_test.go`) saying DB tests must run with `-p=1`;
   - acceptance becomes: `make test-db TEST_DATABASE_URL=<scratch>` passes
     **three times in a row**, and that's the only parallelism claim made. The
     two flakes failed **inside** a serialized run, so their cause is state left
     by an earlier package or test. Find it and fix it. You may change any shared
     test helper this needs, beyond the five named tests.
   Drop the normal (package-parallel) run from the acceptance criteria. Record
   in the report that it isn't supported, and why.

## Revision 3 (answers pi review 2, 2 findings)

1. **The stuck-job test must prove the janitor did the recovery.**
   `jobs/e2e_test.go:24-30` currently returns an ordinary retryable error, so
   normal requeueing can satisfy it with no janitor involved. When you fix
   `TestE2E_StuckJobRecoveredAndCompleted`, make the fixture an abandoned claim:
   a job in `running` with an expired lease and no live worker heartbeating it.
   Assert that the janitor moves it back to runnable (check the row state the
   janitor writes), then that it completes. Prove it: with the janitor's reclaim
   step disabled (temporarily, e.g. not starting the janitor in the test), the
   test fails. Paste that red run. Don't change the janitor itself; if it doesn't
   reclaim, STOP and report.
2. **Hermetic runs.** The `test-db` target and every verification command set
   `AGENT_MEM_EVAL=` (empty) explicitly. The skip in
   `handlers/topic_judge_eval_test.go` ("set AGENT_MEM_EVAL=1 ...") is
   intentional, is outside acceptance, and is the one SKIP allowed in the counts.

## Revision 4: final (answers pi review 3; approved by Enzo 2026-10-05 without a 4th review)

Allowed SKIPs in the counts, each with its reason, and nothing else:
- `handlers/topic_judge_eval_test.go`: live eval, needs `AGENT_MEM_EVAL=1`
  (intentionally off);
- `handlers/describe_attachment_test.go:107`
  (`TestDescribeAttachmentHandler_UnsupportedMime`): skips unconditionally on
  main; leave it as is and list it;
- `liteparse_test.go:96`: skips when the `lit` binary is absent. If `lit` is
  installed on this machine, it must run and pass. If not, list the skip.
Any SKIP caused by a missing DB variable still fails acceptance.
