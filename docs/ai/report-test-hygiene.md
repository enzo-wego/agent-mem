# Test hygiene evidence

## Scope and status

Tests and test infrastructure only; no product code changes. The complete approved plan, including revisions 2–4, is committed alongside this report.

The historical `TestHeartbeat_ExtendsLease` failure has not reproduced. Its root cause is **not established**, and the test remains unchanged. This is an explicit gap against revision 2's request to identify both serialized flakes; passing runs must not be described as proof of that historical root cause.

Package-parallel DB testing is not supported: graph packages share tables, and multiple helpers delete `graph.jobs`. `make test-db` is the supported serialized mode (`-p=1 -count=1 -json`), with both DB variables supplied by `TEST_DATABASE_URL` and `AGENT_MEM_EVAL=` explicitly empty. No package-parallel success claim is made.

## Safety and environment

- Work was confined to the existing `fix/test-hygiene` worktree.
- Immediately before container creation, `docker ps --format '{{.Names}} {{.Ports}}'` showed port 5433 occupied by the dev DB and no binding on 55439.
- Created only `fix-test-hygiene`, using `pgvector/pgvector:pg16`, bound to `127.0.0.1:55439`; database `agentmem_test`.
- Scratch DSN: `postgres://agentmem:scratch-test-only@127.0.0.1:55439/agentmem_test?sslmode=disable`. The password is a disposable fixture value, not a credential read from the environment or configuration.
- Migrated this DB with `go run ./cmd/agent-mem migrate`; output:

```text
INF Running migrations dir=./migrations
INF Migrations applied
```

All migration/test/vet processes used `env -i PATH="$PATH" HOME="$HOME"`, both `DATABASE_URL` and `AGENT_MEM_TEST_DATABASE_URL` set to the scratch DSN, and `AGENT_MEM_EVAL=`. For Makefile runs, the target sets both DB variables from the explicitly supplied scratch DSN. No dev DB/hub, provider credential, or live runtime setting values were accessed. No PR, merge, deployment, or push-protection bypass was performed.

## Classification and history

| Test/check | Classification and evidence | Change |
|---|---|---|
| Gateway interface / timeout ordering | Test dependency wrong: same-package `llmgateway` test imported `handlers`, while production `handlers/subject_queries.go` imports `llmgateway`. | Moved the compile-time `handlers.GeminiClient` conformance check and strict `SummaryLease > RequestTimeout` assertion into external `handlers_test/llmgateway_contract_test.go`; kept the strict gateway request-timeout check in `client_test.go`. |
| `TestE2E_IngestContent_TRYThread` | Test fixture stale. Original `b76915d` used a payments-alerts channel and a URL pointing to the ingested node itself. `92dfc60` deliberately introduced incident-only author filtering; `89d238a` deliberately prohibited graph self-loops. | Uses ordinary fixture channel `CTESTTRY` and a distinct referenced Slack thread. Preserves created/updated/unchanged, stored rows, Jira edge, Slack edge, and edge-count assertions. |
| `TestImportBambooHR_CSVBytes_ParsesAndUpserts` | Test fixture wrong. `d2ce092` introduced manager names in the CSV fixture, but the schema's `reports_to` is integer. `751f896` explicitly states real exports contain manager EEIDs and corrects depth computation to resolve EEIDs. | Manager fields changed to `1` and `2`; retains exactly three people and depths `{1:0, 2:1, 3:2}`. |
| `TestIngestURL_AlreadyFresh` | Test seed wrong, not freshness code. `b574b9d` introduced a body INSERT without required `machine_id` and ignored seed errors. `af68f37` explicitly fixed this same missing-column defect in production body upserts; the original schema already required the column. | Adds fixture `machine_id` and fails on both seed INSERT errors. Keeps five-minute age, HTTP 202, `already_fresh`, and zero jobs. |
| `TestE2E_StuckJobRecoveredAndCompleted` | Test did not prove its stated contract. `6870d18` introduced a sleeping live handler returning an ordinary retryable error; normal Retry could satisfy the test, and the janitor raced a still-live worker's state write. Current `worker.go` confirms ordinary errors take the Retry path. The original test passed in the isolated package diagnostic (3.03s), so that run does not establish the cause of the historical `calls=1` result. | Seeds an abandoned running claim, expires its lease with DB time, and starts only the janitor. Asserts queued state, cleared lock/lease fields, exact janitor error marker, and unchanged first attempt. Only then starts dispatch and asserts done, one handler execution, and exactly two total claims. Waits for the janitor and manager shutdown. |
| `TestHeartbeat_ExtendsLease` | Historical cause unresolved. `f196a08` introduced the current lease-extension contract; `6cbb09b` introduced its shared helper, which deletes jobs before seeding. Neither the original isolated package diagnostic nor the full serialized diagnostic reproduced the missing row. | Unchanged: retains the elapsed-initial-lease and future-lease assertion. No timeout increase, assertion weakening, or speculative product fix. |

### Additional serialized-run isolation defects

The initial serialized diagnostic exposed ACL cleanup errors: `graph.people` could not be deleted while a previous package's `graph.identity_map` rows still referenced it. The existing ACL helper (history `fd36a20`) omitted that dependent table and only logged deletion failures. Added deletion of `identity_map` before `people`, and made cleanup errors fatal. Subsequent targeted/full evidence must be read independently from the historical heartbeat failure: this FK defect does not prove why a jobs row disappeared.

A repeated full run also exposed two TSV migration-fixture failures:

```text
TestArtifactTSV_UpLockBounded:
  after failed up: tsv column=1 trigger=1 functions=3, want 0/0/0
TestArtifactTSV_PopulatedUpgrade:
  db version after DownTo = 20260929065720 (<nil>), want 20261003154813
```

History `ad39ee5` deliberately changed Jira migration tests to reapply only their own migration through `Provider.ApplyVersion`, leaving older migration entries recorded after newer ones. Scratch migration history confirmed that insertion order. The TSV fixtures (`706d495`, `7e9ffa7`) still assumed application order equaled version order. Goose's provider `GetDBVersion` reports the highest applied version, but even provider `DownTo` rolls back in application order and stops too early in this state. Switching only to `Provider.DownTo` was tested and insufficient; it still left TSV installed and reported highest version `20261005111701`.

Both TSV fixtures now use the existing provider convention plus shared test helper `downToTSVBase`: select all applied versions greater than the base, apply their Down migrations in descending version order, and assert the highest applied version equals the exact base. Restoration uses `provider.Up` and reports errors. All original rollback, lock-bound, upgrade, backfill, and content assertions remain intact. No migration/product SQL changed.

The focused migration sequence passed twice on the same scratch DB:

```text
=== RUN   TestArtifactTSV_UpLockBounded
--- PASS: TestArtifactTSV_UpLockBounded (5.17s)
=== RUN   TestArtifactTSV_PopulatedUpgrade
--- PASS: TestArtifactTSV_PopulatedUpgrade (0.12s)
=== RUN   TestJiraUpdatesMigration_UpDown
--- PASS: TestJiraUpdatesMigration_UpDown (0.05s)
=== RUN   TestArtifactTSV_UpLockBounded
--- PASS: TestArtifactTSV_UpLockBounded (5.15s)
=== RUN   TestArtifactTSV_PopulatedUpgrade
--- PASS: TestArtifactTSV_PopulatedUpgrade (0.13s)
=== RUN   TestJiraUpdatesMigration_UpDown
--- PASS: TestJiraUpdatesMigration_UpDown (0.06s)
PASS
ok github.com/agent-mem/agent-mem/internal/graph/handlers 11.232s
```

## Janitor negative control

Temporarily replaced only `janitor.Run(ctx)` with an unused-handle statement; no product code changed. Ran the named test with both scratch variables and eval disabled:

```text
=== RUN   TestE2E_StuckJobRecoveredAndCompleted
    e2e_test.go:38: waitForStatus: job 854: got "running", want "queued" after 2s
--- FAIL: TestE2E_StuckJobRecoveredAndCompleted (2.06s)
FAIL
FAIL github.com/agent-mem/agent-mem/internal/graph/jobs 2.743s
FAIL
```

Exit 1. Restored the reclaim loop immediately. Enabled targeted run:

```text
=== RUN   TestE2E_StuckJobRecoveredAndCompleted
--- PASS: TestE2E_StuckJobRecoveredAndCompleted (0.13s)
=== RUN   TestHeartbeat_ExtendsLease
--- PASS: TestHeartbeat_ExtendsLease (2.52s)
PASS
ok github.com/agent-mem/agent-mem/internal/graph/jobs 2.976s
```

## Vet and targeted verification

```text
$ go vet ./...
(no output)
exit 0
```

The actual command used the isolated environment and both scratch DB variables described above. `internal/llmgateway` tests also passed after contract relocation.

Targeted synchronous ingestion, people import, freshness, and gateway lease-contract checks:

```text
=== RUN   TestE2E_IngestContent_TRYThread
--- PASS: TestE2E_IngestContent_TRYThread (0.09s)
PASS
ok github.com/agent-mem/agent-mem/internal/graph 0.675s
=== RUN   TestImportBambooHR_CSVBytes_ParsesAndUpserts
--- PASS: TestImportBambooHR_CSVBytes_ParsesAndUpserts (0.04s)
=== RUN   TestIngestURL_AlreadyFresh
--- PASS: TestIngestURL_AlreadyFresh (0.03s)
=== RUN   TestSummaryLeaseExceedsGatewayRequestTimeout
--- PASS: TestSummaryLeaseExceedsGatewayRequestTimeout (0.00s)
PASS
ok github.com/agent-mem/agent-mem/internal/graph/handlers 0.608s
```

## Allowed skips

The diagnostic JSON output contained exactly these skips, all allowed by revision 4:

- `TestDescribeAttachmentHandler_UnsupportedMime`: `requires network; covered by integration tests` (unconditional on main).
- `TestParseDocument_RichPDF`: `lit binary not in PATH; skipping real PDF test`.
- `TestTopicJudgeGolden`: `set AGENT_MEM_EVAL=1 to run the topic-judge eval (needs real DB + API key)`; live eval deliberately disabled.

No missing-DB-variable skip is acceptable. No new skips were added.

## Three consecutive serialized acceptance runs

Executed this command three times in one `&&` chain on the same scratch DB, without reset or remigration between runs. The chain exited 0.

```sh
env -i PATH="$PATH" HOME="$HOME" AGENT_MEM_EVAL= \
  make test-db TEST_DATABASE_URL='postgres://agentmem:scratch-test-only@127.0.0.1:55439/agentmem_test?sslmode=disable'
```

Counts include top-level tests and subtests (JSON events with a nonempty `Test`); package-level PASS is not double-counted. Each run had **1,458 PASS, 0 FAIL, 3 SKIP** across 14 packages. The three skips in every run were exactly the allowed names/reasons listed above; no missing-variable skip occurred.

| Package | Run 1 PASS / FAIL / SKIP | Run 2 PASS / FAIL / SKIP | Run 3 PASS / FAIL / SKIP |
|---|---:|---:|---:|
| `internal/graph` | 2 / 0 / 0 | 2 / 0 / 0 | 2 / 0 / 0 |
| `internal/graph/acl` | 4 / 0 / 0 | 4 / 0 / 0 | 4 / 0 / 0 |
| `internal/graph/bfs` | 7 / 0 / 0 | 7 / 0 / 0 | 7 / 0 / 0 |
| `internal/graph/entities` | 9 / 0 / 0 | 9 / 0 / 0 | 9 / 0 / 0 |
| `internal/graph/extractor` | 39 / 0 / 0 | 39 / 0 / 0 | 39 / 0 / 0 |
| `internal/graph/fetchers` | 90 / 0 / 0 | 90 / 0 / 0 | 90 / 0 / 0 |
| `internal/graph/handlers` | 971 / 0 / 3 | 971 / 0 / 3 | 971 / 0 / 3 |
| `internal/graph/hydrate` | 5 / 0 / 0 | 5 / 0 / 0 | 5 / 0 / 0 |
| `internal/graph/identity` | 6 / 0 / 0 | 6 / 0 / 0 | 6 / 0 / 0 |
| `internal/graph/ids` | 85 / 0 / 0 | 85 / 0 / 0 | 85 / 0 / 0 |
| `internal/graph/jobs` | 98 / 0 / 0 | 98 / 0 / 0 | 98 / 0 / 0 |
| `internal/graph/normalizer` | 70 / 0 / 0 | 70 / 0 / 0 | 70 / 0 / 0 |
| `internal/graph/scoring` | 34 / 0 / 0 | 34 / 0 / 0 | 34 / 0 / 0 |
| `internal/graph/temporal` | 38 / 0 / 0 | 38 / 0 / 0 | 38 / 0 / 0 |

### Run 1: package output extracted from JSON

```text
ok github.com/agent-mem/agent-mem/internal/graph 0.674s
ok github.com/agent-mem/agent-mem/internal/graph/acl 0.529s
ok github.com/agent-mem/agent-mem/internal/graph/bfs 0.524s
ok github.com/agent-mem/agent-mem/internal/graph/entities 0.529s
ok github.com/agent-mem/agent-mem/internal/graph/extractor 0.553s
ok github.com/agent-mem/agent-mem/internal/graph/fetchers 0.854s
ok github.com/agent-mem/agent-mem/internal/graph/handlers 56.967s
ok github.com/agent-mem/agent-mem/internal/graph/hydrate 0.534s
ok github.com/agent-mem/agent-mem/internal/graph/identity 0.580s
ok github.com/agent-mem/agent-mem/internal/graph/ids 0.398s
ok github.com/agent-mem/agent-mem/internal/graph/jobs 51.052s
ok github.com/agent-mem/agent-mem/internal/graph/normalizer 0.561s
ok github.com/agent-mem/agent-mem/internal/graph/scoring 0.525s
ok github.com/agent-mem/agent-mem/internal/graph/temporal 0.416s
exit 0
```

Jobs-path events:

```text
PASS TestE2E_StuckJobRecoveredAndCompleted (0.12s)
PASS TestHeartbeat_ExtendsLease (2.52s)
```

### Run 2: package output extracted from JSON

```text
ok github.com/agent-mem/agent-mem/internal/graph 0.299s
ok github.com/agent-mem/agent-mem/internal/graph/acl 0.317s
ok github.com/agent-mem/agent-mem/internal/graph/bfs 0.337s
ok github.com/agent-mem/agent-mem/internal/graph/entities 0.343s
ok github.com/agent-mem/agent-mem/internal/graph/extractor 0.356s
ok github.com/agent-mem/agent-mem/internal/graph/fetchers 0.733s
ok github.com/agent-mem/agent-mem/internal/graph/handlers 49.932s
ok github.com/agent-mem/agent-mem/internal/graph/hydrate 0.551s
ok github.com/agent-mem/agent-mem/internal/graph/identity 0.343s
ok github.com/agent-mem/agent-mem/internal/graph/ids 0.180s
ok github.com/agent-mem/agent-mem/internal/graph/jobs 50.733s
ok github.com/agent-mem/agent-mem/internal/graph/normalizer 0.303s
ok github.com/agent-mem/agent-mem/internal/graph/scoring 0.300s
ok github.com/agent-mem/agent-mem/internal/graph/temporal 0.181s
exit 0
```

Jobs-path events:

```text
PASS TestE2E_StuckJobRecoveredAndCompleted (0.12s)
PASS TestHeartbeat_ExtendsLease (2.51s)
```

### Run 3: package output extracted from JSON

```text
ok github.com/agent-mem/agent-mem/internal/graph 0.376s
ok github.com/agent-mem/agent-mem/internal/graph/acl 0.306s
ok github.com/agent-mem/agent-mem/internal/graph/bfs 0.330s
ok github.com/agent-mem/agent-mem/internal/graph/entities 0.339s
ok github.com/agent-mem/agent-mem/internal/graph/extractor 0.322s
ok github.com/agent-mem/agent-mem/internal/graph/fetchers 0.699s
ok github.com/agent-mem/agent-mem/internal/graph/handlers 46.508s
ok github.com/agent-mem/agent-mem/internal/graph/hydrate 0.328s
ok github.com/agent-mem/agent-mem/internal/graph/identity 0.324s
ok github.com/agent-mem/agent-mem/internal/graph/ids 0.184s
ok github.com/agent-mem/agent-mem/internal/graph/jobs 50.814s
ok github.com/agent-mem/agent-mem/internal/graph/normalizer 0.668s
ok github.com/agent-mem/agent-mem/internal/graph/scoring 0.558s
ok github.com/agent-mem/agent-mem/internal/graph/temporal 0.454s
exit 0
```

Jobs-path events:

```text
PASS TestE2E_StuckJobRecoveredAndCompleted (0.13s)
PASS TestHeartbeat_ExtendsLease (2.51s)
```

## Review and remaining prerequisite

Two independent read-only reviews found no actionable patch-introduced defect in the gateway/fixtures or isolation/migration changes. Both source/history review and successful runs leave the historical jobs-flake attribution unproven. Claim, heartbeat, janitor, and worker state transitions update rows; the shared test helper already deletes jobs before seeding, and jobs tests do not use `t.Parallel`. Cross-process deletion/environmental interference is only an inference, not an established cause.

To satisfy the original-flake-root requirement, a reproducible failing scratch setup or the conductor's complete failing-run context is still needed. Otherwise an explicit decision to accept the unchanged heartbeat assertion with this nonreproduction evidence is required. No product defect is asserted and no speculative product change was made.
