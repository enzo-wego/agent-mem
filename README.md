# agent-mem

`agent-mem` is a Go service and CLI for persistent coding-agent memory. It captures local hook events, stores prompts/observations/session summaries in PostgreSQL with `pgvector`, uses Gemini for extraction and embeddings, and serves a small dashboard for search and inspection.

## What It Does

- Runs a long-lived worker on `:34567`
- Accepts hook events such as session start, prompt submit, post-tool-use, and stop
- Stores prompts, observations, summaries, and sync metadata in PostgreSQL
- Builds relevant context for future sessions
- Exposes a dashboard and JSON API for search, timelines, sync, settings, and logs
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
│                                                              │
└────────────┬───────────────────────┬─────────────────────────┘
             │                       │
             ▼                       ▼
   ┌───────────────────┐    ┌───────────────────────┐
   │  PostgreSQL +     │    │  Gemini API           │
   │  pgvector         │    │  (extract + embed)    │
   └───────────────────┘    └───────────────────────┘
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
- [dashboard](/Users/neocapitelo/go/src/github.com/agent-mem/dashboard)
- [plugin/skills](/Users/neocapitelo/go/src/github.com/agent-mem/plugin/skills)

## Graph Memory

Graph Memory builds a cross-source knowledge graph that links Slack messages, Jira tickets, GitHub PRs, Confluence pages, PagerDuty incidents, Datadog monitors, Sentry issues, and Google Workspace docs into a single queryable artifact store. Each source is a node; relationships extracted from bodies (references, mentions, ownership) become typed edges.

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

Optional: `AGENT_MEM_GRAPH_RUNNER` (default `any`), `AGENT_MEM_JIRA_BASE_URL`, `AGENT_MEM_GH_BASE_URL`.

### Optional: LiteParse for fast PDF/office parsing

agent-mem can use [LiteParse](https://github.com/run-llama/liteparse) to
extract text from PDF/DOCX/XLSX/PPTX locally before falling back to Gemini
multimodal. This is faster (~50–200ms vs 2–5s) and avoids API cost for
text-heavy documents.

Install on the VPS:

```bash
cargo install liteparse   # installs the `lit` CLI binary
# or
npm install -g @llamaindex/liteparse   # ships a prebuilt binary
```

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

### Phase 3 — read endpoints

Search (`/api/graph/search`), entity resolve (`/api/graph/resolve`), and node detail (`/api/graph/node/{id}`) ship in Phase 3. See [docs/plans/12-graph-memory-read.md](docs/plans/12-graph-memory-read.md).

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

Default local ports:

- PostgreSQL: `5433`
- Worker/dashboard: `34567`

Gemini is optional for startup, but required for extraction and hybrid semantic search.

## Dashboard

The React dashboard source lives in [dashboard](/Users/neocapitelo/go/src/github.com/agent-mem/dashboard). The worker serves embedded production assets from [internal/worker/dashboard](/Users/neocapitelo/go/src/github.com/agent-mem/internal/worker/dashboard).

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
