# Phase 2 — one signature constant, bump to v9, and a capped stale-summary sweep

Worker brief. Self-contained: assume no knowledge of the conversation that produced it.

Issue: `agent-mem-8q4` (P1) — read `bd show agent-mem-8q4` for the recorded findings.
Parent plan: `docs/ai/thread-summary-firstline-plan.md`.
Repo: `/Users/neocapitelo/go/src/github.com/agent-mem`, branch `main`.

## State of the working tree — read this first

Phase 1 (`agent-mem-t8r`) is **already applied and uncommitted** in the working tree:
`flattenLines` in `channels.go` plus four call sites switched to it, and
`TestFlattenLines`. Do not revert, re-do, or "clean up" any of it. Your changes land on
top. `git diff` will show both rounds mixed together; that is expected and the conductor
has a baseline snapshot to separate them.

## Background

Slack thread summaries are cached in `graph.thread_summaries`, keyed by a signature
string `v<N>:<message-count>:<newest-updated-ms>`. Bumping the `v<N>` prefix is how a
summarizer change invalidates every cached row.

Two problems, both verified against production on 2026-08-17.

**1. The prefix is hardcoded in two places that must stay in sync by comment.**

- `internal/graph/handlers/summarize_thread.go:115` — `fmt.Sprintf("v8:%d:%d", count, maxUpdated)`
  (writes the signature)
- `internal/graph/handlers/channels.go:559` — `fmt.Sprintf("v8:%d:%d", cnt, last)`
  (computes `liveSig` to detect staleness on view)

Both carry a "keep in sync" comment. If they ever diverge, every thread reads as
permanently stale and re-enqueues `summarize_thread` on every channel view. The cost is
bounded — `summarySkip` matches on the written signature and returns before the LLM call
— so it is permanent job churn rather than LLM burn. Still worth making impossible.

**2. A prefix bump does not actually regenerate the corpus.**

Staleness is detected only lazily, when a channel view renders
(`channels.go:576-579`: `stale := !ok || s == "" || cachedSig != liveSig` → enqueue).
Worker startup does not help: `BackfillMissingThreadSummaries`
(`summarize_thread.go:325`) ends in `WHERE ... ts.channel_id IS NULL`, so it matches
threads with **no** summary row and never a stale-signature one.

Production proves the consequence. `graph.thread_summaries` today:

| prefix | rows | window |
|---|---|---|
| v7 | 788 | 2026-07-13 → 2026-07-28 |
| v8 | 584 | 2026-07-28 → 2026-08-17 |
| v5 | 431 | 2026-07-09 → 2026-07-13 |
| v6 | 1 | 2026-07-13 |

The v7 → v8 bump shipped around 2026-07-28. Three weeks later **1,218 of 1,802 rows
(68%) are still on v5/v7** because nobody re-opened those channels. Without a deliberate
sweep, a v9 bump would strand the same majority on the truncated summaries Phase 1 exists
to fix.

Note all 1,802 rows need regenerating, the 584 v8 ones included — every one of them was
built by the truncating transcript builder. That is why the sweep must key off the new
version, not off "not v8".

## Goal

1. One constant as the single source of truth for the signature version.
2. Set it to `v9` so Phase 1's fix invalidates every cached summary.
3. A manually-triggered, capped, deduped HTTP endpoint that enqueues
   `summarize_thread` for rows whose signature is not on the current version.

## Non-goals — do NOT do these

- **Do NOT wire the sweep into worker startup or any periodic/heartbeat job.** It must
  fire only on an explicit POST. This mirrors `NewBackfillAttachmentsHandler`, whose doc
  comment says exactly this for the same reason (a bulk LLM re-run, `agent-mem-16e`).
- **Do NOT add a scheduled job, cron entry, or `available_at` self-reschedule.**
- **Do NOT run the sweep.** Ship the capability; the conductor runs it against prod
  behind a canary.
- Do not change `summarySkip`, `enqueueSummarizeThread`, `BackfillMissingThreadSummaries`,
  or the summarizer prompt.
- Do not touch `flattenLines` or anything else from Phase 1.
- Do not deploy. Do not commit or push.

## The change

### 1. The shared constant

Declare it once in `internal/graph/handlers/summarize_thread.go`, near the top, next to
the existing version-history comment at line ~107-115:

```go
// threadSummarySigVersion is the single source of truth for the thread-summary
// cache-key version. Bumping it invalidates every cached row in
// graph.thread_summaries. Every reader and writer of a signature MUST derive it
// from this constant — it used to be hardcoded in two places kept in sync by
// comment, and a divergence would make every thread read permanently stale.
//
// v5..v8 history: see the comment block below.
// v9: the LLM transcript builder no longer truncates each message at its first
// newline (agent-mem-t8r), so every summary cached under v5..v8 was written from
// a transcript missing everything after line 1 of every multi-line message.
const threadSummarySigVersion = "v9"
```

Keep the existing v2/v4/v8 history comment — it is useful provenance. Add the v9 line to
it or reference it as above; do not delete it.

Then a small helper beside the constant so no caller formats the prefix itself:

```go
func threadSummarySignature(count int, newestUpdatedMs int64) string
```

returning `fmt.Sprintf("%s:%d:%d", threadSummarySigVersion, count, newestUpdatedMs)`.

### 2. Use it at both existing sites

| file:line | current | becomes |
|---|---|---|
| `summarize_thread.go:115` | `fmt.Sprintf("v8:%d:%d", count, maxUpdated)` | `threadSummarySignature(count, maxUpdated)` |
| `channels.go:559` | `fmt.Sprintf("v8:%d:%d", cnt, last)` | `threadSummarySignature(cnt, last)` |

Delete the now-obsolete "keep this in sync with channels.go" / "keep in sync with
summarize_thread's signature format" comments at both sites — the constant makes them
untrue. Replace with a one-line pointer to the constant.

After this, `grep -rn '"v8:\|v8:%d' internal/` must return nothing in non-test code.

### 3. The sweep

Add to `internal/graph/handlers/backfill_api.go`, modelled on
`NewBackfillAttachmentsHandler` (same file, line ~96) — read it first and follow its
shape, naming, and response style.

**Query function**, in `summarize_thread.go` beside `BackfillMissingThreadSummaries`:

```go
func BackfillStaleThreadSummaries(ctx context.Context, db *pgxpool.Pool, limit int) (matched, enqueued int)
```

- Select `channel_id, thread_ts` from `graph.thread_summaries`
  `WHERE signature NOT LIKE threadSummarySigVersion || ':%'`.
- Order **oldest `updated_at` first** so repeated capped runs make monotonic progress
  instead of re-picking the same rows.
- `LIMIT $1`.
- Call `enqueueSummarizeThread` for each — it already dedups against a queued/running
  job for the same `(channel_id, thread_ts)`, so overlapping runs cannot pile up.
- Return `matched` (rows the query returned) and `enqueued` (how many actually got a job
  — `enqueueSummarizeThread` is best-effort and silently skips dupes, so you will need
  it to report back; if making it return a bool is intrusive, count matched rows and
  document that `enqueued` is best-effort. State which you chose and why).
- Also return the **total** stale count (a separate cheap `COUNT(*)` without the limit)
  so the operator can see how much work remains after a capped run. Add it to the
  response as `remaining`.

**HTTP handler**: `NewBackfillStaleSummariesHandler(deps Deps) http.Handler` for
`POST /api/graph/backfill/stale-summaries`.

- Optional JSON body `{"limit": N}`; ignore decode errors so an empty body works.
- Default limit **20**, not 1000. This is the canary cap and the safe default matters:
  each enqueued thread cascades `summarize_thread → index_artifact (force) → link_topics`,
  and `link_topics` is roughly 15 LLM judge calls per node. A careless 1000 is ~15k judge
  calls.
- Reject `limit > 500` with 400, matching the attachments handler's ceiling.
- Respond 202 with `{status, matched, enqueued, remaining, limit}`.

Register in `internal/graph/handlers/router.go` next to the two existing backfill routes
(lines 25-26).

### 4. Tests

Pure unit tests where possible, package `handlers`, no database. Put signature tests in
`channels_test.go` or a new `summarize_thread_sig_test.go` — your call, say which.

- `threadSummarySignature(3, 1785480000000)` returns `"v9:3:1785480000000"`.
- `threadSummarySigVersion` is `"v9"` — a literal assertion, so a future bump has to be
  deliberate and shows up in a failing test.
- `summarySkip` still behaves correctly when both signatures are on v9: reuse the
  existing cases in `summarize_thread_skip_test.go`. **Those two constants at lines 11-12
  are `"v8:3:..."` / `"v8:4:..."` literals** — update them to build from
  `threadSummarySignature` so they cannot rot on the next bump, and confirm the existing
  assertions still pass.
- Handler limit validation: `limit > 500` → 400; empty body → default 20. A
  `httptest.NewRequest`/`ResponseRecorder` test that does not need a live DB is
  preferred; if the handler cannot be exercised without a pool, say so and cover the
  limit clamp by testing the extracted clamp logic instead. Do not fake a DB.

Do not write a DB-backed test for `BackfillStaleThreadSummaries` — see the database
warning below.

## Verification — run these and paste the real output

```bash
cd /Users/neocapitelo/go/src/github.com/agent-mem
go build ./...
go vet ./internal/graph/handlers/
go test ./internal/graph/handlers/ -count=1
grep -rn 'v8:%d\|"v8:' internal/ --include=*.go
grep -rn 'threadSummarySigVersion\|threadSummarySignature' internal/ --include=*.go
```

The fourth command must show no non-test hits. The fifth must show the constant declared
once and used at both signature sites plus the sweep.

**Database warning.** Tests in this package skip unless `DATABASE_URL` is set. Leave it
unset. Never point them at the live dev or production database — those tests truncate
graph tables, and production is the live hub. If a test tries to connect instead of
skipping, stop and report it.

## Acceptance criteria

1. `threadSummarySigVersion = "v9"` declared exactly once, with a doc comment explaining
   why it is a constant and what v9 means.
2. `threadSummarySignature` used at both `summarize_thread.go:115` and
   `channels.go:559`; no hardcoded `v8:` or `v9:` prefix remains in non-test code.
3. The obsolete "keep in sync" comments are gone.
4. `BackfillStaleThreadSummaries` matches on `signature NOT LIKE <version>:%`, orders
   oldest-`updated_at` first, honours `limit`, and reports `remaining`.
5. `POST /api/graph/backfill/stale-summaries` registered in `router.go`; default limit
   20; `limit > 500` rejected with 400.
6. The sweep is reachable **only** via that POST — grep the diff and confirm it appears
   in no startup path, no job registry entry, no scheduler.
7. `summarize_thread_skip_test.go` constants derive from `threadSummarySignature`, and
   its existing assertions still pass.
8. `go build ./...` and `go vet` clean; full package test run passes with DB-backed
   tests skipping, not failing.
9. No TODO placeholders, no `t.Skip` added, no stubbed assertions, no fake DB.

## Report back

The diff summary, every file touched, the verbatim output of all five verification
commands, and anything you chose not to do or decided differently — including the two
judgement calls flagged above (how `enqueued` is counted, and where the signature tests
live). Do not commit or push; the conductor reviews and ships.
