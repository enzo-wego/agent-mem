# Plan: five small hardening fixes (97s, 7bs, cg2, 6nx, zn0)

REVIEWER: pi (auto: GPT weekly 22% left)

All five were checked against origin/main `e2b1b27` on 2026-10-05 by a read-only
triage. Read each issue in full with `bd show <id>` before starting. Each fix is
independent and small. Do them as five separate commits on one branch, each with
its own regression test.

## 1. agent-mem-97s: flat processor loses messages when no LLM is configured

`internal/worker/processor.go`: `processMessage` returns `nil` when
`s.getFlatLLM() == nil` ("No LLM configured, skipping"), and the caller then
calls `s.db.MarkMessageProcessed`. The message is consumed and never processed.

Fix: the loop must not claim a message when no LLM is configured. In the loop
that calls `ClaimPendingMessage`, check `s.getFlatLLM() == nil` first. If it's
nil, log once at WARN, sleep the normal poll interval, and claim nothing.
Messages stay `pending`. In `processMessage`, replace `return nil` with an error
(`errNoLLM`) as a second line of defence, so a race that gets past the first
check requeues the message instead of marking it processed. Test: with no LLM
configured, one pending message stays `pending` after a loop iteration
(not `processed`, not `failed`).

## 2. agent-mem-7bs: two test helpers can wipe a live DB

`internal/graph/identity/testdb_test.go` (`openTestDB`, lines ~12-30) and
`internal/graph/extractor/extractor_test.go` (`openTestDB`, ~19-35) have no
database-name guard. `internal/graph/handlers/testdb_test.go:25-40` does: skip
when `DATABASE_URL` is empty, and `t.Fatalf` unless `databaseName(dsn) ==
"agentmem_test"`. Copy that guard, and the `databaseName` helper if it isn't
reachable from those packages, into both. Fail closed when the DSN doesn't
parse. Test: run each package with `DATABASE_URL` pointing at a database named
`postgres` on your scratch server. Both must fail fast with the refusal message
(paste it). Never point it at 5433.

## 3. agent-mem-cg2: heartbeat jobs have no timeout ceiling

`internal/graph/jobs/worker.go:54-58` applies `context.WithTimeout(ctx, Lease)`
only when `!d.entry.Heartbeat`. Heartbeat types (`refresh_slack_groups`,
`import_bamboohr`, `recompute_person_distance`, `refresh_topic_scope`,
`backfill_identifiers`, `refresh_slack_members`; see `registry.go` /
`handlers.go`) can therefore run forever. Fix: add `MaxRuntime time.Duration`
to the registry entry. When a heartbeat entry has `MaxRuntime > 0`, wrap the
handler ctx in `WithTimeout(ctx, MaxRuntime)`. Set 30 min as the default for
every heartbeat entry, unless reading a handler shows it legitimately runs
longer. In that case set a larger value and say why in the report. Test: a
heartbeat entry with `MaxRuntime = 200ms` and a handler that blocks on
`<-ctx.Done()` returns within about 1s, and the job is marked failed or retried
the way a lease timeout is today (match the existing non-heartbeat timeout
path).

## 4. agent-mem-6nx: janitor skips running jobs with a NULL lease

`internal/graph/jobs/janitor.go:65-67` selects `status = 'running' AND
lease_until IS NOT NULL AND lease_until < NOW()`. Fix: also reclaim `status =
'running' AND lease_until IS NULL AND locked_at < NOW() - <grace>`, with grace
= 30 min (const). Update the `ORDER BY` so NULL-lease rows are included (e.g.
`ORDER BY COALESCE(lease_until, locked_at)`). Test: seed a `running` row with
`lease_until NULL` and `locked_at` 1 h ago, plus one with `locked_at` 1 min ago.
One janitor scan requeues only the first.

## 5. agent-mem-zn0: hardcoded runner "vps"

`TargetRunner: "vps"` is hardcoded at `handlers/backfill_api.go:62`,
`backfill_slack.go:72,100,119,347`, `backfill_slack_thread.go:86` and
`ingest_content.go:407`. The hub only works because its runner env var is set to
`vps`. Fix: use `deps.Runner` (`handlers.go:50`), the way
`refresh_jira_board.go` does. Where `deps.Runner` is empty, keep today's
behaviour: grep for an existing default and follow it, never inventing a new
one. After the change, `git grep -n '"vps"' -- internal ':!*_test.go'` must
match only `config.go` and comments. Test: one handler test that sets
`deps.Runner = "local"` and asserts the enqueued job's `target_runner` is
`local`.

## Rules

- Branch `fix/jobs-hardening` off `origin/main`, one commit per fix.
- Scratch DB: throwaway `pgvector/pgvector:pg16` on a free port (check
  `docker ps`), database `agentmem_test`, `go run ./cmd/agent-mem migrate`.
  Run the full graph suite with `make test-db
  TEST_DATABASE_URL=<scratch DSN>`. Never use the dev DB on 5433 or the hub.
- Each new test must fail without its fix. Paste one red run per fix (revert
  the fix temporarily, or run it on `origin/main`).
- No API keys, tokens or settings reads. No PR, merge or deploy.

## Acceptance criteria

1. Five commits, each with a test that was red before and is green after
   (outputs pasted).
2. `go build ./...` and `go vet ./...` are clean.
   `make test-db TEST_DATABASE_URL=<scratch>` passes, with SKIPs limited to the
   three documented in `docs/ai/report-test-hygiene.md`. Also run
   `go test ./internal/worker/...` and `./internal/graph/identity/...
   ./internal/graph/extractor/...` with both DB variables set.
3. The `"vps"` grep in item 5 is clean.
4. `docs/ai/report-jobs-hardening.md` with the per-fix evidence and any
   `MaxRuntime` exceptions.
5. Push `fix/jobs-hardening`.

## Revision 2 (answers pi review 1, 3 findings). Overrides the sections above where they differ.

1. **errNoLLM never uses up a message's retry budget** (fix 1). In
   `processor.go:80-108`, ordinary errors are retried and the message fails for
   good on attempt 3. Route `errNoLLM` (match with `errors.Is`) to a requeue
   that **refunds** the attempt: add a DB method (e.g.
   `RequeuePendingMessageUncharged`) that sets the row back to `pending` with a
   delay and decrements `attempts` (floor 0), following
   `RequeuePendingMessage`'s style. Test: a message already at `attempts =
   maxMessageAttempts` that hits `errNoLLM` ends `pending`, with attempts not
   above the cap, and never `failed`.
2. **One client per message** (fix 1). `processor.go:138-140` and `217-219`
   fetch the client again and `return nil` when it's nil. Change
   `processMessage` to fetch `gc := s.getFlatLLM()` once, return `errNoLLM` if
   it's nil, and pass `gc` to the per-type functions. Remove their own
   fetch/nil-returns. Test: inject a client getter that returns non-nil on the
   first call and nil afterwards (or pass nil directly to the per-type function).
   The message must not be marked processed.
3. **Empty runner = queue default "any"** (fix 5). `jobs/queue.go:69-71` maps
   an empty `TargetRunner` to `"any"`. So `TargetRunner: deps.Runner`, where an
   empty `deps.Runner` gives `"any"`. On the hub `deps.Runner` is `vps` (from
   `AGENT_MEM_GRAPH_RUNNER`), so hub behaviour is unchanged. The model to follow
   is `refresh_jira_updates.go` (`runner := cfg.Runner`), not
   `refresh_jira_board.go`. Tests: `deps.Runner = "local"` gives `local`;
   `deps.Runner = ""` gives `any`. The grep rule in fix 5 stands.
4. **Scratch binding**, the same as for any DB step: export
   `DATABASE_URL`/`AGENT_MEM_TEST_DATABASE_URL` set to the scratch DSN and check
   host `127.0.0.1`, your port, and db `agentmem_test` before running anything.
   Migrate with `DATABASE_URL=<scratch> go run ./cmd/agent-mem migrate`.

## Revision 3 (answers pi review 2, 2 findings). Overrides R2 item 2's test and fix 2's test.

1. **Client tests** (replaces R2 item 2's test). Two separate tests:
   (a) success with one captured client: `processMessage` gets a working fake
   client once, the message is marked `processed`, even if later getter calls
   would return nil; (b) client lost between the loop's pre-claim check and
   `processMessage`: the getter returns non-nil for the loop check and nil
   inside `processMessage`. The message must end `pending`, with the attempt
   refunded, and not `processed` or `failed`. Never call the per-type functions
   with a nil client.
2. **DB guard tests** (replaces fix 2's test). Put the guard in a small pure
   function, `checkScratchDSN(dsn string) error`, in each of the two packages'
   test helpers (or one shared test-util package if it already exists). It's
   called by `openTestDB` **before** any connection. Table-driven unit test, no
   DB needed: `.../agentmem_test` passes; `.../agentmem`, `.../postgres` and a
   malformed DSN are each refused with the refusal message. For the red proof,
   paste a run on `origin/main` showing `openTestDB` there connects to a
   non-`agentmem_test` database on your scratch server without refusing. Point
   it at `/postgres` on the scratch server, never at 5433. Also keep one green
   integration run of each package against `agentmem_test`.

## Revision 4: final (answers pi review 3; approved by Enzo 2026-10-05 without a 4th review). Overrides earlier text where they differ.

1. **Guard on the database pgx will actually connect to** (fix 2, and also the
   existing handlers guard). `handlers/testdb_test.go:13` checks only the URL
   path, and pgx v5.8.0 lets `?dbname=` override it. So
   `.../agentmem_test?dbname=agentmem` passes today. `checkScratchDSN` must
   parse with `pgxpool.ParseConfig(dsn)` and check `cfg.ConnConfig.Database ==
   "agentmem_test"`. `openTestDB` must then connect with **that same parsed
   config** (`pgxpool.NewWithConfig`), not re-parse the raw string. Apply it in
   `identity`, `extractor` **and** `handlers/testdb_test.go`. Unit-test cases add:
   `.../agentmem_test?dbname=agentmem` is refused, and a key/value DSN
   `dbname=agentmem` is refused. Also check whether `search_semantic_floor_db_test.go`
   and `sync/graph_sync_test.go:39` share this hole, and fix any that do the same way.
2. **MaxRuntime only for jobs that really use a heartbeat** (fix 3).
   `Registry.Register` replaces entries, and `NewRefreshSlackGroupsHandler` /
   `NewImportBambooHRHandler` re-register without `Heartbeat`, keeping their
   existing 5- and 10-minute timeouts. Keep those timeouts unchanged. Apply
   `MaxRuntime` only to entries that have `Heartbeat == true` after
   `RegisterAll`. Test: build the real registry the way the worker does, list
   every entry with `Heartbeat == true`, and assert each has `MaxRuntime > 0`.
   Also assert the two re-registered types keep their existing lease timeouts.
   List the final heartbeat set in the report. The synthetic 200 ms test stays.
