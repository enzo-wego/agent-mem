# Drop graph.jobs from the Sync Status table

## Goal

Remove `graph.jobs` from the Sync Status list on the dashboard Sync tab.

## Why

`graph.jobs` is a per-machine **work queue**, not shared memory. The two sides are
supposed to diverge, so showing it next to the memory tables invites the reader to
treat a normal difference as a fault. Measured 2026-08-05: local ~10.8k rows (its
jobs table was reset earlier that day) vs VPS ~616.6k (583k done + 33k failed, four
weeks of unpruned history, `agent-mem-xag`). Neither number tells the reader
anything actionable.

It is also the most expensive row on the page: `GetSyncStats` runs an unfiltered
`COUNT(*)` per table, and `graph.jobs` is by far the largest, so dropping it makes
the tab cheaper as well as clearer.

## Non-goals

- Do NOT remove any other table from the list.
- Do NOT change the sync engine, the pull/push paths, or what actually syncs.
  This is a display change only; `graph.jobs` keeps syncing exactly as it does now
  (whether it *should* is `agent-mem-u3t` / `agent-mem-6nx`, a separate question).
- Do NOT touch the Flat Memory / Graph Memory grouping.
- Do NOT prune the jobs table.

## Files expected to change

1. `internal/database/sync.go` — drop `"graph.jobs"` from the `tables` slice in
   `GetSyncStats`.
2. `internal/worker/dashboard/**` — only if the frontend changes; it should not
   need to (the UI renders whatever rows the API returns), so expect **no**
   frontend edit and no embed rebuild.

## Approach

In `GetSyncStats`, remove `"graph.jobs"` from the slice and leave a short comment
saying why, so nobody adds it back as an oversight:

```go
// graph.jobs is deliberately absent: it is a per-machine work queue, not shared
// memory, so local and cloud are expected to differ. It was also the largest
// table on the page, making its COUNT(*) the slowest cell.
```

Since the frontend groups rows by the `graph.` prefix, the Graph Memory section
simply renders one row fewer. No UI change.

## Acceptance criteria

1. `GET /api/sync/info` returns 12 rows, with no `graph.jobs` entry.
2. The remaining 12 are unchanged in both name and order.
3. The Sync tab still renders both Flat Memory and Graph Memory sections.
4. No frontend file changed and no embedded dashboard rebuild.
5. `go build ./...` and `go vet ./...` clean.
6. No TODO placeholders.

## How to verify

```bash
go build ./... && go vet ./...
```

Then the conductor restarts the worker and confirms `/api/sync/info` has 12 rows
and no `graph.jobs`.
