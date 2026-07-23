# Plan: publish the agent-mem graph as an MCP server

**Status:** implemented

**Baseline:** `main` at `817425f72620195f9eb50a45206cd036076de0c1`
on 2026-07-23.

## Outcome

Add `agent-mem mcp`, a stdio MCP server with five read-only graph tools:

1. `graph_search`
2. `graph_node`
3. `graph_neighbors`
4. `graph_cluster_summary`
5. `graph_resolve`

Production runs the image-shipped binary through this chain:

```text
Mac MCP client
  -> ssh enzo@enzogo.io.vn
  -> sudo docker exec -i agent-mem-worker-1 agent-mem mcp
  -> http://127.0.0.1:34567/api/graph/*
  -> running worker
```

## Decisions

- Use the official Go SDK pinned to
  `github.com/modelcontextprotocol/go-sdk@v1.6.1`.
- Serve stdio only. Do not add a public MCP listener or worker route.
- Run inside `agent-mem-worker-1`, not through the separately installed host
  binary.
- Load runtime settings from PostgreSQL once at startup when the environment
  does not provide an API key. Apply environment variables last and close the
  database pool before serving.
- Treat this as a trusted operator/admin integration. Do not accept an
  `asker_eeid` or claim per-user authorization.
- Do not alter cluster-summary caching or graph ACL behavior.
- Do not expose a neighbor `direction`; the current endpoint traverses both
  directions.
- Canonicalize URL-shaped resolve seeds before BFS traversal while preserving
  the original seed in `graph_trace`.
- Do not vendor dependencies and do not rebuild the dashboard.

## Tool contracts

All tools return the worker JSON object as `map[string]any`, producing both
structured MCP output and a JSON text fallback.

### `graph_search`

Input:

- `q` string, required
- `types` string array, optional
- `limit` integer, default `10`, range `1..50`

Worker request:

```text
GET /api/graph/search?q=...&types=slack_thread,jira&limit=10
```

### `graph_node`

Input: exactly one of `id` or `url`.

Worker request:

```text
GET /api/graph/node?id=...
GET /api/graph/node?url=...
```

### `graph_neighbors`

Input:

- `id` string, required
- `depth` integer, default `1`, range `1..3`
- `kinds` string array, optional

Worker request:

```text
GET /api/graph/node/{path-escaped-id}/neighbors?depth=1&kind=REFERENCES
```

IDs containing `:`, `/`, and `#` must be path-escaped.

### `graph_cluster_summary`

Input:

- `node` canonical node ID, required
- `depth` integer, default `2`, range `1..3`

Worker request:

```text
GET /api/graph/cluster/summary?node=...&depth=2
```

The description warns callers that synthesis can take roughly 15 seconds and
return tens of kilobytes.

### `graph_resolve`

Input:

- `seeds` string array, required, `1..20` canonical IDs or ingested URLs
- `query` string, required
- `depth` integer, default `2`, range `1..3`
- `budget_tokens` integer, default `4000`, range `500..16000`
- `include_bodies` boolean, default `true`

Worker request:

```text
POST /api/graph/resolve
Content-Type: application/json

{
  "seeds": ["https://github.com/wego/payments/pull/2198"],
  "query": "is WithRebateRepo safe to remove?",
  "depth": 2,
  "budget_tokens": 4000,
  "include_bodies": true
}
```

## HTTP proxy

`internal/graphmcp` owns the transport behavior independently of Cobra:

- injected base URL, API key, and `*http.Client`;
- `http.NewRequestWithContext` for cancellation;
- `url.Values` for query strings and `url.PathEscape` for neighbor IDs;
- bearer authorization on every request;
- JSON content type for POST;
- 90-second timeout;
- 8 MiB successful-response cap and bounded error bodies;
- object-shaped JSON decoding;
- concise non-2xx errors.

Before serving, `agent-mem mcp` probes protected `GET /api/settings` with a
five-second timeout so bad worker URLs and API keys fail early.

## Runtime bootstrap

`newMCPCmd(getCfg func() *config.Config)` executes after Cobra's existing
configuration pre-run:

1. Use `AGENT_MEM_API_KEY` when already present.
2. Otherwise connect to `cfg.DatabaseURL`.
3. Read `database.NewDB(pool).GetAllSettings(ctx)`.
4. Apply database settings.
5. Reapply environment settings for final precedence.
6. Close the pool.
7. Refuse an empty key unless `--allow-unauthenticated` is explicit.
8. Proxy to `--worker-url`, or
   `http://127.0.0.1:<configured-worker-port>` by default.

Stdout is reserved for JSON-RPC. Diagnostics and application logs use stderr.

## Implementation and verification

The implementation proceeds in test-first slices:

1. Add a PostgreSQL-backed test for raw URL resolve seeds, see it fail, then
   canonicalize only URL-shaped seeds before constructing the BFS frontier.
2. Add exact-route HTTP client tests, then implement the bounded worker proxy.
3. Use official in-memory MCP transports to test tool listing, schemas,
   structured output, defaults, validation, worker failures, and cancellation.
4. Add Cobra tests using an `httptest.Server` and environment API key so the
   tests cannot connect to PostgreSQL.
5. Run focused tests, the full Go suite, vet, build, and `git diff --check`.
6. Re-run the resolve URL test against a disposable migrated pgvector
   PostgreSQL container rather than relying on its no-database skip.

Expected source surface:

```text
README.md
cmd/agent-mem/main.go
cmd/agent-mem/mcp.go
cmd/agent-mem/mcp_test.go
docs/plans/20-graph-mcp-server.md
go.mod
go.sum
internal/graph/handlers/resolve.go
internal/graph/handlers/resolve_test.go
internal/graphmcp/client.go
internal/graphmcp/client_test.go
internal/graphmcp/server.go
internal/graphmcp/server_test.go
```

No migration, dashboard asset, generated code, or worker-route change is
expected.

## Registration

```bash
claude mcp add --scope user agent-mem-graph -- \
  ssh enzo@enzogo.io.vn sudo docker exec -i agent-mem-worker-1 agent-mem mcp

codex mcp add agent-mem-graph -- \
  ssh enzo@enzogo.io.vn sudo docker exec -i agent-mem-worker-1 agent-mem mcp

gemini mcp add --scope user agent-mem-graph \
  ssh enzo@enzogo.io.vn sudo docker exec -i agent-mem-worker-1 agent-mem mcp
```

Verify with `claude mcp list`, `codex mcp list`, and `gemini mcp list`.

## Deployment and smoke test

After commits and beads state are pushed:

```bash
make deploy

ssh enzo@enzogo.io.vn \
  'sudo docker exec agent-mem-worker-1 agent-mem mcp --help'
```

Register at least Claude Code and run:

```bash
claude -p \
  --allowedTools "mcp__agent-mem-graph__graph_node" \
  "Call graph_node with id jira:PAY-2223. Return only node_id and title."
```

Then call `graph_resolve` with
`https://github.com/wego/payments/pull/2198`; the canonical
`gh_pr:wego/payments#2198` and related artifacts must be present.

## Follow-up

Track automatic `wego/payments` PR ingestion separately as P2. The production
check on 2026-07-23 found #2196, #2197, and #2198, while #2199 through #2202
were missing. That investigation must not expand the MCP implementation.
