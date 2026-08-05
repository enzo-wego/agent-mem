# Fix pull-cursor data loss and repair the local graph (agent-mem-zqt, P0)

## Goal

Local is permanently missing ~5,800 graph rows that exist on the VPS. Stop the
loss, make it visible when it happens, and recover the already-lost rows.

Three code changes plus a repair pass:

1. Replace offset-based pull cursors with **keyset** cursors on the six affected
   tables.
2. Make `ImportGraph*` absorb collisions on the row's **real identity**, not just
   `sync_id`.
3. **Log** import failures instead of discarding them.
4. Reset the affected cursors so the corrected walk re-pulls what was skipped.

## Non-goals

- Do NOT fix `graph.jobs` double-claim (`target_runner='any'`). Separate issue
  `agent-mem-u3t`, separate round.
- Do NOT add job-table retention (`agent-mem-xag`).
- Do NOT make imports propagate *updates* (`DO UPDATE`). Absorbing collisions with
  `DO NOTHING` is enough to stop the data loss; update propagation is a distinct
  design question. See "Why not DO UPDATE".
- Do NOT change the push path, the flat-table (`observations` etc.) cursors, or
  the sync interval.
- Do NOT change the Sync Status page again.
- Do NOT deploy. Ask the conductor; deploy is a separate explicit step.

## Evidence (verified 2026-08-05 — do not re-derive)

Deficit equals `pull cursor − rows held`, exactly, on every offset-cursor table:

| table | cursor | local rows | VPS rows | missing |
|---|---|---|---|---|
| graph.nodes | 29,910 | 28,928 | 29,909 | ~981 |
| graph.artifact_index | 26,997 | 23,677 | 27,001 | ~3,324 |
| graph.artifact_bodies | 27,205 | 25,764 | 27,206 | ~1,442 |
| graph.user_affinity_config | 111 | 85 | 111 | 26 |
| graph.edges | (id-based) | 12,485 | 13,229 | 744 (FK cascade) |

Sampled twice, 100s apart: local graph counts did not move at all while
`graph.jobs` advanced +127 on both sides. The sync loop runs; the gap does not
converge. **This is permanent skipping, not lag.**

## Root causes

**1. Offset pagination over a filtered, re-sorting set.**
`internal/worker/sync_handlers.go:207-225` advances these cursors by batch size:

```go
cursors.GraphNodes = gNodesAfter + len(graphNodes)
```

while the queries in `internal/database/graph_sync.go` use `LIMIT $2 OFFSET $3`
over `WHERE machine_id IS DISTINCT FROM $1`. Any row inserted before the current
offset shifts one unseen row past the window, permanently. Worse, three of them
order by a **mutable** column, so rows re-sort between requests:

| function | line | current ordering | cursor should be |
|---|---|---|---|
| `GetGraphNodesForPull` | 669 | `ORDER BY updated_at` + OFFSET | keyset on `id` (TEXT) |
| `GetGraphArtifactIndexForPull` | 730 | `ORDER BY refreshed_at` + OFFSET | keyset on `node_id` (TEXT) |
| `GetGraphArtifactBodiesForPull` | 746 | `ORDER BY fetched_at` + OFFSET | keyset on `node_id` (TEXT) |
| `GetGraphSlackGroupsForPull` | 775 | `ORDER BY id` + OFFSET | keyset on `id` (TEXT) |
| `GetGraphEntitiesForPull` | 804 | `ORDER BY id` + OFFSET | keyset on `id` (TEXT) |
| `GetGraphUserAffinityConfigForPull` | 863 | `ORDER BY eeid` + OFFSET | keyset on `eeid` (INT) |

Already correct, leave alone: `GetGraphPeopleForPull` (637),
`GetGraphEdgesForPull` (702), `GetGraphJobsForPull` (833) — all `id > $2`.

**2. Conflict target too narrow.** Every `ImportGraph*` uses
`ON CONFLICT (sync_id) DO NOTHING`. That absorbs only a `sync_id` collision. A
collision on the row's real identity raises an error instead:
`graph.nodes.id` (TEXT PK), `artifact_index.node_id` / `artifact_bodies.node_id`
(PK), `edges UNIQUE(from_node_id,to_node_id,kind)`, `entities.id`,
`slack_groups.id`, `user_affinity_config.eeid`, and several UNIQUE columns on
`graph.people` (`eeid`, `email`, `slack_user_id`, `jira_account_id`,
`github_login`, `pagerduty_user_id`). Both machines derive the same content and
generate the same deterministic natural ids with *different* `sync_id`s, so these
collisions are routine.

**3. Errors discarded.** `internal/sync/engine.go:329-395`:

```go
if err := e.db.ImportGraphNode(ctx, &pullResp.GraphNodes[i]); err == nil {
    totalImported++
}
```

No logging, no counting, no retry — then `:420-438` advances the cursor
unconditionally. Nothing anywhere surfaces the loss.

## Files expected to change

1. `internal/database/graph_sync.go` — six `*ForPull` queries → keyset; the five
   TEXT-keyed ones take a `string` cursor. Also the `ON CONFLICT` target in the
   `ImportGraph*` functions.
2. `internal/worker/sync_handlers.go` — parse the five text cursors as strings,
   pass them through, and set response cursors from the **last returned row's
   key** instead of `offset + len(batch)`.
3. `internal/sync/engine.go` — `PullCursors` gains string fields for the five
   TEXT tables; `getPullCursor`/`setPullCursor` gain string variants (settings
   values are already TEXT); import loops log failures.
4. Tests — see "Tests".

## Approach

### 1. Keyset cursors

For each of the six functions, replace `LIMIT $2 OFFSET $3` with a keyset
predicate on the table's own primary key and order by that same key:

```sql
-- GetGraphNodesForPull
WHERE machine_id IS DISTINCT FROM $1 AND id > $2
ORDER BY id ASC LIMIT $3
```

- `nodes` → `id`, `slack_groups` → `id`, `entities` → `id` (all TEXT)
- `artifact_index`, `artifact_bodies` → `node_id` (TEXT)
- `user_affinity_config` → `eeid` (INTEGER — keep an int cursor for this one)

Change the parameter from `afterOffset int` to `afterKey string` (or `afterEEID
int` for affinity). An empty-string cursor must mean "from the beginning":
`id > ''` is true for every non-empty TEXT id, so a plain `''` default works —
confirm that and leave a short comment, since it is the thing a reader will
double-check.

Ordering by the PK is the point: it is immutable, unique and total, so the walk
cannot skip or repeat regardless of concurrent inserts or updates.

### 2. Absorb natural-identity collisions

In every `ImportGraph*`, change

```go
ON CONFLICT (sync_id) DO NOTHING
```

to the bare form

```go
ON CONFLICT DO NOTHING
```

Postgres permits only one inference target per statement, and the bare form
absorbs *any* unique or PK violation — `sync_id` and the natural key alike — which
is exactly what is wanted. It does **not** absorb foreign-key violations; those
still error and are then logged by part 3.

Leave a `ponytail:`-style comment noting the bare form is deliberate: the row's
identity is its natural key, and a differing `sync_id` for the same logical row
must not raise.

### Why not DO UPDATE

Tempting, but out of scope and riskier: a blind `DO UPDATE` is last-writer-wins
across two machines that both mutate the same rows, which can silently clobber
newer local state. Deciding the merge rule per column (`body_revision`,
`updated_at`) is its own design task. `DO NOTHING` fixes the loss; the rows we are
missing do not exist locally at all, so they insert cleanly.

### 3. Log import failures

In each import loop in `internal/sync/engine.go`, stop discarding the error.
Count failures and log at WARN with the table name and the row key, following the
zerolog style already used in this package. Include the failure count in whatever
summary the pull already logs.

**Do NOT block the cursor on failure.** It looks safer and it deadlocks: an edge
whose node has not yet arrived fails FK, and if the cursor cannot advance past it
the pull loop (`for { ... if batchTotal == 0 { break } }`, `engine.go:264`)
re-requests the same batch forever inside one cycle. Advance as today, log the
failure, and let a subsequent walk pick it up once the parent row exists. Put this
reasoning in a comment so nobody "fixes" it later.

### 4. Repair pass (conductor runs this, not the worker)

Once the above is committed and the worker restarted, reset the six cursors so the
corrected keyset walk re-pulls everything. Imports are idempotent, so this is
safe to repeat:

```sql
DELETE FROM settings WHERE key IN (
  'pull_cursor:graph.nodes',
  'pull_cursor:graph.artifact_index',
  'pull_cursor:graph.artifact_bodies',
  'pull_cursor:graph.slack_groups',
  'pull_cursor:graph.entities',
  'pull_cursor:graph.user_affinity_config'
);
```

Then let **two** sync cycles run: pass 1 lands the missing nodes, pass 2 lands the
edges and artifacts that failed FK while their parent node was absent. Re-run the
comparison until it converges or stops improving.

Note the re-walk re-requests ~27k artifact rows at 50/batch. It will take a while
and generate real traffic — expected, not a fault.

## Tests

Add to the existing style in `internal/sync/graph_sync_test.go` (scratch DB, never
the live dev DB):

1. **Pagination under concurrent insert** — the regression that pins this bug.
   Page through a table with a small limit; between page 1 and page 2 insert a row
   that sorts *before* the current position; assert every pre-existing row is
   still delivered. This test must fail against the old offset implementation.
2. **Collision absorbed** — import a row whose natural key already exists locally
   under a different `sync_id`; assert no error and no duplicate.
3. **Empty cursor means from-the-start** — assert a `''` cursor returns the
   lowest-keyed row.

## Acceptance criteria

1. All six `*ForPull` functions use a keyset predicate; no `OFFSET` remains in
   `graph_sync.go` pull queries.
2. Response cursors derive from the last returned row's key, never `offset + len`.
3. All `ImportGraph*` use the bare `ON CONFLICT DO NOTHING`.
4. Import failures are logged with table and row key, and counted.
5. The cursor still advances past a failed row (no deadlock), with a comment
   explaining why.
6. The three tests above pass; test 1 demonstrably fails on the old code.
7. `go build ./...`, `go vet ./...` clean; `go test ./internal/sync/... ./internal/database/...` passes.
8. No TODO placeholders, no skipped or stubbed tests.

## How to verify

Paste raw output, do not summarise.

```bash
go build ./... && go vet ./...
go test ./internal/sync/... ./internal/database/...
```

Then the conductor restarts the worker, runs the repair pass, and re-compares all
13 table counts local vs VPS. Success = the five graph gaps close to roughly
in-flight delta, from 981 / 3,324 / 1,442 / 744 / 26 to ~0.

## Open question, not a blocker

Local `user_prompts` reads 10,531 vs the VPS's 10,528 with `Unsynced: 0` — a gap
in the *opposite* direction, on the push side, that persisted across a 100s
sample. It may be ordinary in-flight lag. Re-check after this fix lands; if it
persists, file it separately. Do not chase it in this round.
