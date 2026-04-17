# agent-mem

`agent-mem` is a Go service and CLI for persistent coding-agent memory. It captures local hook events, stores prompts/observations/session summaries in PostgreSQL with `pgvector`, uses Gemini for extraction and embeddings, and serves a small dashboard for search and inspection.

## What It Does

- Runs a long-lived worker on `:34567`
- Accepts hook events such as session start, prompt submit, post-tool-use, and stop
- Stores prompts, observations, summaries, and sync metadata in PostgreSQL
- Builds relevant context for future sessions
- Exposes a dashboard and JSON API for search, timelines, sync, settings, and logs
- Installs Codex hooks and bundled plugin skills from the CLI

## Architecture

```text
coding-agent hooks
  -> agent-mem hook <event>
  -> agent-mem worker (:34567)
  -> PostgreSQL + pgvector
  -> dashboard/API
```

Main code paths:

- [cmd/agent-mem/main.go](/Users/neocapitelo/go/src/github.com/agent-mem/cmd/agent-mem/main.go)
- [internal/worker](/Users/neocapitelo/go/src/github.com/agent-mem/internal/worker)
- [internal/database](/Users/neocapitelo/go/src/github.com/agent-mem/internal/database)
- [dashboard](/Users/neocapitelo/go/src/github.com/agent-mem/dashboard)
- [plugin/skills](/Users/neocapitelo/go/src/github.com/agent-mem/plugin/skills)

## Quick Start

### Docker

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

Then run the worker against a PostgreSQL instance:

```bash
agent-mem worker
```

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
agent-mem install-skill mem-search --scope project
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
