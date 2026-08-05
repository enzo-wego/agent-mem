# Restore local graph from the VPS, then prove sync holds

## Goal

Make the VPS the authoritative source for graph memory and rebuild local's graph
schema from it, because ~653 nodes (and their edges/artifacts) can never be
reconciled by sync alone. Then prove the two stay in step.

## Why sync alone cannot finish the job

The keyset-cursor fix (`e394fa3`, `agent-mem-zqt`) is deployed and working: it has
already recovered `graph.user_affinity_config` fully (26 → 0) and steadily drained
`artifact_index` (3,324 → 1,522) and `artifact_bodies` (1,442 → 843).

But `graph.nodes` plateaued at **+653** with a hard floor:

```
Sync import failed  table=graph.nodes  key=slack:C011RFSBLP3:1784104095.233339
  error="violates foreign key constraint \"nodes_author_person_id_fkey\""
```

Those nodes reference VPS `graph.people.id` values that local cannot obtain.
`graph.people.id` is a machine-local `BIGSERIAL`, but nodes reference it *across
machines*. Local holds 438 legacy `machine_id='local'` people that describe the
same humans as the VPS's rows and collide with them on `UNIQUE(email)` /
`UNIQUE(slack_user_id)`. The import absorbs the collision and skips the row, so
the VPS's person id never appears locally, so the FK can never be satisfied. No
number of cursor resets changes that. Restoring replaces local's person ids with
the VPS's authoritative ones and the FKs resolve.

## Pre-flight: the restore is lossless (verified 2026-08-05)

Local authors almost nothing in graph, and everything it did author is already on
the VPS:

| table | VPS-origin | local-origin | local-origin unpushed |
|---|---|---|---|
| graph.nodes | 29,262 | 5 | 0 |
| graph.edges | 11,483 | 1,006 | 0 |
| graph.people | 484 | 438 (`machine_id='local'`) | 0 |

Every row currently flagged `sync_version=0` locally carries the **VPS's**
machine_id — they are re-imported VPS rows, not local work. So no local-only graph
data exists to lose.

Re-run this check immediately before truncating; do not restore on stale evidence:

```sql
SELECT 'nodes' t, machine_id, count(*) FILTER (WHERE sync_version=0) unpushed, count(*) total
FROM graph.nodes GROUP BY 1,2
UNION ALL SELECT 'edges', machine_id, count(*) FILTER (WHERE sync_version=0), count(*) FROM graph.edges GROUP BY 1,2
UNION ALL SELECT 'people', machine_id, count(*) FILTER (WHERE sync_version=0), count(*) FROM graph.people GROUP BY 1,2;
```

**Abort the restore if any row has `sync_version=0` AND a local machine_id**
(`local` or `a0411a4a-81c4-401c-8b78-3c068f373f5c`) — that would be local work not
yet on the VPS, and it must be pushed first.

## Non-goals

- Do NOT touch the flat tables (`observations`, `session_summaries`,
  `user_prompts`, `sdk_sessions`). They already match except `user_prompts`
  (local +3), which is push-side and tracked separately.
- Do NOT restore `graph.jobs` from the VPS. It is 597k rows, and per
  `agent-mem-u3t` importing the VPS's queue can make local execute VPS jobs.
- Do NOT change sync code in this round. `e394fa3` is deployed on both sides.
- Do NOT touch the VPS's data. It is the source of truth; this is a one-way copy.

## Decision needed before starting

`DROP SCHEMA graph CASCADE` also destroys local's `graph.jobs` (~597k rows, mostly
completed history per `agent-mem-xag`). Recurring jobs re-enqueue themselves at
worker startup, so the queue self-heals, but in-flight local job state is lost.
Confirm which the human wants:

- **A (simpler):** accept the reset; let startup re-enqueue.
- **B:** dump local `graph.jobs` first and reload it after the restore.

## Steps

Every command below says which machine it targets. Both hosts have a container
named `agent-mem-postgres-1` — do not mix them up.

### 1. Dump the VPS graph schema (human runs this)

Claude Code's permission classifier blocks `ssh … docker exec`, so run this
yourself with the `!` prefix. It streams to a **local** file; no `docker cp`
needed, and ssh stdout is 8-bit clean for the custom format:

```bash
ssh enzo@enzogo.io.vn 'sudo docker exec agent-mem-postgres-1 pg_dump -U agentmem -d agentmem --schema=graph --exclude-table-data=graph.jobs -Fc' > /tmp/vps_graph.dump
```

Then sanity-check it before it is allowed near local data:

```bash
ls -lh /tmp/vps_graph.dump
pg_restore -l /tmp/vps_graph.dump | grep -c 'TABLE DATA'
```

A dump under ~50 MB or with no `TABLE DATA` entries means the pg_dump failed —
stop and investigate rather than restoring a truncated copy.

`--exclude-table-data` keeps the `graph.jobs` table definition but none of its
rows, which is what we want since the schema is being recreated.

Note pg_dump emits FK constraints *after* the data, so the load cannot fail on
FK ordering.

### 2. Quiesce local

```bash
docker compose stop worker      # local — stop writes before touching the schema
```

### 3. Replace the local graph schema

```bash
docker exec -i agent-mem-postgres-1 psql -U agentmem -d agentmem \
  -c 'DROP SCHEMA graph CASCADE'
docker exec -i agent-mem-postgres-1 pg_restore -U agentmem -d agentmem \
  --no-owner --no-privileges < /tmp/vps_graph.dump
```

### 4. Stop local re-pushing rows it did not author

The VPS never pushes, so its own rows sit at `sync_version = 0`. Restored
verbatim, local reads ~30k of them as "mine, unpushed" and pushes them all back —
absorbed by `ON CONFLICT DO NOTHING`, but a large pointless round trip. Mark
foreign-authored rows as already synced (local's machine_id is
`a0411a4a-81c4-401c-8b78-3c068f373f5c`):

```sql
UPDATE graph.nodes            SET sync_version = 1 WHERE sync_version = 0 AND machine_id <> 'a0411a4a-81c4-401c-8b78-3c068f373f5c';
UPDATE graph.edges            SET sync_version = 1 WHERE sync_version = 0 AND machine_id <> 'a0411a4a-81c4-401c-8b78-3c068f373f5c';
UPDATE graph.people           SET sync_version = 1 WHERE sync_version = 0 AND machine_id <> 'a0411a4a-81c4-401c-8b78-3c068f373f5c';
UPDATE graph.artifact_index   SET sync_version = 1 WHERE sync_version = 0 AND machine_id <> 'a0411a4a-81c4-401c-8b78-3c068f373f5c';
UPDATE graph.artifact_bodies  SET sync_version = 1 WHERE sync_version = 0 AND machine_id <> 'a0411a4a-81c4-401c-8b78-3c068f373f5c';
UPDATE graph.entities         SET sync_version = 1 WHERE sync_version = 0 AND machine_id <> 'a0411a4a-81c4-401c-8b78-3c068f373f5c';
UPDATE graph.slack_groups     SET sync_version = 1 WHERE sync_version = 0 AND machine_id <> 'a0411a4a-81c4-401c-8b78-3c068f373f5c';
UPDATE graph.user_affinity_config SET sync_version = 1 WHERE sync_version = 0 AND machine_id <> 'a0411a4a-81c4-401c-8b78-3c068f373f5c';
```

### 5. Park the pull cursors at the restored high-water mark

Local is now an exact copy, so there is nothing behind to fetch. Setting the
cursors avoids a pointless ~27k-row re-walk on the next tick:

```sql
INSERT INTO settings (key, value) VALUES
  ('pull_cursor:graph.nodes',                (SELECT COALESCE(max(id),'')      FROM graph.nodes)),
  ('pull_cursor:graph.artifact_index',       (SELECT COALESCE(max(node_id),'') FROM graph.artifact_index)),
  ('pull_cursor:graph.artifact_bodies',      (SELECT COALESCE(max(node_id),'') FROM graph.artifact_bodies)),
  ('pull_cursor:graph.slack_groups',         (SELECT COALESCE(max(id),'')      FROM graph.slack_groups)),
  ('pull_cursor:graph.entities',             (SELECT COALESCE(max(id),'')      FROM graph.entities)),
  ('pull_cursor:graph.people',               (SELECT COALESCE(max(id),0)::text FROM graph.people)),
  ('pull_cursor:graph.edges',                (SELECT COALESCE(max(id),0)::text FROM graph.edges)),
  ('pull_cursor:graph.user_affinity_config', (SELECT COALESCE(max(eeid),0)::text FROM graph.user_affinity_config))
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value;
```

Leave `pull_cursor:graph.jobs` alone — jobs were not restored.

### 6. Restart and verify

```bash
docker compose start worker     # local
```

## Acceptance criteria

1. Local and VPS totals match for all nine `graph.*` tables except `graph.jobs`
   (allowing only in-flight delta).
2. `docker logs agent-mem-worker-1 | grep "Sync import failed"` shows **zero**
   FK failures after the restart — this is the real proof the 653-node blocker is
   gone, not just that counts moved.
3. Counts stay matched across at least **three** consecutive sync cycles (~3 min),
   proving sync keeps working rather than merely that a copy landed.
4. Local push still works: a newly created local row reaches the VPS.
5. Flat table counts unchanged by the whole exercise.
6. The dashboard Sync tab renders both sections with the new numbers on both hosts.

## How to verify

Re-use the sampler from this session and paste raw output:

```bash
python3 <scratchpad>/watch_convergence.py      # gaps per table, per minute
docker logs agent-mem-worker-1 --since 5m 2>&1 | grep -c "Sync import failed"
```

Criterion 4: note a `graph.edges` or observation count on both sides, let a cycle
pass, confirm the local increment appears on the VPS.

## Residual risk, worth its own issue

This restore fixes today's divergence but not its cause. `graph.people.id` is a
per-machine serial referenced across machines, so if local ever authors people
again, the same id collision recurs. The durable fix is one of:

- make local **pull-only** for graph (it authors 5 nodes out of 29,267 — the
  ingestion jobs are all `target_runner='vps'` already), or
- key cross-machine references on a natural identity (email / slack_user_id)
  rather than a local serial, or
- remap `author_person_id` to the local person id during import.

File this before closing the round.
