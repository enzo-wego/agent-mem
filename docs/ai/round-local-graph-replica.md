# Round: turn the laptop back on as a graph read replica

**Written** 2026-08-26. **Status** awaiting approval.

## Goal

Bring the laptop instance back into service with one authority per memory domain:

| Domain | Author | Replica | Direction |
|---|---|---|---|
| Flat memory (`observations`, `session_summaries`, `user_prompts`, `sdk_sessions`) | laptop | hub | laptop → hub (push) |
| Graph memory (`graph.*` + derived `thread_summaries` / `slack_users` / `slack_channels`) | hub (Mac mini) | laptop | hub → laptop (pull) |

Nothing crosses in the other direction. The laptop never authors graph memory and
never runs graph jobs; the hub never sends flat memory back.

## Measured state — 2026-08-26

**Laptop** (`machine_id a0411a4a…`, postgres on 5433 up 4 days, **worker container not running**)

| | rows | not yet pushed |
|---|---|---|
| observations | 129,512 | 1,608 |
| session_summaries | 8,784 | 146 |
| user_prompts | 12,220 | 72 |
| sdk_sessions | 2,923 | 20 |
| graph.nodes | 35,052 | 759 |
| graph.edges | 18,025 | 901 |
| graph.people | 937 | 3 |
| graph.artifact_index | 30,866 | 1,292 |
| graph.thread_summaries | 1,919 | — |
| **graph.jobs** | **2,341,737 (776 MB)** | 312,039 |

`last_push 2026-08-25T03:52Z`, `last_pull 2026-08-22T08:54Z` — pull cursors kept
advancing to 2026-08-25T03:55Z, so every cycle since Aug 22 imported rows and then
**ended in an error before `last_pull` was written**. `sync_enabled=true`,
`sync_url=http://100.125.54.118:34567` (hub over Tailscale) — already correct.

Laptop job queue: `queued 1,553,969` / `done 761,962` / `failed 24,022` /
`running 1,784`. Of the queued rows, **1,455,722 carry the hub's machine_id**,
97,634 the laptop's, 613 null.

Nothing listens on `127.0.0.1:8750` — the local llm-gateway is down (repo and
`.env` are present at `~/go/src/github.com/llm-gateway`).

**Hub** (`machine_id fa05616a…`) — `sync_enabled=false`, `sync_url` empty: pure
server role, it never pushes or pulls.

| | rows |
|---|---|
| observations | 129,627 (84,033 hub-authored, 45,603 laptop-authored) |
| session_summaries / user_prompts / sdk_sessions | 8,815 / 12,242 / 2,930 |
| graph.nodes | 35,586 (all hub-authored) |
| graph.edges / artifact_index / thread_summaries | 18,850 / 31,278 / 2,059 |
| graph.jobs | 3,476,621 — of which **124,860 carry the laptop's machine_id** |

The laptop's graph is ~534 nodes / ~412 artifacts / ~140 thread summaries behind
the hub. Incremental pull closes that on its own.

## What today's symmetric sync is doing wrong

1. **The laptop's graph jobs run on the hub.** `push()` (`internal/sync/engine.go`)
   ships nine graph tables including `graph.jobs`
   (`internal/database/graph_sync.go:399`, `WHERE sync_version = 0 AND target_runner
   != 'local'`). The hub imports them into its own queue and executes them:
   **124,682 laptop-authored jobs are `done` on the hub.** That is duplicate LLM
   work billed against the hub's seat.
2. **The hub's graph jobs pile up on the laptop as phantoms.**
   `GetGraphJobsForPull` (`graph_sync.go:859`) serves every other machine's
   non-`local` job; imports are `ON CONFLICT DO NOTHING`, so a row pulled while
   `queued` stays `queued` locally forever even after the hub finishes it. That is
   the 1,455,722 rows above. `Claim` matches `target_runner IN ('any', $runner)`
   (`internal/graph/jobs/queue.go:105`), so **starting the laptop worker as it
   stands would begin re-running the hub's work** — against a gateway that is not
   even up, so 1.5M jobs would fail, back off, and retry.
3. **Startup arms the hub's recurring jobs a second time.**
   `internal/worker/server.go:186-285` unconditionally enqueues
   `refresh_slack_channels`, `refresh_slack_bots`, `detect_hot_topics`,
   `derive_person_roles`, `refresh_jira_board`, `notify_watch_channels`,
   `monitor_hourly_report`, plus up to 1,000 thread-summary backfill jobs. On the
   laptop those mean duplicate Slack DMs and duplicate LLM spend.

The new topology removes all three by construction.

## Non-goals

- Do **not** change the hub's role, its settings, or its gateway backends (all
  three tiers stay `claude`; `FALLBACK_ON_QUOTA` stays `false`).
- Do **not** touch `llm_hourly_call_cap`.
- Do **not** prune the hub's own job history, or any hub graph data.
- Do **not** add a dashboard setting. The runner role is a deployment fact, not a
  user preference (`AGENT_MEM_GRAPH_RUNNER`, env-only today —
  `internal/config/config.go:515`), so this round adds no GUI surface.
- Do **not** rebuild the embedded dashboard: no frontend file changes.

## Code changes

### 1. `graph.jobs` leaves sync entirely — both sides

A work queue is per-machine by definition; the docs already parked this question
(`docs/ai/sync-tab-drop-graph-jobs.md`, `agent-mem-u3t` / `agent-mem-6nx`). Close
it by deleting jobs from the transport rather than gating it:

- `internal/sync/engine.go` — drop `GraphJobs` from `SyncPushPayload`,
  `SyncPullResponse`, `PullCursors`, the push gather + mark-synced, the pull
  import, the `g_jobs_after` query param and its cursor.
- `internal/worker/sync_handlers.go` — drop jobs from the push accept and the pull
  serve.
- `internal/database/graph_sync.go` — delete `GetUnsyncedGraphJobs`,
  `GetGraphJobsForPull`, `ImportGraphJob`, `SyncableGraphJob` (no other callers).
- Leftover `pull_cursor:graph.jobs` settings rows become inert; delete them in the
  data step.

### 2. Push carries flat memory only

In `push()`, delete the nine `GetUnsyncedGraph*` calls, the nine payload fields
and the nine `MarkSyncedGraphBySyncID` calls. Leave a comment naming the topology
so nobody re-symmetrises it:

```go
// Push is flat-memory only. The hub owns graph memory and this side is a read
// replica of it (docs/ai/round-local-graph-replica.md, 2026-08-26): sending
// graph rows back made the hub execute this machine's job queue.
```

### 3. Pull carries graph memory only

Delete the four flat imports and their four cursors from `pull()`, and the
corresponding flat queries from the pull handler. Keep the eight graph tables and
`pullDerived()` untouched.

*Why the handler must change too:* the pull loop repeats until a batch comes back
empty. If the client simply stopped importing flat rows while the hub kept serving
them, the flat cursors would never advance and the loop would re-request the same
batch forever. Cutting flat on the server side is what makes the client-side
deletion safe.

Both sides deploy from this repo, and the hub is the only server, so a compat flag
buys nothing. If flat pull is ever wanted back it is a revert of this hunk.

### 4. A runner role that runs nothing

`AGENT_MEM_GRAPH_RUNNER=none` on the laptop:

- `internal/worker/server.go` — when `cfg.Graph.Runner == "none"`, skip the seven
  startup enqueues, skip `BackfillMissingThreadSummaries`, and do not start
  `jobs.NewManager`. Log one line: `graph jobs disabled on this machine (runner=none)`.
- `docker-compose.yml` — the laptop needs the env var. `.env` is gitignored and
  absent locally, so create it with the single line
  `AGENT_MEM_GRAPH_RUNNER=none` rather than committing a laptop-specific value to
  compose.

Accepted consequence: on-demand graph work triggered from the laptop dashboard
(the lazy thread-summary popup) enqueues a job nobody claims. The summary still
arrives once the hub produces it, via the derived pull. Not worth a mechanism.

## Data + ops changes

1. **Truncate the laptop's job queue.** Nothing references `graph.jobs` (no inbound
   FKs, verified) and the laptop will now run zero jobs, so the whole 776 MB table
   goes: `TRUNCATE graph.jobs;` on the laptop only. Also
   `DELETE FROM settings WHERE key = 'pull_cursor:graph.jobs';`
2. **Optional, hub side:** the 124,860 laptop-authored job rows on the hub are
   history (124,682 `done`, 177 `failed`, 1 `running`). Deleting them is cosmetic;
   listed for completeness, skip unless asked.
3. **Bring the local llm-gateway back up** — `cd ~/go/src/github.com/llm-gateway &&
   docker compose up -d`, then confirm `curl -s localhost:8750/health`. Without it
   every local embedding and summarisation fails with `ErrUnreachable`. Note both
   gateways draw on the same Claude subscription seat, so the seat's rate-limit
   window is now shared between two machines.
4. **Restart the laptop worker** — `docker compose up -d --build worker` in this
   repo (local build; the hub is untouched by this round).

## Order of operations

1. Code changes 1–4, `go build ./... && go vet ./... && go test ./internal/sync/...`
   (handler integration tests only against the `agentmem_test` scratch DB — never
   the live dev DB).
2. Truncate the laptop job queue and drop the stale cursor row.
3. Start the local gateway; verify `/health`.
4. Rebuild + restart the laptop worker.
5. Verify (below). Only then commit and push.

## Acceptance criteria

1. `go build ./...`, `go vet ./...` clean; `internal/sync` tests pass. No TODO
   placeholders, no skipped tests.
2. A laptop push cycle logs a non-zero flat total and sends **no** graph rows: the
   `graph.*` unsynced counts on the laptop stay where they are (759 / 901 / 3 /
   1,292 …) and stop shrinking.
3. `last_pull` on the laptop advances within two cycles (~2 min) — the error that
   has blocked it since Aug 22 must be gone or reported.
4. Laptop graph counts converge on the hub's (nodes → 35,586+, artifact_index →
   31,278+, thread_summaries → 2,059+).
5. `select count(*) from graph.jobs` on the laptop stays at 0 after 10 minutes of
   uptime.
6. The hub gains no new rows with the laptop's machine_id in any `graph.*` table.
7. No Slack DM arrives from the laptop instance.
8. Laptop `observations` grows again as sessions run, and those rows reach the hub.

## How to verify

```bash
# 2: graph rows are no longer leaving the laptop
docker exec agent-mem-postgres-1 psql -U agentmem -d agentmem -c \
  "select 'nodes' t, count(*) filter (where sync_version=0) from graph.nodes
   union all select 'edges', count(*) filter (where sync_version=0) from graph.edges"

# 3 + 4: pull is healthy and catching up
docker exec agent-mem-postgres-1 psql -U agentmem -d agentmem -c \
  "select key, value from settings where key in ('last_pull','last_push')" -c \
  "select count(*) from graph.nodes"

# 5: the queue stays empty
docker exec agent-mem-postgres-1 psql -U agentmem -d agentmem -c \
  "select status, count(*) from graph.jobs group by 1"

# 6: nothing new from the laptop on the hub
ssh enzo@payments "PATH=/opt/homebrew/bin:\$PATH docker exec agent-mem-postgres-1 \
  psql -U agentmem -d agentmem -c \"select machine_id, count(*) from graph.jobs \
  where machine_id='a0411a4a-81c4-401c-8b78-3c068f373f5c' group by 1\""

docker logs --tail 100 agent-mem-worker-1 | grep -E "Sync (push|pull)|runner=none"
```

## Decision to confirm

**Flat memory becomes push-only, so the hub's own memories stop reaching the
laptop.** 84,033 of the hub's 129,627 observations were authored on the Mac mini
(you work there too). Those already sit on the laptop from past pulls, but any new
mini-authored session would from now on be invisible to local search. That follows
the stated strategy — flat goes up, graph comes down — and is a one-hunk revert if
it turns out you want mini sessions in local search after all. Say the word and I
keep flat pull in place instead.

## Risks accepted

- The laptop can no longer author graph memory at all. Any graph note written
  locally stays local and unreplicated. Authoring belongs on the hub now.
- Two gateways share one Claude seat; a heavy local run and a heavy hub run can
  now collide in the same rate-limit window.
- Truncating the laptop's job queue discards its `done`/`failed` history. It is a
  work queue, not memory, and 761k done rows have no reader.
