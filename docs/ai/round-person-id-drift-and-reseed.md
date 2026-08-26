# Round: fix the person-id drift, then re-seed the laptop's graph

**Written** 2026-08-26. **Status** awaiting approval. **Issue** agent-mem-xdq1 (P0).

## The question this answers

"Do we need to backup and restore the database from hub to local?"

**Yes for the `graph` schema on the laptop, no for anything else** - but only after a
one-line code fix lands. Restoring first would re-drift within a day.

## Root cause

`internal/database/graph_sync.go:434` - `ImportGraphPerson` inserts every column
**except `id`**:

```go
INSERT INTO graph.people
    (eeid, email, display_name, slack_user_id, jira_account_id,
     github_login, pagerduty_user_id, is_bot, reports_to, depth_from_root,
     first_seen_at, identity_resolved_at, merged_into,
     sync_id, sync_version, machine_id)
VALUES ($1,...,$16)
ON CONFLICT DO NOTHING
```

So a pulled person gets a fresh `nextval('graph.people_id_seq')` on the receiving
side. Every other graph importer preserves its primary key - `ImportGraphNode`
inserts `id` explicitly.

`graph.nodes.author_person_id`, `people.reports_to` and `people.merged_into` all
reference `people(id)` **across machines**. Measured on the laptop, the same three
people:

| person | hub id | laptop id |
|---|---|---|
| `B0A3Z18SFTP` | 4784 | 4776 |
| `B0BJCEZVCNM` | 4788 | 4778 |
| `ajanthan@wego.com` (Ajanthan Mani) | 4792 | 4780 |

Identical rows, different keys. `graph.people_id_seq` sits at 5719 against
`max(id)` 4782, so roughly 937 ids have been burned by conflicting re-imports.

Two consequences, one visible and one silent:

1. **Visible:** a node whose `author_person_id` is 4788 can never satisfy the FK on
   the laptop. That is the whole measured gap.
2. **Silent:** `reports_to` and `depth_from_root` on the laptop point at whoever
   happens to hold that id locally, so the laptop's org chart - which drives
   importance scoring and hot-topic alerting - is unreliable there.

## The measured gap it explains

| table | laptop | hub | gap |
|---|---|---|---|
| nodes | 35,615 | 35,656 (up to the laptop's own cursor) | 41 and regrowing |
| edges | 18,174 | 18,889 | 715 |
| artifact_index | 31,271 | 31,334 | 63 |
| artifact_bodies | 31,906 | 31,997 | 91 |
| people, entities, slack_groups, slack_users, slack_channels, thread_summaries, user_affinity_config | | | counts equal |

One cause, three symptoms: the node fails its person FK, the edges pointing at that
node fail theirs, and the artifact rows hanging off it fail too. July nodes match
exactly on both sides (28,845), so the 2026-08-05 restore was clean and all of this
accumulated since - about 34 edges a day.

A cursor reset alone does not fix it. I reset `pull_cursor:graph.people` and
`pull_cursor:graph.nodes` at 09:52 as a test: the re-walk imported 36,552 rows and
still ended with 49 `nodes_author_person_id_fkey` failures. The rows are being
offered and refused, not missed.

## Why a restore is needed on top of the code fix

The code fix stops new drift. It cannot repair the ids already assigned: the
laptop's `graph.nodes.author_person_id` values point at laptop ids, so simply
replacing `graph.people` would break the references that currently do resolve.
Re-seeding nodes, people, edges and artifacts together is the only way to get one
consistent id space, and it is what the 2026-08-05 runbook
(`docs/ai/graph-restore-from-vps.md`) already does.

## Accepted loss

The laptop holds graph rows the hub does not:

| table | laptop-authored | of those, present on the hub | lost by re-seed |
|---|---|---|---|
| graph.edges | 3,130 | 1,278 | **1,852** |
| graph.artifact_index | 363 | 199 | **164** |
| graph.nodes | 0 | - | 0 |

These are derived rows the laptop computed while it still ran graph jobs. They are
all marked `sync_version <> 0` (pushed), but the hub rejected them, so the Aug 5
runbook's abort test - "abort if `sync_version = 0` with a local machine_id" - would
wrongly call this restore lossless. Under the topology shipped in #48 the laptop
authors no graph memory at all, so re-deriving on the hub is the correct end state.
Stated here so the loss is a decision, not a surprise.

## Changes - code (worker)

`internal/database/graph_sync.go`:

1. `ImportGraphPerson` inserts `id` explicitly, with `ON CONFLICT (id) DO NOTHING`
   so the intent is the primary key and not whichever constraint happens to fire
   first. Comment says why: the id is referenced across machines.
2. After a people batch imports, advance the sequence so locally-authored rows can
   never collide with a hub id:
   `SELECT setval('graph.people_id_seq', GREATEST((SELECT max(id) FROM graph.people), last_value)) FROM graph.people_id_seq;`
   Put this in the importer's caller (`internal/sync/engine.go`, once per pull
   cycle) rather than per row.
3. A test in `internal/sync` that imports a person with a high id into an empty
   table and asserts the id survives and the sequence moved above it.

Not in scope: `ImportGraphEdge` drops its id too (`graph_sync.go:471`). Nothing
references an edge id across machines, so it is harmless and stays as is - noted so
the next reader does not conflate the two.

## Changes - data (conductor, after the fix is deployed)

Following `docs/ai/graph-restore-from-vps.md`, with three corrections for today:

1. The hub is `enzo@payments`, not the retired VPS, and `ssh ... docker exec` works
   from this session - no `!`-prefixed hand-off needed.
2. Do **not** re-park the pull cursors with bare `max(id)` values as that runbook
   does. Six tables now use composite `<RFC3339Nano>|<pk>` cursors; a bare id fails
   `DecodeCursor` and silently means "from the beginning". Delete the cursor rows
   instead and let one honest full walk run - post-restore the FKs resolve, so the
   walk converges instead of failing.
3. `graph.jobs` is excluded from the dump **and** no longer syncs at all after #48.
   The laptop runs `AGENT_MEM_GRAPH_RUNNER=none`, so its queue must stay empty.

Steps:

```bash
# 1. dump the hub's graph schema (1,944 MB on disk, jobs data excluded)
ssh enzo@payments 'PATH=/opt/homebrew/bin:$PATH docker exec agent-mem-postgres-1 \
  pg_dump -U agentmem -d agentmem --schema=graph \
  --exclude-table-data=graph.jobs -Fc' > /tmp/hub_graph.dump
ls -lh /tmp/hub_graph.dump && pg_restore -l /tmp/hub_graph.dump | grep -c 'TABLE DATA'

# 2. quiesce the laptop
docker compose stop worker

# 3. replace the schema
docker exec -i agent-mem-postgres-1 psql -U agentmem -d agentmem -c 'DROP SCHEMA graph CASCADE'
docker exec -i agent-mem-postgres-1 pg_restore -U agentmem -d agentmem \
  --no-owner --no-privileges < /tmp/hub_graph.dump

# 4. the hub never pushes, so its rows arrive at sync_version = 0 and the laptop
#    would read 35k of them as "mine, unpushed". Push is flat-only after #48, so
#    they can no longer leak upward - but mark them anyway so the Sync tab reads true.
#    (one UPDATE per graph table, machine_id <> the laptop's id)

# 5. clear the graph pull cursors so the next walk starts clean
docker exec agent-mem-postgres-1 psql -U agentmem -d agentmem \
  -c "DELETE FROM settings WHERE key LIKE 'pull_cursor:graph.%'"

# 6. restart
docker compose up -d worker
```

Flat memory is never touched: `observations`, `session_summaries`, `user_prompts`
and `sdk_sessions` stay exactly as they are. The laptop is their author.

## Acceptance criteria

1. `go build ./...`, `go vet ./...` clean; the new `internal/sync` test passes
   against the `agentmem_test` scratch DB, never the live dev DB.
2. `ajanthan@wego.com` carries id **4792** on both machines.
3. A full pull cycle after the restore logs **zero** `Sync import failed` lines.
4. Laptop and hub totals match for nodes, edges, artifact_index, artifact_bodies,
   people, entities, slack_groups, thread_summaries, user_affinity_config, allowing
   only rows the hub created during the restore window.
5. `select count(*) from graph.jobs` on the laptop is 0 and stays 0.
6. `graph.people_id_seq` is above `max(id)`.
7. Flat memory row counts unchanged by the restore, and push still drains.

## Risks

- The restore drops the 1,852 + 164 laptop-authored derived rows listed above.
- `DROP SCHEMA graph CASCADE` on the laptop is irreversible without the dump. Keep
  `/tmp/hub_graph.dump` until criterion 4 passes.
- HNSW rebuild: `docker-compose.yml` sets `shm_size: 1gb` because a 64 MB `/dev/shm`
  silently loses the index on restore. It is already set - do not run this on a
  postgres started without it, or graph search comes back with 0 recall
  (`agent-mem-e02` is that failure mode on the old VPS).
- Nothing on the hub is written at any point. It is a one-way copy.
