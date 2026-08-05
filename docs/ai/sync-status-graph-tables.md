# Sync Status: show graph-memory tables

## Goal

The dashboard **Sync** tab shows sync counts for the four flat-memory tables only.
Graph memory already syncs — it is just invisible on this page. Add the nine
`graph.*` syncable tables to the Sync Status table, split into two labelled
sections: **Flat Memory** and **Graph Memory**.

Works in both modes:
- local mode → Total + Unsynced columns
- cloud mode → Total column only (handler zeroes Unsynced, see below)

## Non-goals

- Do NOT touch the **Cloud Statistics** card (`/api/stats`, `GetStats`). It stays
  Observations / Summaries / Prompts. Explicitly out of scope this round.
- Do NOT change the sync engine, the push/pull payloads, or any migration.
  Graph sync already works; this is a reporting-only change.
- No new API fields (see "Approach" — grouping is derived, not transmitted).
- No changes to Worker Health or Connected Clients cards.

Deploy IS in scope this round (both local and the VPS) — see "Deploy". The worker
agent does not deploy; the conductor does, after review.

## Background (verified, do not re-derive)

- `internal/sync/engine.go` already pushes and pulls all nine graph tables:
  `graph.people`, `graph.nodes`, `graph.edges`, `graph.artifact_index`,
  `graph.artifact_bodies`, `graph.slack_groups`, `graph.entities`, `graph.jobs`,
  `graph.user_affinity_config`.
- Every one of those tables has `sync_id UUID NOT NULL DEFAULT gen_random_uuid() UNIQUE`
  and `sync_version BIGINT NOT NULL DEFAULT 0`, each with a partial index
  `... (sync_version) WHERE sync_version = 0`
  (`migrations/20260527000001_graph_schema.sql`). So the existing unsynced
  predicate is index-backed on graph tables too.
- `internal/database/sync.go:407-418` `GetSyncStats` hardcodes the four flat
  table names. **This list is the entire gap.**
- `internal/worker/sync_handlers.go:300-313` calls `GetSyncStats` and, in cloud
  mode, zeroes `Unsynced` before returning. No change needed there.
- `dashboard/src/pages/Sync.tsx:87-110` renders `syncInfo.stats` generically, so
  the rows would already appear un-grouped; the frontend change is only for the
  two section headings.

## Files expected to change

1. `internal/database/sync.go` — extend the `tables` slice in `GetSyncStats`.
2. `dashboard/src/pages/Sync.tsx` — group rows under two subheadings.
3. `internal/worker/dashboard/**` — regenerated embedded build output (see Build).

Nothing else. If you find yourself editing the sync engine, migrations, or
`api.ts`, stop and re-read this plan.

## Approach

### 1. Backend (`internal/database/sync.go`)

In `GetSyncStats`, extend the table list to:

```go
tables := []string{
    "observations", "session_summaries", "user_prompts", "sdk_sessions",
    "graph.people", "graph.nodes", "graph.edges",
    "graph.artifact_index", "graph.artifact_bodies",
    "graph.slack_groups", "graph.entities", "graph.jobs",
    "graph.user_affinity_config",
}
```

Keep the two existing `fmt.Sprintf` count queries exactly as they are. The
schema-qualified names interpolate fine, and the flat predicate
`WHERE sync_id IS NOT NULL AND sync_version = 0` is correct for graph tables too
(`sync_id` is `NOT NULL` there, so the first clause is a no-op).

The list is a hardcoded literal — that is deliberate, it mirrors the tables the
sync engine actually ships. Do not introduce a registry, a config value, or an
`information_schema` discovery query for this.

### 2. Frontend (`dashboard/src/pages/Sync.tsx`)

Split the rendered rows into two groups and emit a full-width subheading row
before each non-empty group:

- **Flat Memory** — stats whose `table` has no `graph.` prefix
- **Graph Memory** — stats whose `table` starts with `graph.`

Derive the grouping from the name prefix in the component; do NOT add a `kind`
field to `SyncStats` / `api.ts`. Leave a brief comment noting the prefix is the
grouping key. Preserve current ordering within each group (backend order).

Keep the existing table markup, the `!isCloud` conditional on the Unsynced
column, and the existing Tailwind classes including the dark-mode variants.
Subheading rows should use a `<td colSpan=...>` matching the live column count
(3 in local mode, 2 in cloud mode).

Target rendering:

```
Table                          Total   Unsynced
Flat Memory
  observations               117,505          0
  session_summaries            7,476          0
  user_prompts                10,526          0
  sdk_sessions                 2,671          0
Graph Memory
  graph.people                   728          0
  graph.nodes                    ...          0
  ...
```

### 3. Build the embedded dashboard

Required after any `dashboard/src` change (per `AGENTS.md:59-69`):

```bash
cd dashboard && npm run build
cd .. && rm -rf internal/worker/dashboard/* && cp -R dashboard/dist/* internal/worker/dashboard/
```

Commit the regenerated `internal/worker/dashboard/` output with the source change.

## Acceptance criteria

1. `GetSyncStats` returns 13 rows: the 4 flat tables then the 9 graph tables.
2. `GET /api/sync/info` in local mode includes every `graph.*` row with a
   plausible `total` and an `unsynced` value.
3. The Sync tab renders **Flat Memory** and **Graph Memory** subheadings with the
   right rows under each, in both light and dark mode.
4. Cloud mode still shows only the Total column, no Unsynced column, and the
   subheadings still render.
5. `go build ./...` clean; `go vet ./...` clean.
6. `cd dashboard && npx tsc -b` clean (or `npm run build` succeeds, which runs it).
7. `internal/worker/dashboard/` regenerated and committed.
8. Cloud Statistics card unchanged.
9. No TODO placeholders, no skipped tests, no stubbed branches.

## How to verify

Run these and paste the real output — do not summarise.

```bash
go build ./... && go vet ./...
go test ./internal/database/... ./internal/sync/...
cd dashboard && npm run build
```

Then prove the endpoint against a **scratch DB, never the live dev DB**
(truncating the dev DB propagates graph + fixtures to prod). If a running local
worker is already pointed at the dev DB, a read-only `GET /api/sync/info` against
it is fine — it only counts rows. Paste the JSON `stats` array.

If no worker is running, a `psql` count against the scratch DB proving the nine
`graph.*` tables exist and are countable is acceptable evidence for criterion 2,
plus a Go unit test if one fits the existing test style in
`internal/sync/graph_sync_test.go`.

Screenshot or describe the rendered Sync tab for criterion 3.

## Deploy (conductor only, after review + commit + push)

No migration is required — this change adds no columns and no tables, it only
counts existing ones. So the blocked `ssh … docker compose exec` migrate path is
not involved and no human-run command is needed.

```bash
# local worker (arm64, built in place)
make restart

# VPS enzo@enzogo.io.vn (amd64 built HERE, pushed to GHCR, pulled there)
make deploy
```

`make deploy` tags the image with `git rev-parse --short HEAD`, so commit and push
must land first.

Verify after each:
- local  → `http://localhost:34567` Sync tab shows both sections
- VPS    → `http://enzogo.io.vn:34567` Sync tab shows both sections in cloud mode
  (Total column only, subheadings present)

Quote the deployed image tag and the observed `graph.*` rows as evidence.

## Risks

- `COUNT(*)` on `graph.edges` / `graph.artifact_bodies` is a sequential scan.
  Acceptable at current row counts; the page is manually loaded, not polled. If a
  count is visibly slow, report it rather than adding caching.
- The VPS runs in cloud mode, so its Unsynced column is suppressed by the handler.
  A missing Unsynced column there is correct behaviour, not a bug.
