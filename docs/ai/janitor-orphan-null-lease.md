# Plan — `agent-mem-h06j`: the janitor cannot reap a `running` job with a NULL lease

**Repo:** `~/go/src/github.com/agent-mem` · **Branch off:** `main` · **Verified against** `8e81b0c`

Small, self-contained. Found while diagnosing the hot-topic scope refresh
(`docs/ai/hot-topic-scope-refresh-repair.md`) but unrelated to it — do not bundle the commits.

---

## The defect

On the hub right now:

```
   id    |       type        | status  | attempts |              locked_by               |           locked_at           | lease_until
 4812378 | detect_hot_topics | running |        1 | a0411a4a-81c4-401c-8b78-3c068f373f5c | 2026-08-18 17:27:12.065536+00 | NULL
```

Six days `running`. It will never move again. `Janitor.scan` reclaims only rows whose lease has a
value **and** has passed (`internal/graph/jobs/janitor.go:62-67`):

```sql
WHERE status = 'running'
  AND lease_until IS NOT NULL
  AND lease_until < NOW()
```

A `running` row with `lease_until IS NULL` matches nothing. It is invisible to the janitor, invisible
to `Claim` (which only takes `status='queued'`, `queue.go:100-106`), and holds a `locked_by` that no
longer exists. It is not currently *harmful* — the in-memory pool is per-process and the worker has
restarted many times since — but it is a permanent lie in the queue and it hides a class of leak.

## How the row got there — unproven

No code path produces it. `Claim` sets `locked_by`, `locked_at` and `lease_until` together
(`queue.go:110-116`). Every terminal transition — `Complete`, `Retry`, `RetryRefund`, `Fail`
(`internal/graph/jobs/state.go:9-90`) — clears all three at once. `StartHeartbeat` only bumps the
lease forward (`heartbeat.go:59`). The `lease_until` column has existed since
`migrations/20260527000003_graph_jobs_lease.sql` (May 27), so this is not a migration artifact.

The date lines up with the 2026-08-18 quota-burn incident response, which included hand-written
queue pruning (`5f3c52a`, `333573a`, `b90fc5e`). A manual `UPDATE` is the most plausible origin.
**Do not spend time proving it** — the point is that the janitor's invariant is too narrow to survive
a row it did not create.

## Goal

A `running` job that no live worker is heartbeating gets reclaimed, whether or not it has a lease.

## Non-goals

- No new column, no new table, no job-ownership registry, no worker liveness heartbeat.
- Do not change `Claim`, the pool, or any state transition in `state.go`.
- Do not touch `detect_hot_topics` — the job *type* here is incidental.

## Approach

Widen the janitor predicate to treat a missing lease as an expired one, using `locked_at` plus a
generous grace so a genuine in-flight job is never stolen:

```sql
WHERE status = 'running'
  AND (
        (lease_until IS NOT NULL AND lease_until < NOW())
     -- ponytail: a running row with no lease is a leak, not a long job. 1h grace
     -- is >> the longest lease in the registry (600s) plus heartbeat slack.
     OR (lease_until IS NULL AND locked_at < NOW() - INTERVAL '1 hour')
      )
ORDER BY COALESCE(lease_until, locked_at) ASC
```

`ORDER BY lease_until ASC` (`janitor.go:67`) must become the `COALESCE` above, or NULL-lease rows
sort last under Postgres' `NULLS LAST` default and starve behind the batch limit.

The longest `Lease` in the registry is 600s (`handlers.go:88-104`, `registry.go:44-53`), and
`Heartbeat: true` handlers extend it while alive. One hour is therefore unambiguous: no honest job
reaches it with a NULL lease.

Note the pre-existing index `idx_jobs_stuck btree (locked_at) WHERE status = 'running'` already
supports the new branch — it exists and is currently unused by any query.

## Files expected to change

| File | Change |
|---|---|
| `internal/graph/jobs/janitor.go` | widen the `scan` predicate + fix the `ORDER BY` |
| `internal/graph/jobs/janitor_test.go` | one case: `running` + NULL lease + old `locked_at` is reclaimed; one case: `running` + NULL lease + `locked_at` 5 min ago is **not** |

## Acceptance criteria

1. Both new test cases pass; the existing janitor tests still pass unchanged.
2. On the hub after deploy, `4812378` returns to `status='queued'` with
   `last_error` containing `janitor: lease expired`, and then either completes or fails through the
   normal retry path.
3. `SELECT count(*) FROM graph.jobs WHERE status='running' AND lease_until IS NULL
   AND locked_at < now() - interval '1 hour'` is `0` after one janitor scan interval
   (30s, `internal/worker/server.go:294`).
4. No job that is genuinely in flight is reclaimed: watch one `Heartbeat: true` handler
   (`refresh_slack_groups` or `import_bamboohr`) run to completion across a janitor tick.

## How to verify on the hub

```bash
ssh enzo@payments 'cd ~/go/src/github.com/agent-mem && git pull --ff-only \
  && PATH=/opt/homebrew/bin:$PATH docker compose up -d --build worker'

ssh enzo@payments 'PATH=/opt/homebrew/bin:$PATH docker exec agent-mem-postgres-1 psql -U agentmem \
  -d agentmem -c "SELECT id,type,status,attempts,lease_until,left(last_error,120) \
  FROM graph.jobs WHERE id=4812378;"'

ssh enzo@payments 'PATH=/opt/homebrew/bin:$PATH docker logs --since 5m agent-mem-worker-1 2>&1 \
  | grep -i "requeued expired leases"'
```

## Known landmines

- **`ORDER BY` with NULLs.** Forgetting the `COALESCE` makes the fix silently no-op whenever the
  batch (`JanitorBatchSize: 100`, `server.go:295`) is full of lease-bearing rows.
- **Grace too short.** Anything under the longest lease (600s) risks reclaiming a live job and
  running it twice. Handlers are not all idempotent — do not tune this down.
- **`attempts` is not refunded** by the janitor (it appends to `last_error` and requeues,
  `janitor.go:69-79`). `4812378` will come back with `attempts=1` of `max_attempts=5`. That is the
  existing behaviour for expired leases; do not change it here.
