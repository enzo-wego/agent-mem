# Phase 3 — the stale-summary sweep must skip orphaned rows

Worker brief. Self-contained: assume no knowledge of the conversation that produced it.

Issue: `agent-mem-9ll` (P1) — run `bd show agent-mem-9ll` for the recorded evidence.
Repo: `/Users/neocapitelo/go/src/github.com/agent-mem`, branch `main`, clean at the merge
of PR #33.

## What is broken

`POST /api/graph/backfill/stale-summaries` (added in `agent-mem-8q4`, shipped in PR #33)
is live on the production hub and does nothing. Run on 2026-08-17 it returned
`{matched:20, enqueued:20, remaining:1800}`, all 20 jobs reached `status=done` with no
error, and the `v9` count in `graph.thread_summaries` did not move.

`BackfillStaleThreadSummaries` (`internal/graph/handlers/summarize_thread.go`) orders
`ORDER BY updated_at ASC`, with the stated intent that repeated capped runs make
monotonic progress. That only holds if the selected row's `updated_at` actually advances.

It does not for a thread whose underlying Slack nodes are gone. `summarizeThreadHandler`
loads the thread's messages and hits:

```go
if count == 0 {
    return nil // nothing ingested yet; /topics re-enqueues on next view
}
```

It returns before touching `graph.thread_summaries`. Job `done`, no error, `updated_at`
unchanged, `signature` still `v5`. So the sweep re-selects the same rows on the next run,
and the next, forever.

Measured on production:

| stale rows | count | `updated_at` window |
|---|---|---|
| with no live nodes (orphans) | 154 | 2026-07-09 → 2026-07-23 |
| with live nodes | 1,646 | 2026-07-09 → 2026-08-17 |

The 154 orphans sit in the oldest window, so oldest-first selects them first. At the
default cap of 20 this is not "8 wasted runs" — it is a permanent wall. The sweep can
never reach a single real thread.

The underlying truncation fix is fine and verified end-to-end in production. Only the
sweep's row selection is broken.

## Goal

The sweep selects only rows whose thread still has live nodes, so every run makes real
progress and `remaining` means work that can actually be done.

## Non-goals — do NOT do these

- **Do NOT delete the 154 orphaned `thread_summaries` rows**, and do not add anything that
  deletes them. Whether summaries of content the graph no longer holds should be pruned is
  a separate decision, deliberately not folded in here.
- **Do NOT change `summarizeThreadHandler`'s `if count == 0 { return nil }` early return.**
  It is correct: a thread with nothing ingested must not be summarized, and the `/topics`
  path relies on re-enqueueing later.
- **Do NOT make the handler bump `updated_at` on the skip path** as a workaround. That
  would silently mark orphaned rows as fresh and corrupt the sync cursor
  (`GetThreadSummariesSince` keys on `updated_at`).
- **Do NOT wire the sweep into startup or any schedule.** It stays POST-only.
- **Do NOT run the sweep.** The conductor runs it against prod.
- Do not touch `flattenLines`, `threadSummarySigVersion`, or the summarizer prompt.
- Do not deploy. Do not commit or push.

## The change

### 1. Filter orphans out of the sweep query

In `BackfillStaleThreadSummaries` (`internal/graph/handlers/summarize_thread.go`), add a
live-node `EXISTS` guard to **both** the capped `SELECT` and the `remaining` `COUNT(*)`,
so the two agree:

```sql
AND EXISTS (
  SELECT 1 FROM graph.nodes n
  WHERE n.scope = 'slack:' || ts.channel_id
    AND COALESCE(NULLIF(n.metadata->>'thread_ts',''), split_part(n.id,':',3)) = ts.thread_ts
    AND n.deleted_at IS NULL)
```

You will need to alias `graph.thread_summaries` as `ts` in both statements.

This mirrors the join `BackfillMissingThreadSummaries` (same file) already uses to find
real threads — read it first and keep the two consistent in style.

Update the function's doc comment to say why the guard exists: without it, a thread whose
nodes are gone is selected forever, because the handler returns before `updated_at` can
advance. Reference `agent-mem-9ll`.

### 2. Keep the ordering

`ORDER BY updated_at ASC` stays. With orphans excluded, every selected row now runs a real
summarize that writes a new signature and a fresh `updated_at`, so oldest-first genuinely
advances.

### 3. Test

`internal/graph/handlers/` unit tests cannot exercise this without a database, and you must
not stand up a fake one. So instead:

- Extract the sweep's `WHERE`/`EXISTS` predicate into a package-level SQL string constant
  (for example `staleSummarySelectSQL` / a shared `staleSummaryWhereSQL` fragment) used by
  both the `SELECT` and the `COUNT(*)`, and add a unit test asserting the fragment is
  present in both statements and mentions `deleted_at IS NULL`. That pins the two queries
  agreeing, which is the specific thing that would silently rot.
- If you judge a string-matching test to be low value, say so in your report and explain
  what you did instead. Do not add a DB-backed test, and do not add a test that always
  passes.

Keep the existing `TestResolveStaleSummariesLimit` and
`TestBackfillStaleSummariesHandlerRejectsLargeLimit` passing.

## Verification — run these and paste the real output

```bash
cd /Users/neocapitelo/go/src/github.com/agent-mem
go build ./...
go vet ./internal/graph/handlers/
go test ./internal/graph/handlers/ -count=1
```

**Database warning.** Tests in this package skip unless `DATABASE_URL` is set. Leave it
unset — those tests truncate graph tables, and the hub is live production. If a test tries
to connect instead of skipping, stop and report it.

Do **not** connect to the production database to check your query. The conductor will
validate the SQL read-only against prod.

## Acceptance criteria

1. Both the capped `SELECT` and the `remaining` `COUNT(*)` carry the same live-node
   `EXISTS` guard.
2. `ORDER BY updated_at ASC` retained.
3. Nothing deletes or mutates `thread_summaries` rows.
4. `summarizeThreadHandler`'s `count == 0` early return is unchanged.
5. Sweep still reachable only via the POST — no startup path, no job registry, no
   scheduler. Grep your own diff and confirm.
6. Doc comment explains the guard and references `agent-mem-9ll`.
7. `go build ./...` and `go vet` clean; full package test run passes with DB-backed tests
   skipping, not failing.
8. No TODO placeholders, no `t.Skip` added, no stubbed or always-passing assertions.

## Report back

The diff, the verbatim output of all three commands, and your call on the test approach in
§3. Do not commit or push; the conductor reviews, ships and runs the sweep.
