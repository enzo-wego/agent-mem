# Native Pi Integration Design

## Goal

Provide first-class agent-mem support for Pi through one idempotent command:

```bash
agent-mem install pi --scope user
```

The integration must capture coding-session activity in flat memory and expose both flat-memory and graph-memory reads as native Pi tools without requiring MCP support in Pi.

## User experience

A new machine can install or upgrade the integration by pulling `main`, building or installing the `agent-mem` binary, and running:

```bash
agent-mem install pi --scope user
```

The command installs:

- `~/.pi/agent/extensions/agent-mem.ts`
- `~/.agents/skills/mem-search/SKILL.md`

Project scope remains available through `--scope project`, targeting `.pi/extensions/agent-mem.ts` and `.agents/skills/mem-search/SKILL.md` below the selected project directory. Re-running the command updates managed files safely. Pi must be reloaded or restarted after installation.

## Architecture

The integration is a native TypeScript Pi extension. It uses Pi lifecycle events for writes and registers typed Pi tools for reads. It communicates with the existing agent-mem worker HTTP API directly; it does not introduce an MCP bridge or duplicate worker-side persistence and search logic.

The extension has three responsibilities:

1. Translate Pi lifecycle events into the existing `agent-mem hook` CLI contract.
2. Recall flat-memory context at session start and inject it before the first agent run.
3. Register native read tools backed by existing flat-memory and graph-memory worker endpoints.

A shared extension-local HTTP client handles worker URL resolution, bearer authentication, cancellation, errors, and bounded output.

## Flat-memory writes

Pi events map to agent-mem events as follows:

| Pi event | agent-mem event | Behavior |
|---|---|---|
| `session_start` | `session-start` | Fetch project recall context |
| `before_agent_start` | `prompt-submit` | Record the user prompt |
| `tool_result` | `post-tool-use` | Record tool name, input, bounded result, and error state |
| `agent_settled` | `stop` | Record the final assistant response and queue summary processing |
| `session_shutdown` | internal flush | Await bounded outstanding writes before teardown |

The extension sends the canonical snake-case payload already accepted by `internal/hooks` and `internal/worker`. Prompt and tool writes may run asynchronously to avoid adding latency, but outstanding work is tracked. Final summary writes and shutdown flushing are awaited with timeouts. Failures never block or terminate Pi.

To avoid duplicate summaries, the extension tracks whether the current settled interaction has already emitted `stop`. Session replacement reloads extension state through Pi's normal extension lifecycle.

## Startup recall

On `session_start`, the extension invokes `agent-mem hook session-start` and caches returned context. On the first `before_agent_start`, it injects that context as a custom Pi message and clears the cache. This makes recall part of the first model request without triggering an unsolicited model turn at startup.

If the binary or worker is unavailable, startup continues without recalled context.

## Native flat-memory tools

The extension registers:

- `memory_search`: hybrid semantic and full-text search over observations and summaries.
- `memory_search_by_file`: retrieve observations related to a repository path.
- `memory_timeline`: retrieve project observations in a date range.
- `memory_list_observations`: list observations with an optional type filter.
- `memory_get_observation`: retrieve one complete observation by ID.

The current project defaults to the basename of `ctx.cwd`, matching worker project extraction. An explicit project parameter may override it where useful. Input limits are validated by TypeBox schemas.

## Native graph-memory tools

The extension registers equivalents of the existing MCP tools:

- `graph_search`
- `graph_node`
- `graph_neighbors`
- `graph_resolve`
- `graph_cluster_summary`
- `graph_person`

Their argument constraints and descriptions remain aligned with `internal/graphmcp/server.go`. They call the same worker graph HTTP endpoints used by `internal/graphmcp.Client`, preserving worker-side behavior and authorization.

## Configuration and authentication

The extension resolves configuration in this order:

1. `AGENT_MEM_WORKER_URL`, when set.
2. `http://127.0.0.1:${AGENT_MEM_WORKER_PORT}`, when the port is set.
3. `http://127.0.0.1:34567`.

Read requests include `Authorization: Bearer $AGENT_MEM_API_KEY` when the variable is present. Hook writes continue through the `agent-mem` binary, selected by `AGENT_MEM_BIN` or defaulting to `agent-mem`, so existing database-backed runtime configuration remains authoritative.

No credential is persisted into the installed extension or skill.

## Errors and output limits

Memory is optional infrastructure from Pi's perspective:

- Lifecycle failures are swallowed after bounded cleanup and never interrupt coding.
- Read-tool failures are returned as tool errors with actionable messages.
- Abort signals cancel HTTP reads.
- Responses are capped consistently before entering model context; complete structured data may be retained in tool-result details only when within a safe bound.
- Long graph synthesis calls receive a larger but finite timeout than ordinary searches.

## Installation

The Go installer gains a `pi` provider and `agent-mem install pi` command. The provider is distinct from `omp`: native Pi uses `@earendil-works/pi-coding-agent` and `.pi/extensions`, while OMP retains its existing package import and paths.

The extension source is embedded in the Go binary, making installation independent of the repository checkout after the binary has been built. The existing embedded `mem-search` skill is installed through `internal/agentinstall`. Its guidance is updated to prefer native Pi tools when available while retaining curl instructions for agents without native tools.

## Testing

Automated tests cover:

1. Pi provider normalization and rejection of unsupported providers.
2. User and project destination paths.
3. Creation, replacement, and idempotent reinstall of the embedded extension.
4. Combined installation of the Pi extension and embedded skills.
5. Static extension contract checks for expected imports, lifecycle subscriptions, tool names, endpoint mappings, authentication, and output bounds.
6. Existing Go test suites and binary build.

Machine validation then installs the user-scoped integration and exercises it from the payments repository:

1. Start or verify the worker.
2. Launch a fresh Pi session or a non-interactive Pi run with the installed extension.
3. Submit a unique prompt and perform a tool call.
4. Verify the prompt and resulting observation through flat-memory APIs.
5. Invoke `memory_search` and verify the unique activity can be read.
6. Invoke `graph_search`, then fetch or resolve a returned graph artifact.

No production mutation beyond normal memory capture is required. Changes are committed and pushed to `origin/main` only after automated and machine validation pass.

## Scope boundaries

This work does not change graph ingestion, flat-memory extraction semantics, database schema, MCP support for other agents, or the OMP adapter. It does not automatically install Pi, start PostgreSQL, or provision worker credentials. Documentation will state those prerequisites and include new-machine and upgrade commands for the payments Mac mini.
