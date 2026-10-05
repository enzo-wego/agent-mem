# Jobs hardening evidence

## Scope and isolation

Implemented the five approved fixes; the copied `plan-jobs-hardening.md` includes all four revisions. Revision 4 takes precedence. No operational repair of hub rows was attempted: the approved plan prohibits hub access.

Work stayed in the existing `fix/jobs-hardening` worktree. Immediately before starting `fix-jobs-hardening`, `docker ps` showed the other worker on 55482, the dev DB on 5433, and no binding on 55483. Created `pgvector/pgvector:pg16` bound only to `127.0.0.1:55483`, with database `agentmem_test` and a disposable fixture password.

All builds, migrations and test runs used a scrubbed environment:

```sh
DSN='postgres://agentmem:scratch-test-only@127.0.0.1:55483/agentmem_test?sslmode=disable'
env -i PATH="$PATH" HOME="$HOME" DATABASE_URL="$DSN" \
  AGENT_MEM_TEST_DATABASE_URL="$DSN" AGENT_MEM_EVAL= <command>
```

Checked the literal host, port and database before DB commands. Migration output was `Running migrations` / `Migrations applied`. The only non-`agentmem_test` connection was the explicitly required original-helper negative control against `/postgres` on this same scratch server; it had no graph schema. Guard unit/subprocess cases use port 1 and reject unsafe DSNs before connecting.

No API keys, tokens or runtime settings were read; no dev DB/hub access, PR, merge, deploy, or push-protection bypass. After the user supplied exported issue details, no further Beads commands or writes were run.

## 1. agent-mem-97s — preserve disabled flat work

The pre-claim check leaves messages untouched and warns once per disabled period. `processMessage` captures one client and passes it to both per-type handlers. A missing client after the pre-check returns `errNoLLM`; `errors.Is` routes that error before retry-cap handling to a delayed requeue with `GREATEST(attempts - 1, 0)`. The delay is the normal one-second polling interval. Other failure/retry behavior is unchanged.

Initial tests against the original processor:

```text
status=completed attempts=1, want pending/0
--- FAIL: TestProcessPendingMessages_NoLLMLeavesPending (0.04s)
status=completed attempts=4, want pending/3
--- FAIL: TestProcessPendingMessages_ClientLostRefundsAtCap (0.03s)
FAIL github.com/agent-mem/agent-mem/internal/worker 0.658s
```

The loss test blocks the claim at PostgreSQL, waits until the actual claim is waiting on a lock, removes the client, then releases the claim. The row starts at the retry cap and must end pending at the same cap with a future availability time.

Review found the first captured-client success test did not detect the redundant per-type getter. Replaced it with an injected resolver returning the fake for the pre-claim and capture calls, then nil for every subsequent lookup. It asserts completion, embedding, exactly one stored summary, and exactly two resolutions. Restoring the historical re-fetch/nil return inside `processSummary` produced:

```text
captured client did not embed summary after reload
--- FAIL: TestProcessPendingMessages_CapturedClientCompletes (0.03s)
FAIL github.com/agent-mem/agent-mem/internal/worker 0.665s
```

Restored fix, full worker suite:

```text
--- PASS: TestProcessPendingMessages_NoLLMLeavesPending (0.03s)
--- PASS: TestProcessPendingMessages_ClientLostRefundsAtCap (0.02s)
--- PASS: TestProcessPendingMessages_CapturedClientCompletes (0.03s)
--- PASS: TestProcessPendingMessages_FirstNonRetryableFailureRequeues (0.01s)
--- PASS: TestProcessPendingMessages_ThirdNonRetryableFailureIsTerminal (0.02s)
--- PASS: TestProcessPendingMessages_RetryableFailureDoesNotSpendBudget (0.52s)
ok github.com/agent-mem/agent-mem/internal/worker 1.284s
```

The schema calls its successful status `completed`, not `processed`; assertions use that actual persisted value.

## 2. agent-mem-7bs — guard the effective pgx database

Identity, extractor, both handler test packages, and sync now validate `pgxpool.ParseConfig(dsn).ConnConfig.Database == "agentmem_test"` and connect with that same config through `NewWithConfig`. Pure `checkScratchDSN` wrappers use the shared parse-and-validate function within each package. The inherited handlers guard already parsed pgx's effective database, but reparsed on connection; that reparse is removed. Updated its existing consumers, the external handlers helper used by semantic-floor tests, and sync's overly broad substring guard.

Unit cases cover valid URL/keyword scratch DSNs; refused `agentmem`, `postgres`, malformed input, URL query `dbname` overrides, keyword live DSNs, and a different database containing `test`. Subprocess tests invoke the actual helpers and require refusal before any connection.

Required original-helper negative control, before helper edits, using `/postgres` on scratch 55483:

```text
=== RUN TestEnsurePerson_NewSlackUser
truncate graph.identity_map: ERROR: relation "graph.identity_map" does not exist (SQLSTATE 42P01)
--- FAIL: TestEnsurePerson_NewSlackUser (0.03s)
=== RUN TestExtract_BareJiraKey_KnownPrefixLinked
ERROR: relation "graph.nodes" does not exist (SQLSTATE 42P01)
--- FAIL: TestExtract_BareJiraKey_KnownPrefixLinked (0.02s)
```

Both reached SQL on the non-test database without refusing. With guards installed, the same deliberate refusal diagnostic produced:

```text
refusing to run: DATABASE_URL database name "postgres" is not "agentmem_test"; tests may delete graph rows
--- FAIL: TestEnsurePerson_NewSlackUser (0.00s)
refusing to run: DATABASE_URL database name "postgres" is not "agentmem_test"; tests may delete graph rows
--- FAIL: TestExtract_BareJiraKey_KnownPrefixLinked (0.00s)
```

That diagnostic's exit 1 is expected safety behavior, not an acceptance failure. All pure and subprocess guard tests passed. Full integration suites against `agentmem_test`:

```text
ok github.com/agent-mem/agent-mem/internal/graph/identity 0.647s
ok github.com/agent-mem/agent-mem/internal/graph/extractor 0.633s
ok github.com/agent-mem/agent-mem/internal/sync 0.754s
```

## 3. agent-mem-cg2 — finite heartbeat runtime

`Entry.MaxRuntime` controls the handler context only when `Heartbeat` is true. Non-heartbeat entries still use their lease timeout. Real `RegisterAll` tests enumerate and print the final registry, not preliminary overwritten registrations.

Final heartbeat set, all **30 minutes**:

- `backfill_identifiers`: finite indexed-node sweep, regex extraction and context-bound SQL.
- `recompute_person_distance`: context-bound SQL rebuild; recursive chain depth capped at 20.
- `refresh_slack_members`: context-bound HTTP/SQL, 60-second HTTP timeout, capped page/retry loops.
- `refresh_topic_scope`: finite source traversal and context-bound ingestion; downstream work is enqueued, distillation input is bounded.

No larger-runtime exceptions. The final `refresh_slack_groups` and `import_bamboohr` entries are non-heartbeat, with unchanged **5-minute** and **10-minute** leases and zero MaxRuntime. Runtime cancellation is cooperative, matching existing lease timeout semantics; ignoring a context is not forcibly preempted.

Negative control retained the additive Entry field for compilation but restored the old unbounded heartbeat timeout selection:

```text
=== RUN TestRunOne_HeartbeatMaxRuntimeMatchesLeaseTimeout/heartbeat/retry
blocking handler exceeded one-second timeout ceiling
=== RUN TestRunOne_HeartbeatMaxRuntimeMatchesLeaseTimeout/heartbeat/exhausted
blocking handler exceeded one-second timeout ceiling
--- FAIL: TestRunOne_HeartbeatMaxRuntimeMatchesLeaseTimeout (2.07s)
```

Restored deadline behavior:

```text
--- PASS: TestRunOne_HeartbeatMaxRuntimeMatchesLeaseTimeout (0.95s)
    --- PASS: .../lease/retry (0.28s)
    --- PASS: .../lease/exhausted (0.23s)
    --- PASS: .../heartbeat/retry (0.22s)
    --- PASS: .../heartbeat/exhausted (0.22s)
--- PASS: TestRunOne_HeartbeatWithoutMaxRuntimeKeepsParentContext (0.01s)
final heartbeat types: [backfill_identifiers recompute_person_distance refresh_slack_members refresh_topic_scope]
--- PASS: TestRegisterAll_HeartbeatJobsHaveMaxRuntime (0.00s)
```

These tests assert persisted deadline errors, attempt counts, cleared lease/lock fields, retry versus terminal failure, and that non-heartbeat jobs ignore MaxRuntime. The re-registration lease test passed in the full graph suite.

## 4. agent-mem-6nx — reclaim abandoned NULL leases

The janitor includes running NULL-lease rows whose lock is older than the constant 30-minute grace. Ordering uses `COALESCE(lease_until, locked_at)`. Existing expired-lease behavior and retry count are unchanged.

A direct single scan seeds one NULL-lease row locked an hour ago and another locked one minute ago. Before the fix:

```text
reclaimed=0, want 1
job 5: status=running cleared=false attempts=1, want queued/true/1
--- FAIL: TestJanitorScan_NullLeaseGrace (0.01s)
```

After the fix:

```text
--- PASS: TestJanitorScan_NullLeaseGrace (0.03s)
```

Asserts exactly one reclaim, old row queued with cleared locks, fresh row still running, and both attempts unchanged.

## 5. agent-mem-zn0 — configured runner routing

All seven hardcoded backfill runner fields use `deps.Runner`. Empty values reach the existing queue default `any`. No compatibility aliases or operational row retargeting were added.

Restoring only the historical API enqueue's hardcoded runner:

```text
.../configured_local: target_runner = "vps", want "local" for deps.Runner = "local"
.../empty_uses_queue_default: target_runner = "vps", want "any" for deps.Runner = ""
--- FAIL: TestBackfillSlackHandler_TargetRunner (0.04s)
```

Restored fix:

```text
--- PASS: TestBackfillSlackHandler_TargetRunner (0.03s)
    --- PASS: .../configured_local (0.02s)
    --- PASS: .../empty_uses_queue_default (0.01s)
```

Literal search of non-test Go sources found `"vps"` only in `config/config.go` and comments in `handlers.go`, `dispatcher.go`, `manager.go`, and `queue.go`.

## Runtime smoke, independent of tests

Ran a throwaway Go command with an explicit scratch-host/port/database guard, a real ephemeral localhost HTTP server, actual handlers/queue, janitor loop and dispatcher. It did not instantiate application configuration or access settings. Observed:

```text
HTTP backfill: 202, stored target_runner=local
Janitor runtime: abandoned NULL lease -> queued
Heartbeat runtime: failed after 217ms, last_error=context deadline exceeded
```

The command removed its fixture jobs; removed the throwaway source after the run.

## Full acceptance

```text
$ go build ./...
(no output; exit 0)
$ go vet ./...
(no output; exit 0)
```

Both rerun after the final client-resolver/test correction.

```sh
env -i PATH="$PATH" HOME="$HOME" AGENT_MEM_EVAL= \
  make test-db TEST_DATABASE_URL="$DSN"
```

One full serialized acceptance run: **1,536 test/subtest PASS, 0 FAIL, 3 SKIP**, across 14 packages; exit 0. Exactly the three skips documented by `report-test-hygiene.md`: unsupported attachment MIME network test, RichPDF missing `lit`, and deliberately disabled topic-judge live eval. No missing-DB-variable skip.

| Package | Seconds |
|---|---:|
| `graph` | 0.594 |
| `graph/acl` | 1.226 |
| `graph/bfs` | 0.581 |
| `graph/entities` | 0.516 |
| `graph/extractor` | 0.365 |
| `graph/fetchers` | 0.932 |
| `graph/handlers` | 45.886 |
| `graph/hydrate` | 0.634 |
| `graph/identity` | 0.361 |
| `graph/ids` | 0.39 |
| `graph/jobs` | 51.621 |
| `graph/normalizer` | 0.909 |
| `graph/scoring` | 0.778 |
| `graph/temporal` | 0.84 |

Also ran `go test -p=1 -count=1 -v ./internal/worker/... ./internal/graph/identity/... ./internal/graph/extractor/... ./internal/sync/...` with both scratch DB variables: four packages passed, no skips. Two independent read-only reviewers covered processor/guards and jobs/routing; the captured-client regression gap identified by review was corrected and proved red/green before delivery.

## Delivery safety

Removed the owned `fix-jobs-hardening` scratch container after verification. The copied plan's SHA-256 matches the absolute-path original: `d024bdc531373238d18a08c315e601e8146a55d8e387fabfb6e9d83849c04e26`.

The active Git hooks contain only Beads integration commands. Git delivery uses a system-tool-only PATH (`/usr/bin:/bin:/usr/sbin:/sbin`), where `bd` is absent, to obey the user's prohibition on further Beads execution. Hook files and Git configuration are unchanged; no remote push-protection bypass is used.
