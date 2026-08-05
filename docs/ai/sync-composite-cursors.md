# Fix incremental pull: composite (timestamp, pk) cursors

## Goal

Make pull deliver **new** rows again on the six graph tables that were converted
to keyset cursors in `e394fa3`. That change fixed walk completeness but broke
incremental delivery, because it keys on a non-monotonic natural id.

## The error being corrected

`e394fa3` (plan: `docs/ai/sync-keyset-cursors-repair.md`) replaced OFFSET
pagination with a keyset on each table's immutable primary key. That is correct
for walking a static set once, and wrong for incremental sync: a cursor only
delivers new rows if the ordering key **increases with write time**. A node id is
a natural key like `slack:C011RFSBLP3:1709557592.431279`, so new rows land
scattered through the key space.

Measured on local after the restore (2026-08-05), cursor parked at the max id
`wego_order:WF-F84B17F3…`:

- **0** of 29,932 nodes sort after the cursor.
- 25,591 nodes (85%) are `slack:*`, which sorts *before* `wego_order:*`.

So every future slack/jira/gh_pr node would sort behind the cursor and never be
pulled. Live proof from the worker log:

```
Sync pull complete  failed=9  imported=644
Sync import failed  table=graph.edges  key=196569
  error="violates foreign key constraint \"edges_from_node_id_fkey\""
```

`graph.edges` keys on a BIGSERIAL, which *is* monotonic, so new edges arrive —
and then fail because their parent nodes, keyed on text, cannot. Edges flowing
while nodes are frozen is the signature of this bug.

The original code's `ORDER BY updated_at` had the right ordering column. The defect
was `OFFSET`, not the column. This change keeps keyset pagination but keys on the
monotonic timestamp, with the pk as tiebreaker.

## Non-goals

- Do NOT revert to OFFSET.
- Do NOT change `graph.people`, `graph.edges` or `graph.jobs`. They key on a
  BIGSERIAL id, which is monotonic with insertion — not a regression, out of scope.
- Do NOT add `DO UPDATE` semantics. Re-delivered rows hit `ON CONFLICT DO NOTHING`
  and are skipped; making updates overwrite content is a separate decision
  (see `agent-mem-zqt` notes).
- Do NOT change the flat tables, the push path, or the dashboard.
- Do NOT run the local cursor reset — that is the conductor's step after deploy.

## Design

### Cursor = (timestamp, pk), transported as one string

Per table, the monotonic column and tiebreaker:

| table | timestamp column | tiebreaker pk |
|---|---|---|
| graph.nodes | `updated_at` | `id` |
| graph.artifact_index | `refreshed_at` | `node_id` |
| graph.artifact_bodies | `fetched_at` | `node_id` |
| graph.slack_groups | `refreshed_at` | `id` |
| graph.entities | `first_seen_at` | `id` |
| graph.user_affinity_config | `updated_at` | `eeid` |

Query shape — a row-value comparison, which Postgres can drive from a composite
btree index:

```sql
WHERE machine_id IS DISTINCT FROM $1
  AND (updated_at, id) > ($2, $3)
ORDER BY updated_at ASC, id ASC
LIMIT $4
```

**`PullCursors` keeps the same JSON shape** — these six fields are already
`string`. Encode the pair as `"<RFC3339Nano>|<pk>"` and decode server-side. No new
fields, no wire-format change beyond the encoding inside that string value.

Rules:

- An **empty** cursor means from the beginning.
- A cursor that **fails to parse** must also mean from the beginning. This is
  deliberate fail-open: the currently parked cursors hold bare ids like
  `wego_order:WF-…`, which will not parse as a pair, so the first pull after
  deploy walks the table from the start and self-heals. Leave a comment saying so.
- The response cursor is the last returned row's `(timestamp, pk)`, encoded.

Re-delivery of an updated row is expected and harmless: the row already exists and
`ON CONFLICT DO NOTHING` skips it. Note that in a comment so nobody "optimises" it.

### Migration: composite indexes

Only `graph.nodes` has an index on its timestamp (`idx_nodes_updated_at`, DESC and
not composite). Add one migration with the composite indexes the new ORDER BY
needs:

```sql
CREATE INDEX IF NOT EXISTS idx_nodes_sync_keyset            ON graph.nodes(updated_at, id);
CREATE INDEX IF NOT EXISTS idx_artifact_index_sync_keyset   ON graph.artifact_index(refreshed_at, node_id);
CREATE INDEX IF NOT EXISTS idx_artifact_bodies_sync_keyset  ON graph.artifact_bodies(fetched_at, node_id);
CREATE INDEX IF NOT EXISTS idx_slack_groups_sync_keyset     ON graph.slack_groups(refreshed_at, id);
CREATE INDEX IF NOT EXISTS idx_entities_sync_keyset         ON graph.entities(first_seen_at, id);
CREATE INDEX IF NOT EXISTS idx_affinity_sync_keyset         ON graph.user_affinity_config(updated_at, eeid);
```

Include a `-- +goose Down` dropping them. Follow the existing migration naming in
`migrations/`.

## Files expected to change

1. `internal/database/graph_sync.go` — the six `*ForPull` queries and their
   signatures (timestamp + pk instead of a single key).
2. `internal/worker/sync_handlers.go` — decode the pair from the query param,
   encode the response cursor from the last row.
3. `internal/sync/engine.go` — no `PullCursors` shape change; keep passing the
   encoded strings. Add the encode/decode helper and its comment.
4. `migrations/<timestamp>_graph_sync_keyset_indexes.sql` — new.
5. `internal/sync/graph_sync_test.go` — tests below.

## Tests

Same scratch-DB rules as before: `openTestPool`'s guard is in place, use
`agentmem_test`, never the dev DB.

1. **New row with a lower-sorting id is still delivered** — the regression that
   pins this bug. Walk `graph.nodes` to exhaustion, then insert a node whose id
   sorts *before* every id already seen (e.g. `aaa:new`) with a current
   `updated_at`; assert the next pull returns it. Must fail on `e394fa3`.
2. **Same-timestamp rows all delivered** — several rows sharing one `updated_at`,
   page size 1, assert each arrives exactly once and pagination terminates (proves
   the pk tiebreaker works).
3. **Unparseable cursor walks from the start** — pass `wego_order:WF-ABC` as the
   cursor, assert the lowest-ordered row comes back rather than nothing.
4. Keep the existing tests passing, adjusting call sites for the new signature.

## Acceptance criteria

1. All six queries use `(timestamp, pk) > (…)` with a matching `ORDER BY`.
2. Cursor encode/decode round-trips; empty and unparseable both mean start.
3. Migration adds the six composite indexes and has a working Down.
4. The four test groups above pass; test 1 demonstrably fails on `e394fa3`.
5. `go build ./...`, `go vet ./...` clean; `go test ./internal/sync/...` passes.
6. No TODO placeholders, no skipped or stubbed tests.

## How to verify (conductor, after deploy)

Deploy the VPS first, then restart local (the pull format's value encoding changes,
so mismatched ends fail closed to a full walk rather than corrupting anything).
Then:

```bash
docker exec agent-mem-postgres-1 psql -U agentmem -d agentmem \
  -c "DELETE FROM settings WHERE key LIKE 'pull_cursor:graph%'"
```

Success is **not** "counts match once" — the restore already achieved that.
Success is: create nothing by hand, wait for the VPS to ingest new slack content,
and confirm local's `graph.nodes` count tracks the VPS's over ~15 minutes, with
`grep -c "Sync import failed"` at or near zero and no
`edges_from_node_id_fkey` errors.
