# Plan: real retry backoff for pending_messages (4g8) + test-DB guard (z14)

Repo: `~/go/src/github.com/agent-mem`. Branch off `main`:
`fix/retry-backoff` (create it; `main` already has the attempts budget and the
tolerant JSON parsers).

Two independent issues, both small, both in the same commit-sized area of the
worker. Do them in one branch.

---

## Part A — `agent-mem-4g8`: the retry budget has no backoff (P1)

### Why

`main` gives every pending message three attempts before
`MarkMessageFailed` (terminal). That budget currently buys nothing.
`RequeuePendingMessage` sets `status = 'pending'` and nothing else;
`processLoop` ticks every second and `ClaimPendingMessage` selects any
`'pending'` row ordered by `created_at`. So all three attempts burn in about
three seconds, against an identical LLM state, and the message lands in
`'failed'` anyway.

Evidence: after the budget shipped, every surviving `pending` row still shows
`attempts = 0` — nothing ever sat in the queue long enough to be observed
mid-retry.

A retry that happens 30 seconds later is a genuinely different roll: a
different sampling of the model, a gateway that may have restarted, a quota
window that may have rolled. A retry three seconds later is the same roll
three times.

`graph.jobs` already solved this exact problem. Mirror it rather than
inventing a second mechanism.

### Change

**1. Migration** `migrations/20260801000006_pending_messages_available_at.sql`

```sql
ALTER TABLE pending_messages
  ADD COLUMN available_at TIMESTAMPTZ NOT NULL DEFAULT NOW();
```

Down: drop the column. Comment it with *why* the column exists (a requeued
message must not be re-claimed on the next 1s tick), following the style of
the recent migrations in that directory — say why, not what the DDL does.

Backfilling existing rows to `NOW()` via the DEFAULT is correct: every row
already in the table is claimable today and must stay claimable.

**2. `internal/database/pending.go`**

- `ClaimPendingMessage` — add `AND available_at <= NOW()` to the inner
  `SELECT`, and order by `available_at ASC, created_at ASC`. Keep the
  `FOR UPDATE SKIP LOCKED` and the `attempts = attempts + 1` exactly as they
  are; the attempt must stay inside the claim.
- `RequeuePendingMessage` — take a new `delay time.Duration` parameter and set
  `available_at = NOW() + ($3 || ' seconds')::interval`. Follow the parameter
  style of `jobs.Retry` in `internal/graph/jobs/state.go:33-42`, which passes
  the seconds as a string and casts. Keep preserving `attempts`.
- `RequeueRetryablePendingMessage` — leave the timing alone. It already
  refunds the attempt, and the caller sleeps via `backoffLLM`, so a gateway
  outage must not additionally park the message. Add one line to its doc
  comment saying that is deliberate, or the next reader will "fix" it.
- Update `PendingMessageCount`'s doc comment if it now over-reports: it counts
  `status = 'pending'` regardless of `available_at`, which is still the right
  queue-depth gauge. Say so rather than changing it.

**3. `internal/worker/processor.go`**

At the non-retryable-failure branch that currently calls
`RequeuePendingMessage`, compute the delay from the attempt count and pass it:

```go
jobs.Backoff(int16(msg.Attempts), 30*time.Second, 10*time.Minute)
```

`jobs.Backoff` (`internal/graph/jobs/backoff.go:14`) is exponential with ±20%
jitter and already tested. Reuse it — do not write a second backoff. If
importing `internal/graph/jobs` from `internal/worker` creates an import
cycle, say so in the report and copy the three-line calculation with a comment
pointing at the original; do **not** silently diverge the curve.

Log the delay alongside the existing attempt count so the log explains when
the message will come back.

Base 30s / cap 10m is chosen so a three-attempt lifetime is roughly 90 seconds
rather than 3 seconds, without parking a legitimately transient failure for
long enough to matter to the dashboard.

### Tests (`internal/worker/processor_test.go`)

The harness already exists and already refuses a non-test database — extend
it, do not invent a new one.

- After a non-retryable failure, the row's `available_at` is in the future
  (assert `> NOW()`, not an exact value — the backoff is jittered).
- A message whose `available_at` is in the future is **not** claimed:
  `processPendingMessages` leaves it `pending` with its attempt count
  unchanged. This is the test that actually proves the bug is fixed.
- The existing three tests still pass. The
  `ThirdNonRetryableFailureIsTerminal` test will now need to push
  `available_at` back between passes (an `UPDATE ... SET available_at = NOW()`
  in the test body is fine and honest — say why in a comment).

---

## Part B — `agent-mem-z14`: `openTestDB` will still wipe the dev database (P1)

### Why

On 2026-07-14 a handler integration test ran with `DATABASE_URL` pointed at
the live dev database. `truncateGraphHandlerTables`
(`internal/graph/handlers/testdb_test.go:33`) hard-deleted every graph table —
~21k nodes down to 3 — and the sync engine pushed the test fixtures to
production. Recovery took a full re-pull.

`internal/worker/processor_test.go` got a guard when it was written. This
helper, the one that actually caused the incident, still has none.

### Change

`internal/graph/handlers/testdb_test.go`, in `openTestDB`, after the
`DATABASE_URL` empty check and **before** `pgxpool.New`: parse the DSN and
`t.Fatalf` unless the database name contains `"test"`. A DSN that fails to
parse must fail closed (empty name → fatal).

Copy the shape from `internal/worker/processor_test.go:18-51` — same
`databaseName` helper, same fail-closed behaviour, same comment pointing at
the incident and at `agent-mem-z14`. They are different packages, so a small
duplicated helper is correct; do not create a shared testing package for
twelve lines.

Guard `openTestDB` itself rather than `truncateGraphHandlerTables`: every
caller of the former is an integration test, and catching it at connect time
means the failure names the real problem before anything touches a table.

### Test

One test that the guard fatals on a non-test DSN is hard to write without
`t.Fatalf` killing the test itself. Do not contort the code for it. Instead
verify by hand and paste the output into your report:

```bash
DATABASE_URL='postgres://agentmem:agentmem@localhost:5433/agentmem_guardcheck' \
  go test ./internal/graph/handlers/ -run TestPins 2>&1 | head
```

It must fatal on the database *name*, before any connection attempt — an
error mentioning "connection refused" instead means the guard is in the wrong
place.

---

## Verify

```bash
cd ~/go/src/github.com/agent-mem
unset DATABASE_URL          # integration tests DELETE rows if this is set
go build ./... && go vet ./... && go test ./...
#   4 failures in internal/hooks + internal/skills are PRE-EXISTING on main
#   (agent-mem-1ng) — leave them alone

DATABASE_URL='postgres://agentmem:agentmem@localhost:5433/agentmem_test' \
  go test ./internal/worker/ -v
```

`agentmem_test` exists on localhost:5433. Never point `DATABASE_URL` at
`agentmem`.

If `httptest` fails with `bind: operation not permitted`, ask for approval to
run outside the sandbox. Do **not** delete, skip or weaken a test to get
around it.

## Rules

- **Do not deploy, and do not touch the VPS.** Production is paused and the
  lead owns unpausing it.
- **Do not change `processing_paused`** in either direction.
- **Do not requeue the failed rows.** 8,695 rows in `'failed'` are a separate
  decision and a bulk LLM spend.
- **Do not run `bd` or `dolt`.** They are not on your PATH and will waste your
  time.
- **Do not commit or push.** Hand back the working tree; the lead reviews,
  commits and ships.
- Report honestly what you did not finish. A named gap is worth more than a
  green summary.
