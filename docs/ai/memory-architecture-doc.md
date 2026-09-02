# Brief: architecture page for flat-memory + graph-memory (VPS), plus local-Mac explanation

Requested 2026-08-02 by Enzo. Round was deferred at step 0 for `/compact`
(context was ~66%). Resume from here — everything below is already verified,
do not re-check it.

## Goal

Two deliverables, one round:

1. An **HTML page** (published via the `Artifact` tool) showing the current
   architecture of the two memory subsystems as they run **on the VPS**:
   flat memory and graph memory. Load the `artifact-design` skill first.
2. A written explanation, in the reply, of **how it works on this Mac** —
   what runs locally, what talks to the VPS, and where the boundary is.

## Non-goals

- No production code changes. This is a documentation round.
- Do not fix `agent-mem-3z0` or `agent-mem-bqf` (found this session, filed,
  see below). Naming them on the page as known gaps is fine.

## Verified facts (2026-08-02 03:30–03:40 UTC, do not re-verify)

Production topology:

| Piece | Where | Detail |
|---|---|---|
| postgres | docker, VPS | `pgvector/pgvector:pg16`, host port **5433**, db `agentmem`, creds `agentmem/agentmem`, up 6 weeks |
| worker | docker, VPS | `ghcr.io/enzo-wego/agent-mem-worker:latest` @ commit `fdfd4b1`, bridge IP `172.18.0.3` |
| llm-gateway | **systemd on the host**, not docker | `llm-gateway.service`, uvicorn `app.main:app`, `/var/go/src/github.com/llm-gateway/.venv`, listening **`172.18.0.1:8750`**, `NRestarts=0` |
| gemini-proxy-relay | systemd, VPS | relays docker `:8888` → loopback Gemini proxy `127.0.0.1:8888` |
| dashboard/API | VPS | port **34567**, `0.0.0.0` — globe lives at `/live`, not `/` |

The worker reaches the gateway across the docker bridge at `172.18.0.1`, which
is why that IP had to go into the worker's `NO_PROXY` (`agent-mem-oar`).

Gateway surface actually in use (12h journal): `POST /generate` (85),
`POST /embed` (50), `POST /describe` (26), `GET /usage` (321, ~1/min poll),
`GET /health`. All 200. No other endpoints are called.

Schema landmarks:
- Flat path: `pending_messages` (`status` ∈ pending/processing/completed/failed,
  `attempts`, `available_at`, `error`, `processed_at`).
- Graph path: `graph.jobs` (`type`, `payload`, `priority`, `status`,
  `available_at`, `attempts`, `max_attempts`, `last_error`, `locked_by`,
  `locked_at`, `lease_until`, `enqueued_at`, `completed_at`, `target_runner`,
  `sync_id`, `sync_version`, `machine_id`), plus `graph.thread_summaries`
  (`channel_id`, `thread_ts`, `signature`, `summary`, `overview`, `highlights`,
  `kind`, `link_signature`, `updated_at`), `graph.nodes`, `graph.people`.
- Job types seen live: `summarize_thread`, `detect_hot_topics`,
  `notify_watch_channels`, `index_artifact`, `fetch_body`,
  `describe_attachment`, `refresh_jira_board`.
- Graph totals: 524,644 done / 32,842 failed / 36 running / 7 queued.

Two summarizers exist and are deliberately NOT merged (thread vs cluster) —
see the `project_agent_mem_summary_pipelines` memory before describing them.

## Still to establish (this is the actual work)

- How a message **enters** `pending_messages` and what the flat path writes —
  the local capture/ingest route vs the Slack route. Nothing new has entered
  the table since 2026-08-01 07:07 UTC (~20h), unconfirmed whether that is
  idle-overnight or a broken capture path. Resolve this before drawing the
  arrow, or draw it and label the uncertainty.
- Where embeddings are written and which table serves search.
- The sync engine: `sync_id` / `sync_version` / `machine_id` on `graph.jobs`
  imply Mac↔VPS replication. Establish direction, trigger and conflict rule —
  this is the core of the "how it works on my Mac" answer.
- Which MCP servers on the Mac read the graph (`code-review-graph` is
  configured locally; there is a planned read-only knowledge MCP — see the
  `project_agent_mem_knowledge_mcp` memory).

Prefer the `code-review-graph` MCP tools over Grep/Read for this mapping; the
repo CLAUDE.md requires it and it is far cheaper.

## Known gaps to mention on the page

- `agent-mem-3z0` (P2) — `summarize_thread` reschedules ~900 jobs/h over 35
  threads writing zero summaries. Churn only, not token spend: the signature
  check short-circuits before the LLM call.
- `agent-mem-bqf` (P2) — 11 `pending_messages` orphaned in `processing` since
  June/July. No lease or reaper on the flat path, unlike `graph.jobs`.

## Constraints

- team-mode: orchestrator only. Writing docs and the HTML page is allowed
  (`docs/ai/**`, scratchpad). Any production code change must be dispatched.
- Do not deploy, do not touch the VPS beyond read-only `psql` SELECTs,
  `docker compose ps` and `journalctl`.
- `ssh ... docker compose exec/logs` and reading `.env` are blocked by the
  permission classifier — hand Enzo the one-liner instead.
