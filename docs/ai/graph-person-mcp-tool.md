# Plan: `graph_person` — person lookup endpoint + MCP tool

Written 2026-08-15. Worker brief — assumes no memory of the conversation that
produced it.

## Goal

Answer "who is Ross / Lei / Chu Yeow at Wego?" through agent-mem. The data
already exists (`graph.people` from BambooHR + lazy identity resolution,
`graph.person_derived_roles` from the daily role job); nothing exposes it as a
person profile. Add one worker read endpoint and one MCP tool on top of it.

## Non-goals

- No person nodes in `graph.nodes` / no `artifact_index` rows for people — we
  are NOT making people semantically searchable, just directly look-up-able.
- No changes to identity resolution, BambooHR import, or role derivation.
- No dashboard UI.
- No deploy in this round (needs an explicit request).

## Files expected to change

| File | Change |
|---|---|
| `internal/graph/handlers/person.go` | NEW — `GET /api/graph/person` handler |
| `internal/graph/handlers/person_test.go` | NEW — handler tests (scratch test DB) |
| `internal/graph/handlers/router.go` | register the route next to `/api/graph/node` |
| `internal/graphmcp/client.go` | add `Person(ctx, q, limit)` proxy method |
| `internal/graphmcp/server.go` | add `graph_person` tool (6th tool) |
| `internal/graphmcp/client_test.go`, `server_test.go` | cover the new method/tool |
| `README.md` | add the endpoint to "Read endpoints", add the tool to the "Graph MCP server" list |

## Approach

### 1. Worker endpoint — `GET /api/graph/person?q=<query>&limit=<n>`

Same auth as the other graph reads (Bearer API key via the existing router
middleware). `q` required; `limit` optional, default 5, max 20.

Match order (first hit wins, on `graph.people` with `merged_into IS NULL`
after following `merged_into` chains to the canonical row):

1. all-digits `q` → `eeid` exact
2. `q` contains `@` → `email` exact (CITEXT)
3. `^U[A-Z0-9]+$` → `slack_user_id` exact
4. otherwise → `display_name ILIKE '%q%'` (and `github_login` exact as a
   bonus OR), ordered by `is_bot ASC, depth_from_root ASC NULLS LAST`, capped
   at `limit` candidates

Response per candidate:

```json
{
  "people": [{
    "person_id": 42, "eeid": 982, "display_name": "Lei Zheng",
    "email": "lei@wego.com", "slack_user_id": "UUK3WPNNQ",
    "github_login": "...", "jira_account_id": "...",
    "is_bot": false, "depth_from_root": 3,
    "manager_chain": [{"eeid": 500, "display_name": "..."}, ...],
    "direct_reports": 4,
    "derived_role": {"domain": "payments", "role_label": "backend engineer",
                      "confidence": 0.9, "evidence": {...}, "computed_at": "..."},
    "recent_artifacts": [{"id": "slack:C..:17..", "type": "slack_thread",
                           "title": "...", "url": "...", "updated_at": "..."}]
  }],
  "total": 1
}
```

- `manager_chain`: walk `reports_to` (an eeid) upward via `graph.people`,
  hard cap 10 hops, cycle-safe (track visited eeids).
- `direct_reports`: `COUNT(*) FROM graph.people WHERE reports_to = $eeid AND merged_into IS NULL`.
- `derived_role`: `graph.person_derived_roles` by eeid; omit the key when absent.
- `recent_artifacts`: top 10 `graph.nodes` by `author_person_id = person_id`
  (canonical id), `deleted_at IS NULL`, ordered `updated_at DESC`.
- People without an eeid (lazily created, unmatched to BambooHR) still return —
  with empty chain/role — rather than being filtered out.

Errors: 400 on empty `q` or bad `limit`; empty `people` array (200) on no match.

### 2. MCP tool — `graph_person`

Follow the exact `mcp.AddTool` pattern in `internal/graphmcp/server.go`:

- Input: `{q string, limit int}` — trim `q`, reject empty; default limit 5,
  bounds 1–20 (mirror `graph_search` validation style).
- `client.Person` follows the `doJSON` GET pattern used by `Search`.
- Description (agents pick tools off this): "Look up a person at the
  organization by name, email, employee id, or Slack user id. Returns their
  profile, manager chain, inferred role with evidence, and recent artifacts
  they authored."

### 3. README

- Read endpoints section: add `GET /api/graph/person` with one curl example.
- Graph MCP server section: 5 tools → 6, one bullet for `graph_person`.

## Constraints (hard)

- **Tests must run against the scratch test DB only** — use the existing
  `openTestDB` helper in `internal/graph/handlers/testdb_test.go` (it is
  guarded; do not weaken the guard, do not point tests at the live dev DB).
- Match surrounding code style; no new dependencies.
- Cheap queries only — no LLM calls anywhere in this path.

## Acceptance criteria

1. `GET /api/graph/person?q=Lei` (Bearer-authed) returns candidates with
   profile, manager chain, derived role, recent artifacts; without the Bearer
   key it is rejected like the sibling read endpoints.
2. Lookup works by display-name substring, exact email, all-digits eeid, and
   `U…` Slack id; a merged person resolves to its canonical row.
3. `graph_person` is registered (server exposes 6 tools), validates input
   (empty q rejected, limit bounds enforced), and proxies the endpoint.
4. Handler tests cover: name match, email match, merged→canonical, manager
   chain with a cycle guard, no-match → empty list, missing derived role.
5. `go build ./...` and `go test ./internal/graph/... ./internal/graphmcp/...`
   pass.
6. README updated as above.

## How to verify (reviewer)

- `go test ./internal/graph/handlers/ ./internal/graphmcp/ -run 'Person|Server' -v`
- `go vet ./...`
- Read the diff for fake completion: no `t.Skip`, no TODO stubs.
- Optional live check (not gating): run the worker locally against the test
  stack and `curl -H "Authorization: Bearer $AGENT_MEM_API_KEY" 'localhost:34567/api/graph/person?q=Lei'`.
