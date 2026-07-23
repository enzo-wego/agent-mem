# agent-mem

`agent-mem` is a Go service and CLI for persistent coding-agent memory. It captures local hook events, stores prompts/observations/session summaries in PostgreSQL with `pgvector`, uses Gemini for extraction and embeddings, and serves a small dashboard for search and inspection. It also hosts **Graph Memory** — a cross-source knowledge graph that links Slack, Jira, GitHub, Confluence, PagerDuty, Datadog, Sentry, Google Workspace, Wego Hub, and shared Claude artifacts into one queryable store ([jump to section](#graph-memory)).

## What It Does

- Runs a long-lived worker on `:34567`
- Accepts hook events such as session start, prompt submit, post-tool-use, and stop
- Stores prompts, observations, summaries, and sync metadata in PostgreSQL
- Builds relevant context for future sessions
- **Graph Memory**: ingests cross-source artifacts (push, URL, or Slack backfill), processes them through a Postgres-backed job queue (fetch → normalize → extract edges → describe media → embed), and serves search / BFS-resolve / node read endpoints
- Exposes a dashboard and JSON API for search, timelines, sync, settings, logs, graph, jobs, and backfill
- Integrates with Claude Code, Codex, and Gemini CLI (one-shot installers)

## Architecture

```text
┌────────────────────────── CLIENTS ───────────────────────────┐
│                                                              │
│   Claude Code hooks       Codex hooks       mem-search skill │
│         │                      │                   │         │
│         ▼                      ▼                   │         │
│   ┌──────────────────────────────────┐             │         │
│   │     agent-mem hook <event>       │             │         │
│   │  (short-lived CLI, stdin JSON →  │             │         │
│   │   POST to worker, stdout reply)  │             │         │
│   └──────────────┬───────────────────┘             │         │
│                  │ POST /api/hook/*                │ GET     │
│                  │                                 │ /api/*  │
│   Browser ───────┼────────── GET /  (SPA) ─────────┤         │
│                  │                                 │         │
└──────────────────┼─────────────────────────────────┼─────────┘
                   │                                 │
                   └─────────────────┬───────────────┘
                                     │  HTTP
                                     ▼
┌──────────────── SERVER  (localhost:34567) ───────────────────┐
│                      agent-mem worker                        │
│                                                              │
│   hook ingest │ hybrid search │ sync push/pull │ dashboard   │
│   graph: ingest/backfill → job queue → search/resolve/node   │
│                                                              │
└────────────┬───────────────────────┬────────────────┬────────┘
             │                       │                │
             ▼                       ▼                ▼
   ┌───────────────────┐    ┌───────────────────┐  ┌──────────────────────┐
   │  PostgreSQL +     │    │  Gemini API       │  │  External sources     │
   │  pgvector         │    │  (extract+embed,  │  │  Slack/Jira/GH/CF/PD/  │
   │  (memory + graph) │    │   media describe) │  │  DD/Sentry/GWS + lit   │
   └───────────────────┘    └───────────────────┘  └──────────────────────┘
```

### Cloud Sync

When `AGENT_MEM_SYNC_ENABLED=true`, the worker runs a ticker every `AGENT_MEM_SYNC_INTERVAL` (default `60s`) that pushes unsynced rows to — and pulls other machines' rows from — a remote `agent-mem` instance at `AGENT_MEM_SYNC_URL`. The remote is just another `agent-mem` worker, so "client" and "server" here are roles, not separate binaries.

```text
┌──────────────────────────┐                        ┌──────────────────────────┐
│  Local agent-mem         │                        │  Remote agent-mem        │
│  (sync client)           │                        │  (sync server,           │
│                          │                        │   same binary)           │
│  every SYNC_INTERVAL:    │                        │                          │
│                          │  POST /api/sync/push   │                          │
│  1. collect unsynced     │  + Bearer API key      │                          │
│     rows (sync_version   │ ─────────────────────► │  INSERT ... ON CONFLICT  │
│     = 0) in batches      │    {machine_id,        │    (sync_id) DO NOTHING  │
│     of 100 per table     │     sessions, obs,     │                          │
│                          │     summaries,         │                          │
│                          │     prompts}           │                          │
│                          │ ◄───────────────────── │                          │
│  2. MarkSynced locally   │   {received, rejected} │                          │
│     (sync_version = now) │                        │                          │
│                          │  GET /api/sync/pull    │                          │
│  3. request rows from    │  ?machine_id=self      │                          │
│     other machines       │  &obs_after=cursor…    │                          │
│     using per-table      │ ─────────────────────► │                          │
│     cursors stored in    │                        │                          │
│     app_settings         │ ◄───────────────────── │                          │
│                          │   rows + new cursors   │                          │
│  4. import locally with  │                        │                          │
│     ON CONFLICT (sync_id)│                        │                          │
│     DO NOTHING           │                        │                          │
└──────────────────────────┘                        └──────────────────────────┘
```

Key properties:

- **Auth**: both endpoints require `Authorization: Bearer $AGENT_MEM_API_KEY`.
- **Identity**: each row carries `sync_id = {machine_id}:{row_id}`; dedup is enforced by a UNIQUE index on `sync_id`, so replaying a push is safe.
- **Heartbeat**: the local worker pushes every cycle even when it has nothing unsynced, so the remote can track `last_push` per `machine_id`.
- **Cursors**: the pull uses per-table `_after` cursors (observations / summaries / prompts / sessions) persisted in `app_settings`, so a restart doesn't re-pull the whole dataset.
- **Embeddings travel with the data** — the remote does not re-embed.
- **Status**: `GET /api/sync/info` returns current mode, cursors, and per-machine push/pull timestamps.

Main code paths:

- [cmd/agent-mem/main.go](/Users/neocapitelo/go/src/github.com/agent-mem/cmd/agent-mem/main.go)
- [internal/worker](/Users/neocapitelo/go/src/github.com/agent-mem/internal/worker)
- [internal/database](/Users/neocapitelo/go/src/github.com/agent-mem/internal/database)
- [internal/graph](/Users/neocapitelo/go/src/github.com/agent-mem/internal/graph) — graph memory: `ids`, `identity`, `normalizer`, `extractor`, `entities`, `fetchers`, `jobs`, `handlers`, `acl`, `scoring`, `bfs`, `hydrate`
- [dashboard](/Users/neocapitelo/go/src/github.com/agent-mem/dashboard)
- [plugin/skills](/Users/neocapitelo/go/src/github.com/agent-mem/plugin/skills)

## Graph Memory

Graph Memory builds a cross-source knowledge graph that links Slack messages, Jira tickets, GitHub PRs, Confluence pages, PagerDuty incidents, Datadog monitors, Sentry issues, Google Workspace docs, Wego Hub published files, and shared Claude artifacts into a single queryable artifact store. Each source is a node; relationships extracted from bodies (references, mentions, ownership) become typed edges.

```text
  Slack msg  ──REFERENCES──▶  Jira PAY-2128  ──REFERENCES──▶  GH PR #1960
      │                            │
   MENTIONS                    PART_OF
      ▼                            ▼
  graph.people               Confluence page
```

### Ingest endpoints

Two endpoints accept content from external integrations or the hook pipeline:

**POST /api/graph/ingest/content** — push a message or document body directly:

```bash
curl -s -X POST http://localhost:34567/api/graph/ingest/content \
  -H "Authorization: Bearer $AGENT_MEM_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "source": "slack",
    "canonical_url": "https://wego.slack.com/archives/C08S954G2LX/p1779710863216389",
    "body": "TRY payments failing — see PAY-2128",
    "metadata": {
      "channel_id": "C08S954G2LX",
      "ts": "1779710863.216389",
      "body_ts": "2026-05-27T09:01:03Z",
      "author": { "ref": "slack_uid:UUK3WPNNQ", "display_name": "Lei Zheng" }
    }
  }'
# {"node_id":"slack:C08S954G2LX:1779710863.216389","outcome":"created","extracted":{...},...}
```

**POST /api/graph/ingest/url** — enqueue a fetch for a URL you don't have the body for:

```bash
curl -s -X POST http://localhost:34567/api/graph/ingest/url \
  -H "Authorization: Bearer $AGENT_MEM_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"url": "https://wegomushi.atlassian.net/browse/PAY-2128"}'
```

Both endpoints return `outcome`: `created`, `updated`, or `unchanged`.

### Backfill a Slack channel

**POST /api/graph/backfill/slack** — pull a channel's last *N* months of history
into the graph. This is a *pull* (the worker calls Slack's `conversations.history`
/ `conversations.replies` Web API with the bot token) — it does **not** need any
webhook or socket-mode wiring. The bot must be a member of the channel
(`/invite @bot`), and private channels need the `groups:history` scope.

```bash
curl -s -X POST http://localhost:34567/api/graph/backfill/slack \
  -H "Authorization: Bearer $AGENT_MEM_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"channel_id": "C08S954G2LX", "months": 1}'
# {"job_id":1,"status":"queued","channel_id":"C08S954G2LX","oldest_ts":"...","estimated_months":1}
```

`channel_id` must match `^C[A-Z0-9]+$`; `months` is 1–24. The endpoint enqueues a
`backfill_slack_channel` job (paginated, re-enqueues itself per cursor) which fans
out into `backfill_slack_thread`, `fetch_body`, `describe_attachment`, and
`index_artifact` jobs. Watch them via the job admin endpoints or the **Jobs**
dashboard tab. See [docs/ai/TESTING.md](docs/ai/TESTING.md) for the full
insert → process → read walkthrough.

### Required env vars (fetchers)

| Variable | Source |
|---|---|
| `AGENT_MEM_SLACK_BOT_TOKEN` | Slack Bot OAuth token |
| `AGENT_MEM_JIRA_EMAIL` + `AGENT_MEM_JIRA_TOKEN` | Jira API basic auth |
| `AGENT_MEM_GH_TOKEN` | GitHub personal access token |
| `AGENT_MEM_CF_TOKEN` | Confluence API token (reuses Jira email) |
| `AGENT_MEM_PAGERDUTY_TOKEN` | PagerDuty REST API v2 token |
| `AGENT_MEM_DATADOG_API_KEY` + `AGENT_MEM_DATADOG_APP_KEY` | Datadog API/App keys |
| `AGENT_MEM_SENTRY_AUTH_TOKEN` + `AGENT_MEM_SENTRY_ORG` | Sentry auth token + org slug |
| `AGENT_MEM_GWS_SERVICE_KEY_PATH` | Path to Google service-account JSON |
| `AGENT_MEM_WEGOHUB_TOKEN` | Wego Hub deploy/Bearer token (read API) |

Optional: `AGENT_MEM_GRAPH_RUNNER` (default `any`), `AGENT_MEM_JIRA_BASE_URL`, `AGENT_MEM_GH_BASE_URL`, `AGENT_MEM_WEGOHUB_BASE_URL` (default `https://internal.wego.com/hub`).

### Optional: LiteParse for fast PDF/office parsing

agent-mem can use [LiteParse](https://github.com/run-llama/liteparse) to
extract text from PDF/DOCX/XLSX/PPTX locally before falling back to Gemini
multimodal. This is faster (~50–200ms vs 2–5s) and avoids API cost for
text-heavy documents.

Install on the VPS:

```bash
npm install -g @llamaindex/liteparse   # ships a prebuilt `lit` binary (recommended)
# or
cargo install liteparse                # builds the `lit` CLI from source
```

> **glibc requirement.** The prebuilt native binary
> (`@llamaindex/liteparse-linux-x64-gnu`) links against **glibc ≥ 2.38** and
> `GLIBCXX_3.4.31`, and ships **no musl variant**. It will *not* load on Alpine
> or on Debian Bookworm (glibc 2.36) — use a `node:22-trixie-slim` (glibc 2.40)
> or newer base. The Docker image installs it into a local project dir and
> symlinks `lit` onto `$PATH`; see [Dockerfile](Dockerfile).

Then set the env (defaults invoke `lit` from `$PATH`):

| Variable | Default | Description |
|---|---|---|
| `LITEPARSE_BIN_PATH` | `lit` | Path to the `lit` binary |
| `LITEPARSE_SCREENSHOT_ENABLED` | `true` | Enable per-page screenshots for image-heavy docs |
| `LITEPARSE_TEMP_DIR` | `os.TempDir()` | Working directory for temp files |

If the binary is not present, agent-mem silently falls back to sending the full document bytes to
Gemini multimodal — no error, just slower.

**Extraction tiers** (automatic, no config needed):

1. **Rich text** (≥ 200 chars extracted): LiteParse text used directly; only a cheap Gemini Embed call is made.
2. **Thin text** (image-heavy doc): LiteParse generates page screenshots; each screenshot is sent to Gemini Vision.
3. **LiteParse unavailable**: full document bytes sent to Gemini multimodal (original behaviour).

### Job admin endpoints

```bash
# List recent jobs
GET  /api/graph/jobs

# Delete a job
DELETE /api/graph/jobs/{id}

# Retry a failed job
POST /api/graph/jobs/{id}/retry
```

### Import org chart from BambooHR

```bash
agent-mem entities import-bamboohr --csv ~/Downloads/bamboohr_org_chart_for_visio.csv
# enqueued import_bamboohr job id=42 (csv: .../bamboohr_org_chart_for_visio.csv, 18432 bytes)
```

The command enqueues an `import_bamboohr` job. The worker processes the CSV to upsert `graph.people` rows with `eeid`, `display_name`, `reports_to`, and `depth_from_root`.

### Graph sync

Graph tables (`graph.people`, `graph.nodes`, `graph.edges`, `graph.artifact_bodies`, etc.) are included in the standard push/pull sync rotation. Batch sizes: `artifact_bodies` and `artifact_index` use 50 rows per batch; all others use 100. Embeddings are excluded from sync transport — the receiving machine re-generates them via the `index_artifact` job.

### Read endpoints

Query the graph that ingest + processing built. All require the Bearer API key;
reads carry the asker identity (`asker_eeid` in the body, or `X-Asker-User`
header) so results are ACL-filtered and person-weighted. Design:
[docs/plans/12-graph-memory-read.md](docs/plans/12-graph-memory-read.md).

**GET /api/graph/search** — keyword + semantic candidate search:

```bash
curl -s -H "Authorization: Bearer $AGENT_MEM_API_KEY" \
  "http://localhost:34567/api/graph/search?q=TRY%20currency&types=slack_thread,jira&limit=5"
# { "results": [ { "node_id": "...", "score": 0.87, "score_breakdown": {...}, ... } ], "total": 14 }
```

Query params: `q` (required), `types` (CSV filter), `limit` (default 10).

**POST /api/graph/resolve** — seed-driven BFS, scored + hydrated as LLM context:

```bash
curl -s -X POST http://localhost:34567/api/graph/resolve \
  -H "Authorization: Bearer $AGENT_MEM_API_KEY" -H "Content-Type: application/json" \
  -d '{
    "seeds": ["jira:PAY-2128"],
    "query": "what is the TRY currency issue?",
    "asker_eeid": 982,
    "depth": 2,
    "budget_tokens": 4000,
    "include_bodies": true
  }'
# { "artifacts": [ {hop:0 seed}, {hop:1 ...}, ... ], "graph_trace": {...}, "cache_misses": [...] }
```

Seeds may be canonical node ids (`jira:PAY-2128`, `slack:C…:ts`) or raw URLs.

**GET /api/graph/node** — direct lookup by `?url=` or `?id=`, returns the node +
`edges_in` / `edges_out`.

**GET /api/graph/node/{id}/neighbors** — adjacency walk; `?depth=1|2|3`,
`?kind=REFERENCES`, `?direction=in|out|both`.

## Graph MCP server

`agent-mem mcp` serves the graph read API as a stdio MCP server for trusted
operators. It exposes five tools:

- `graph_search` discovers candidate Slack threads, Jira issues, pull requests,
  and documents.
- `graph_node` fetches one artifact by canonical ID or exact source URL.
- `graph_neighbors` walks a bounded set of related artifacts.
- `graph_cluster_summary` returns synthesized review or decision context with
  provenance. It can take roughly 15 seconds and return tens of kilobytes.
- `graph_resolve` builds a question-focused context bundle from one or more
  IDs or ingested source URLs.

The production registration runs the image-shipped binary inside the worker
container over SSH. The SSH account and the worker API key are the security
boundary; this is an operator/admin integration, not a per-user authorization
surface. Do not expose it through an unauthenticated network listener.

Register it with any supported client:

```bash
claude mcp add --scope user agent-mem-graph -- \
  ssh enzo@enzogo.io.vn sudo docker exec -i agent-mem-worker-1 agent-mem mcp

codex mcp add agent-mem-graph -- \
  ssh enzo@enzogo.io.vn sudo docker exec -i agent-mem-worker-1 agent-mem mcp

gemini mcp add --scope user agent-mem-graph \
  ssh enzo@enzogo.io.vn sudo docker exec -i agent-mem-worker-1 agent-mem mcp
```

Verify with `claude mcp list`, `codex mcp list`, or `gemini mcp list`. Remove
the registration with:

```bash
claude mcp remove --scope user agent-mem-graph
codex mcp remove agent-mem-graph
gemini mcp remove --scope user agent-mem-graph
```

For local development, `agent-mem mcp --worker-url http://127.0.0.1:34567`
proxies to a local worker. The command loads the runtime API key from
PostgreSQL when `AGENT_MEM_API_KEY` is absent, probes the protected settings
endpoint before serving, and reserves stdout exclusively for MCP protocol
frames. `--allow-unauthenticated` is intended only for a deliberately
unauthenticated local worker.

## Quick Start

### Local server via Docker

1. Start PostgreSQL and the worker:

```bash
docker compose up -d
```

2. Open the dashboard:

```text
http://localhost:34567
```

The worker runs database migrations on startup.

### Local CLI

Build the binary:

```bash
make build-cli
```

Or install it:

```bash
make install-cli
```

## Claude Code Integration

Claude Code is the default provider for `agent-mem hook` — no provider argument is required. There is no bundled installer yet, so register the four hooks manually in `~/.claude/settings.json` (or `.claude/settings.json` for a project-local install):

```json
{
  "hooks": {
    "SessionStart": [
      { "command": "agent-mem hook session-start", "timeout": 30 }
    ],
    "UserPromptSubmit": [
      { "command": "agent-mem hook prompt-submit", "timeout": 10 }
    ],
    "PostToolUse": [
      { "command": "agent-mem hook post-tool-use", "timeout": 10 }
    ],
    "Stop": [
      { "command": "agent-mem hook stop", "timeout": 30 }
    ]
  }
}
```

The worker normalizes Claude Code hook payloads automatically. On `Stop`, the CLI reads the last assistant message from the JSONL transcript at `~/.claude/projects/.../<session>.jsonl` before Claude Code cleans the file up.

The `mem-search` skill also works with Claude Code — copy `plugin/skills/mem-search/` into `~/.claude/skills/` (user-global) or `.claude/skills/` (project-local).

## Codex Integration

Install the bundled Codex hooks and plugin skills:

```bash
agent-mem install codex --scope project
```

For a user-global install:

```bash
agent-mem install codex --scope user
```

You can also install only the hook config:

```bash
agent-mem install-hooks codex --scope project
```

## Gemini CLI Integration

Install the bundled Gemini CLI hooks and plugin skills:

```bash
agent-mem install gemini --scope project
```

For a user-global install:

```bash
agent-mem install gemini --scope user
```

## Uninstallation

To remove `agent-mem` hooks from your coding agents:

### Project Scope
Delete the local configuration directory in your project root:
- **Codex**: `rm -rf .codex`
- **Gemini CLI**: `rm -rf .gemini`
- **Skills**: `rm -rf .agents`
- **Claude Code**: Edit `.claude/settings.json` to remove the `"hooks"` entries.

### User Scope
Edit your global configuration file and remove the `agent-mem` hook entries from the `"hooks"` object:
- **Claude Code**: `~/.claude/settings.json`
- **Codex**: `~/.codex/hooks.json`
- **Gemini CLI**: `~/.gemini/settings.json`
- **Skills**: `~/.agents/skills/`

## Important Commands

```bash
agent-mem version
agent-mem worker
agent-mem migrate
agent-mem migrate-status
agent-mem migrate-up-by-one
agent-mem migrate-rollback --version <migration_version>
agent-mem migrate-fix --version <migration_version>
agent-mem migrate-sqlite --sqlite-path ~/.claude-mem/claude-mem.db
agent-mem backfill-embeddings
agent-mem install codex --scope project
agent-mem install gemini --scope project
agent-mem install-skill mem-search --scope project

# hook adapters (stdin JSON -> worker; defaults to claude, pass "codex" or "gemini" as 2nd arg)
agent-mem hook session-start
agent-mem hook prompt-submit
agent-mem hook post-tool-use
agent-mem hook stop
```

## Configuration

Core environment variables:

- `DATABASE_URL`
- `AGENT_MEM_WORKER_PORT`
- `AGENT_MEM_LOG_LEVEL`
- `AGENT_MEM_GEMINI_API_KEY` or `GEMINI_API_KEY`
- `AGENT_MEM_GEMINI_MODEL`
- `AGENT_MEM_GEMINI_EMBEDDING_MODEL`
- `AGENT_MEM_GEMINI_EMBEDDING_DIMS`
- `AGENT_MEM_CONTEXT_OBSERVATIONS`
- `AGENT_MEM_CONTEXT_FULL_COUNT`
- `AGENT_MEM_CONTEXT_SESSION_COUNT`
- `AGENT_MEM_SKIP_TOOLS`
- `AGENT_MEM_ALLOWED_PROJECTS`
- `AGENT_MEM_IGNORED_PROJECTS`
- `AGENT_MEM_SYNC_ENABLED`
- `AGENT_MEM_SYNC_URL`
- `AGENT_MEM_SYNC_INTERVAL`
- `AGENT_MEM_API_KEY`
- `AGENT_MEM_MACHINE_ID`

### Graph Memory env vars

Fetcher credentials (see the [Required env vars](#required-env-vars-fetchers) table for what each enables — all optional; a source is simply skipped if its token is unset):

- `AGENT_MEM_SLACK_BOT_TOKEN`
- `AGENT_MEM_JIRA_EMAIL`, `AGENT_MEM_JIRA_TOKEN`, `AGENT_MEM_JIRA_BASE_URL`
- `AGENT_MEM_GH_TOKEN`, `AGENT_MEM_GH_BASE_URL`
- `AGENT_MEM_CF_TOKEN`
- `AGENT_MEM_PAGERDUTY_TOKEN`
- `AGENT_MEM_DATADOG_API_KEY`, `AGENT_MEM_DATADOG_APP_KEY`
- `AGENT_MEM_SENTRY_AUTH_TOKEN`, `AGENT_MEM_SENTRY_ORG`
- `AGENT_MEM_GWS_SERVICE_KEY_PATH`

Behaviour and rate limits:

- `AGENT_MEM_GRAPH_RUNNER` — `any` (default), `vps`, or `local`. Controls which jobs this worker claims (`vps` owns the Slack bot token, so backfill jobs target `vps`).
- `AGENT_MEM_GRAPH_RATE_*` — per-source concurrency caps for the job dispatcher's semaphores: `SLACK`, `JIRA`, `GITHUB`, `CONFLUENCE`, `PAGERDUTY`, `DATADOG`, `SENTRY`, `GWS`, `GEMINI`. Sensible defaults are seeded in `app_settings`; override only to throttle a rate-limited source.

LiteParse (optional, for PDF/office parsing) — see [the LiteParse section](#optional-liteparse-for-fast-pdfoffice-parsing):

- `LITEPARSE_BIN_PATH`, `LITEPARSE_SCREENSHOT_ENABLED`, `LITEPARSE_TEMP_DIR`

Default local ports:

- PostgreSQL: `5433`
- Worker/dashboard: `34567`

Gemini is optional for startup, but required for extraction and hybrid semantic search.

## Dashboard

The React dashboard source lives in [dashboard](/Users/neocapitelo/go/src/github.com/agent-mem/dashboard). The worker serves embedded production assets from [internal/worker/dashboard](/Users/neocapitelo/go/src/github.com/agent-mem/internal/worker/dashboard).

Tabs: Timeline, Search, Sessions, Sync, Logs, Settings, plus the Graph Memory tabs:

- **Graph** — Search (keyword/semantic) and Resolve (paste a seed URL → BFS context) over the artifact graph.
- **Jobs** — queue inspector: filter by status/type, 5s auto-refresh while active, retry/delete actions.
- **Backfill** — form to pull a Slack channel's last *N* months into the graph; links straight to the Jobs tab to watch it drain.

> The worker embeds prebuilt dashboard assets at compile time (`//go:embed`). After changing the frontend, run `npm run build` in `dashboard/` and copy `dashboard/dist/*` into `internal/worker/dashboard/` before rebuilding the worker.

Frontend scripts:

```bash
cd dashboard
npm run dev
npm run build
npm run lint
```

## Development Notes

- `go test ./...` runs the Go test suite
- `docker compose up -d` starts the default local stack
- `make status`, `make logs`, and `make down` manage the stack
- Migration files live in [migrations](/Users/neocapitelo/go/src/github.com/agent-mem/migrations)
- Planning docs live in [docs/plans](/Users/neocapitelo/go/src/github.com/agent-mem/docs/plans)

## Security Notes

- This worker is currently designed for trusted environments and local/dev use
- Do not expose port `34567` directly to untrusted networks
- Hook endpoints are intentionally unauthenticated for local agent integrations
- If you enable sync, review your network exposure and API-key setup before using it outside localhost
