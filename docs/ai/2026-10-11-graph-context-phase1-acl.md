# Graph context Phase 1: access control for real askers

Date: 2026-10-11. Owner: Enzo. Status: draft r3 (after pi reviews r1 and r2, `docs/reviews/2026-10-11-graph-context-phase1-acl*.md`), needs pi re-review and Enzo's approval.
REVIEWER: pi (auto: GPT weekly 85% left)
STATUS: approved (Enzo, 2026-10-11 22:15 SGT); O1 = follow-up p-agent round
Parent: `docs/ai/2026-10-11-graph-context-quality.md`, Phase 1. This plan replaces the
parent's Phase 1 table where they differ: it applies Enzo's decisions D1 and D3
(2026-10-11), which supersede the parent's "trusted" flag and Q1/Q3.
Repo: agent-mem only. Base: `main` at `8b10d8e` or later. Branch
`feat/graph-context-phase1-acl`, worktree `/Users/enzo/go/src/github.com/agent-mem-wt-acl`.

## Goal

Every graph read route returns only what the named asker may see. A call that names
no asker fails, unless it carries the explicit admin header. Jira, GitHub and
Confluence content becomes visible to every employee with an eeid, so real askers get
their Jira / PR seeds back.

Decisions that bind this plan:

- **D1.** Every employee with an eeid may see stored Jira, GitHub and Confluence
  content. GWS stays per file. Slack stays per channel.
- **D3.** No second API key. Unfiltered access only via `X-Graph-Admin: 1`. A call
  with no asker gets an error. The MCP fails closed. The ACL protects against
  mistakes, not against a hostile local process (anyone holding the API key can send
  the admin header).

## Non-goals

- PII masking of bodies (D2). Separate round, Phase 1b. p-agent stays off
  (`GRAPH_CONTEXT_ENABLED=false`) until both are deployed.
- Any p-agent change. p-agent's graph-context call is disabled; its later plan will
  send `asker_slack_id`.
- GWS per-file scopes. Today GWS nodes carry the bare scope `gws` and no job grants
  it, so filtered askers keep seeing none of them. That stays (fails closed).
- The `scopes_stale` field and the person-node alias from the parent plan. Not
  needed: p-agent will send `asker_slack_id` instead of looking up a person node.
- Ranking, dedup, labels (Phases 2-5).

## Facts (HEAD 8b10d8e; worker re-checks)

- Shared asker helper `askerScopeSetDiag` (`internal/graph/handlers/acl_guard.go:27-43`):
  empty `X-Asker-User` means `noFilter=true`, so the request is unfiltered. Unknown
  person, NULL eeid or lookup error gives `{"public"}` only.
- `lookupAsker` (`search.go:750-768`) matches `slack_user_id`, `email`,
  `github_login`, `jira_account_id`; no `merged_into` filter; a numeric string is
  never interpreted as an eeid (it could only match one of those columns). It
  returns DB errors to the caller (`:761-766`).
- `/resolve` takes body `asker_eeid` only (`resolve.go:29`); 0 or absent is
  unfiltered (`resolve.go:297`); any non-zero value is treated as resolved even when
  no person has that eeid (`resolve.go:305`); the scope-builder error is ignored
  (`resolve.go:208`).
- Routes using `X-Asker-User`: `/search` (`search.go:161`), `/node` (`node.go:104`),
  `/node/{id}/neighbors` incl. `cards=1` (`neighbors.go:148`), `/epic/{key}`
  (`epic.go:152`). `/cluster/summary` returns 403 for any non-empty asker
  (`cluster_summary.go:124`).
- Routes with no asker handling (`router.go:16,21-105`): every other GET under
  `/api/graph/`, listed in full under Routes below. Some carry identity or content:
  `/slack-user` returns a profile with eeid, email and department
  (`slack_user_api.go:14-21,43-56`); `/slack-users` the Slack id to display-name map
  (`slack_users_api.go:8-28`); `/subscriptions` subscriber identity, topics and
  source refs (`detect_hot_topics.go:784-824`); `/jobs` raw job payloads and errors
  (`jobs_admin.go:24-90`); `/continents` channel ids and names (`channels.go:20-44,673-700`).
- Visibility: `scopeVisible` (`acl_guard.go:51-58`), `parentVisibleSQL`
  (`acl_guard.go:84-90`, used for attachments) and `aclVisibleSQL`
  (`acl_guard.go:93-117`, also behind `nodeVisible`): NULL / '' / `public` visible
  to all, otherwise exact membership in the asker's scope list. `acl.For(eeid)` (`internal/graph/acl/scopes.go:37-72`)
  reads only `graph.member_scopes`, written only by the Slack member refresh. So no
  filtered asker sees any `jira:*`, `github:*` or `confluence:*` node today.
- Scope strings (`fetch_body.go:369-396`, `ingest_content.go:640-662`):
  `slack:<channel>`, `jira:<PROJECT>`, `github:<org/repo>`, `confluence:<SPACE>`,
  `public`, or a bare source name (`gws`, `slack`, `jira`, ...).
- Auth: one shared Bearer key (`internal/worker/sync_handlers.go:334-354`), skipped
  when no key is configured or `sync_url` is set (`:344-354`); on the hub the key is
  set.
  `X-Graph-Caller` is only written to traces (`context_trace.go:82`).
- MCP (`internal/graphmcp/client.go:181-187`) sends Bearer and `X-Graph-Caller: mcp`,
  no asker: always unfiltered. Tools (`server.go:76-206`): `graph_search`,
  `graph_node`, `graph_neighbors`, `graph_cluster_summary` (calls `/cluster/summary`),
  `graph_resolve`, `graph_person` (calls `/person`), `graph_epic`. Startup calls
  `Client.Probe` (`/api/settings`, `client.go:152-154`, `cmd/agent-mem/mcp.go:70`). Claude Code runs it as
  `docker exec -i agent-mem-worker-1 agent-mem mcp` (payments project entry in
  `~/.claude.json`), so the MCP process sees the container's env, not the caller's.
- Dashboard `dashboard/src/api.ts`: `authFetch` (11-19) sends only the Bearer key; no
  asker anywhere, so all reads are unfiltered.
- `graph.context_traces.acl_mode` takes unfiltered / filtered / public-only /
  filtered-membership-error / not-evaluated (`context_trace.go:285-289`; set at
  `search.go:165-171`, `resolve.go:297-303`); TEXT column, no enum. Only `/search` and
  `/resolve` are traced (`router.go:40-44`). `asker_input` records the header and
  body `asker_eeid` only (`context_trace.go:121-133`).
- Eval: case YAMLs carry executable requests with a variant key
  (`testdata/eval/graph_context/cases/case-01.yaml:9-11` names `resolve-asker0`);
  `evalClient.do` takes no per-request headers (`eval_common.go:163-175`); dry-run
  prints hard-coded header text (`eval_relevance.go:169-173`); prepare refers to the
  old variant (`eval_prepare.go:80,372`).
- p-agent calls `/resolve`, `/search`, `/node`, `/node/{id}/neighbors`,
  `/slack-user` (`src/graph-context/client.js`), used by the disabled context
  injection and by the owner-only Mem search Home tab.

## Behaviour after this change

One asker resolver used by the five filtered routes (`/search`, `/resolve`,
`/node`, `/node/{id}/neighbors`, `/epic/{key}`).

Asserted identities: the `X-Asker-User` header (trimmed; empty or whitespace-only
means absent), and on `/resolve` the body fields `asker_slack_id` (new; same
trimming) and `asker_eeid` (absent or `0` means absent). `X-Graph-Admin` counts only
when its value is exactly `1`; any other value is ignored.

Steps, first match wins:

| # | Condition | Result |
|---|---|---|
| 1 | `X-Graph-Admin: 1` | unfiltered, `acl_mode='admin'`; identities are not resolved or checked; one info log line with route and `X-Graph-Caller` |
| 2 | no asserted identity | `403 asker_required` |
| 3 | `asker_eeid` < 0 or not a valid int32; header all digits but not a positive int32 | `400 asker_invalid` |
| 4 | person lookup returns a DB error | `503 acl_unavailable` |
| 5 | any asserted identity matches no person, or its person has NULL eeid after following `merged_into` | `403 asker_unknown` |
| 6 | asserted identities resolve to more than one eeid | `400 asker_conflict` |
| 7 | scope builder returns an error | `503 acl_unavailable` |
| 8 | otherwise | filtered as that eeid, `acl_mode='filtered'` |

- Header lookup: all digits means `graph.people.eeid`; otherwise the four identity
  columns as today. A row with `merged_into` set is followed until a row with
  `merged_into IS NULL`, at most 3 hops; a loop, a missing target or a NULL eeid at
  the end gives `asker_unknown`.
- Every 4xx / 5xx above on a traced route is traced with `acl_mode='rejected'` and
  no items (guard runs inside the trace wrapper). `asker_input` also records
  `body_asker_slack_id`.

**Org-wide scopes (D1).** For a resolved asker (always has an eeid by the rules
above), a node is visible when its scope is NULL / '' / `public`, **or** is in the
asker's member scopes, **or** its source prefix (text before the first `:`, or the
whole scope if it has none) equals one of `jira`, `github`, `confluence` exactly
(`jiraevil:X` and `jira-x` are not org scopes; no `LIKE` prefix). One Go constant
holds that list. `scopeVisible`, `aclVisibleSQL` **and `parentVisibleSQL`** all use
the same org-aware predicate, so an attachment stays parent-gated but becomes
visible when its parent is visible through the org rule. A test asserts the Go and
SQL predicates agree on a table of scopes.

**Routes.** Read-route policy, by literal method and path (all under `/api/graph`):

| Method and path | After |
|---|---|
| GET `/search`, GET `/node`, GET `/node/{id}/neighbors` (incl. `cards=1`), GET `/epic/{key}`, POST `/resolve` | asker resolver above |
| every other GET under `/api/graph/`: `/jobs`, `/topic-rules`, `/person`, `/traces/stats`, `/slack-users`, `/slack-user`, `/channels`, `/channels/recent`, `/channel`, `/channel/topics`, `/cluster/summary`, `/continents`, `/channel-filters`, `/eligibility-gate`, `/business-root`, `/temporal-timezone`, `/boost-alphas`, `/epic-briefs`, `/purpose`, `/slack-members`, `/jira-updates`, `/gh-key-search`, `/subscriptions`, `/pins`, `/pins/board`, `/node/{id}/subject-queries` | `403 admin_required` without `X-Graph-Admin: 1` |

- Implement as one middleware on the graph router: a GET not in the filtered set
  needs admin. A GET route added later is admin-only by default.
- POST / PUT / DELETE routes (writes, ingest, backfills) are out of scope and
  unchanged.
- A mounted-router test walks the chi routes and fails if any GET under
  `/api/graph/` is neither in the filtered set nor admin-gated.

**Callers.**

- Dashboard: `authFetch` adds `X-Graph-Admin: 1` to every request. Rebuild the
  embedded dashboard per CLAUDE.md.
- MCP (`internal/graphmcp`): reads `AGENT_MEM_ASKER` from its env and sends it as
  `X-Asker-User` on the five filtered tools. Missing, empty or whitespace-only: each
  of those tools returns a clear tool error ("agent-mem MCP needs AGENT_MEM_ASKER
  (Slack user id) set by the caller") without any HTTP call. The check lives in the
  graph tool methods, not in `doJSON`, so `Probe` and startup are unaffected. It
  never sends the admin header. `graph_cluster_summary` and `graph_person` are
  removed from the tool list in this phase (their endpoints are admin-only); the
  README says so. Because the MCP runs inside the container, the
  caller's value reaches it only via `docker exec -i -e AGENT_MEM_ASKER ...`; the
  worker documents that `~/.claude.json` arg change in the README / CLAUDE.md but does
  not edit `~/.claude.json` (conductor does it after deploy, with Enzo's OK).
- Eval harness (`cmd/agent-mem/eval*.go`): variant `resolve-asker0` becomes
  `resolve-admin` (admin header, the old unfiltered behaviour, kept as the
  comparison arm); `search-hybrid` sends the case's asker eeid in `X-Asker-User`;
  `prepare` sends each variant with that variant's own effective headers, the same
  as `relevance` (admin only on `resolve-admin`; a real identity and no admin on
  `resolve-asker` and `search-hybrid`). `label-sheet` makes no HTTP call and stays a
  reader of the saved `replay.json` plus the read-only DB, using the renamed keys.
  Effective headers travel with the request through the real HTTP path and dry-run
  prints them from the same value. All 20 checked-in case YAMLs are rewritten to the new variant key (labels,
  label_reasons and seeds untouched; a test asserts the label maps are byte-equal
  before and after). `replay.json` keys follow the rename.

## Files expected to change

`internal/graph/handlers/acl_guard.go`, `search.go` (`lookupAsker`),
`search_arms.go`, `resolve.go`, `node.go`, `neighbors.go`, `epic.go`,
`cluster_summary.go`, the no-asker route handlers listed above, `router.go` if a
middleware is the smallest way to gate admin-only routes,
`internal/graph/acl/scopes.go` (only if the org rule lives there),
`context_trace.go`, `internal/graphmcp/client.go`, `server.go`,
`dashboard/src/api.ts` + embed, `cmd/agent-mem/eval*.go`,
`testdata/eval/graph_context/cases/*.yaml`, `replay.json`, tests beside each,
README / CLAUDE.md (asker contract, MCP env), this plan's Findings.
No migration: the org list is a constant (D1 is a fixed decision, not runtime config).

## Acceptance

1. Resolver: one test per row of the steps table on `/resolve` and `/search`, plus:
   whitespace-only header, `X-Graph-Admin: true` / `0` (not admin), admin with a
   conflicting identity (still admin), header valid + body unknown (`asker_unknown`),
   header and body naming different people (`asker_conflict`), two body fields naming
   different people (`asker_conflict`), merge chain of 1 hop, loop and missing
   target, DB lookup error (503).
2. Filtering on mounted routes: fixtures with a hidden Slack root, a hidden reply and
   a hidden attachment. A named non-member gets none of their ids, titles, bodies or
   edge metadata from `/search` (legacy and hybrid, incl. linked rows), `/resolve`,
   `/node` (404), `/node/{id}/neighbors` (plain and `cards=1`) and `/epic/{key}`; an
   admin call gets them. Unknown asker and scope-builder error are denied on all five
   routes.
3. Org scopes: a named asker with no member scopes sees `jira`, `jira:PAY`,
   `github:wego/payments`, `confluence:X` nodes, and a Jira attachment whose parent is
   `jira:PAY`; does not see `gws`, `jiraevil:X`, `slack:C...` (non-member),
   `slack:D...`, or a Slack attachment whose only parent is hidden. Deleted parent and
   multi-parent attachment cases keep today's behaviour. Go and SQL predicates agree on
   the scope table.
4. Admin-only GETs: the route-walk test from Routes; for each listed route 403
   without the header and not 403 with it (`/traces/stats` with a recorder mounted).
5. MCP: asker header sent from env on the five tools; missing / empty / whitespace
   env gives the tool error with no HTTP request; the two removed tools are gone;
   `Probe` still works without the env.
6. Traces: admin and rejected modes recorded, rejected rows have no items,
   `body_asker_slack_id` recorded.
7. Eval: captured-request tests, separately for `relevance` and `prepare` and for
   each variant: admin header present only on `resolve-admin`, absent on
   `resolve-asker` and `search-hybrid`, which carry the asker; `label-sheet` reads the
   renamed saved replay keys with no HTTP call; `eval relevance --dry-run` over all 20 cases shows the new set; case
   labels unchanged byte for byte.
8. Dashboard: `authFetch` sets `X-Graph-Admin: 1` and a caller's own headers cannot
   drop it; embedded build rebuilt.
9. `make test-db` green except the known flaky `TestPeriodicTickerReclaimInterleavings`
   and `TestReindexHeuristicCLI_RateAndProgress` (SGT clock); `go vet ./...` clean;
   dashboard build green.
10. Branch pushed; no merge, no deploy, no hub runs, no `~/.claude.json` edit in this
    dispatch.

## Verify after merge (conductor, Enzo's go)

1. Deploy per the repo's checked-in `CLAUDE.md` (hub Mac mini,
   `docker compose up -d --build worker`; not the obsolete VPS target); confirm build
   rev.
2. On the hub: no-asker `/search` returns 403; admin header works; dashboard search,
   resolve, node and neighbors pages render.
3. `eval prepare --refresh` then `eval relevance`; paste a before / after table into
   the parent plan's Results. Gate: probes A, C, E get their Jira / PR seeds back for
   the real asker; UC10 returns nothing from the hidden channel; 0 `asker_unknown`
   for the 20 case askers.
4. Update the payments-project MCP entry to pass `-e AGENT_MEM_ASKER` (Enzo's OK),
   then one MCP search from a session with and without the env set.

## Risks

- p-agent's Slack identity resolver also calls `/slack-user`
  (`p-agent/src/channels/slack/socket.js:2721-2743`) and falls back to Slack
  `users.info`. After deploy, department and canonical-name enrichment degrade for
  all bot turns until p-agent changes. Messaging keeps working. The endpoint stays
  admin-only.

- p-agent's owner-only Mem search Home tab calls `/search`, `/resolve`, `/node`,
  `/neighbors` and `/slack-user` with no asker, so it returns 403 after deploy until a
  small p-agent change sends the owner's Slack id (Open question O1).

- Every MCP graph call fails until the caller sets `AGENT_MEM_ASKER`. That includes
  EnzoBot panes in the payments repo and Enzo's own sessions. Intended (D3, fails
  closed). Enzo sets his own value; bot panes get the requester's id in the later
  p-agent plan.
- If `AGENT_MEM_ASKER` is set in a shell profile, bot panes inherit Enzo's identity.
  Still narrower than today's unfiltered access; the p-agent plan must override it
  per pane.
- The 78 people without an eeid get 403 everywhere. Accepted: no context beats
  unfiltered context.
- Anyone with the API key can send the admin header (D3 accepts this).

## Open questions for Enzo

- O1. Mem search Home tab breaks after deploy. Recommended: accept, and right after
  this deploy run one small p-agent round that sends `X-Asker-User: <owner Slack id>`
  on the four filtered calls (filtered as you, not admin) and drops its `/slack-user`
  lookup, which becomes admin-only. Alternative: deploy this only after that p-agent round lands.

## Findings

Worker: acl-worker, 2026-10-11, branch `feat/graph-context-phase1-acl` (base `483d03d`).

**What changed**

- `internal/graph/handlers/acl_guard.go`: one resolver, `resolveAsker` (steps 1-8 of the
  table), wrapped by `requireAsker` (writes the JSON error `{"error": code}`, sets the
  trace `acl_mode`). `askerScopeSet`, `askerScopeSetDiag` and `lookupAsker` are deleted.
  Org scopes: `orgScopeSources` (`jira`, `github`, `confluence`), `orgScope` (Go) and
  `orgScopeSQL` (`split_part(scope, ':', 1) IN (...)`) feed `scopeVisible`,
  `aclVisibleSQL` and `parentVisibleSQL`. Merge chains are followed in Go, at most 3 hops.
- `search.go`, `resolve.go`, `node.go`, `neighbors.go`, `epic.go`: guard runs first in
  each handler. `Resolve`, `Node`, `Epic` and the neighbors handler now hold a
  `scopeBuilder` interface (as `Search` already did) so tests can inject a failing
  builder. `/resolve` takes `asker_slack_id`; its scope-builder error is no longer ignored.
- `router.go`: `Mount` wraps the routes in a group with `graphReadPolicy`. A GET or HEAD
  under `/api/graph/` that `askerFilteredGET` does not list needs `X-Graph-Admin: 1`.
  Matching uses `RawPath` when set, the same string chi routes on, so `%3A` / `%2F` in
  node ids cannot slip past it. `cluster_summary.go`: the old per-asker 403 is gone (the
  route is admin-only).
- `context_trace.go`: `body_asker_slack_id` in `asker_input`; `acl_mode` values `admin`
  and `rejected` (rejected rows have no items).
- `internal/graphmcp`: `Client.SetAsker`, `ErrAskerRequired`, per-request headers in
  `doJSON`; `Search`/`Node`/`Neighbors`/`Resolve`/`Epic` check the asker before any HTTP.
  `ClusterSummary` and `Person` removed from client, interface and tool list.
  `cmd/agent-mem/mcp.go` reads `AGENT_MEM_ASKER`. `Probe` is untouched.
- Eval: variant `resolve-asker0` renamed `resolve-admin` in code, the 20 case YAMLs and
  `replay.json`; `evalVariantHeaders` / `evalRequestHeaders` give each variant its
  headers, used by both `evalClient.do` and the dry-run print.
- Dashboard: `authFetch` (now exported) sets `X-Graph-Admin: 1` last, over a
  `Headers` copy, so a caller's headers cannot drop it. Embed rebuilt
  (`index-CAhXUx9s.js`).
- README (asker contract, MCP tools and env) and CLAUDE.md (contract summary).
  `~/.claude.json` not touched: the payments MCP entry still needs
  `docker exec -i -e AGENT_MEM_ASKER=<slack id> ...` (conductor, after deploy).

**Facts that differed from the plan, and things the plan did not list**

1. `internal/context/epic_brief.go` (session-start hub fallback on every laptop) calls
   `GET /api/graph/epic/<issue>` with the API key and no asker. After deploy it gets
   `403 asker_required`, so the epic brief silently disappears from session context.
   NOT changed: it needs a decision (send an asker, or send the admin header from every
   laptop). Its test still expects no `X-Asker-User`.
2. `cmd/agent-mem/trace.go` (`agent-mem trace stats --base-url`) calls
   `/api/graph/traces/stats`, now admin-only. One line: it sends `X-Graph-Admin: 1`
   (without it the writer-counters line degrades to "unavailable (HTTP 403)").
   `trace_test.go` now fails the fake server without the header.
3. `/resolve`'s response still echoes the requested seeds and reports
   `graph_trace.expanded_nodes` (count before the ACL filter). Not changed; a hidden
   seed returns `artifacts: []` but the count is aggregate metadata.
4. Behaviour changes in existing tests, all intended: an unknown asker used to get
   public-only results and now gets 403; a resolved asker with no memberships now sees
   Jira-parented attachments (org rule); a PR with the bare scope `github` is visible
   to everyone (fixture in `jira_prs_test.go` now uses `gws`, which stays hidden);
   `search-unknown-asker` compat golden removed (the 403 body is covered by the new
   tests). `attachment_derived_content_test` re-parents its attachment to a hidden
   thread, because a resolved asker legitimately sees attachments of a visible parent.
5. Every admin read logs one line (`graph: admin read route=... caller=...`), as the
   plan says. The dashboard sends the admin header on every call, so the hub log gets
   one line per dashboard request.
6. Unknown `GET` paths under `/api/graph/` (typos, removed routes) now answer 403
   `admin_required` instead of 404 unless the admin header is sent. Fail-closed side
   effect of "admin by default".
7. A merged person row whose `merged_into` target is missing cannot exist under the FK;
   the test creates it with `session_replication_role = replica` in one transaction.

**Tests**

- New: `acl_resolver_test.go` (steps table on `/search` and `/resolve`, body identities,
  merge chain 1 / 3 / 4 hops, loop, missing target, DB error 503, unknown asker and
  failing scope builder on all five routes), `acl_routes_test.go` (hidden root /
  reply / attachment across the five routes for non-member, member and admin; org scopes;
  Go vs SQL predicate parity; route walk over the mounted router, 30 GET routes; trace
  modes), `graphmcp` client and server tests, `cmd/agent-mem/eval_acl_test.go`
  (captured headers for `relevance` and `prepare`, `label-sheet` with the server down,
  dry run over all 20 cases, case files equal to base outside `requests:`),
  `dashboard/scripts/authFetch.test.ts` (`node --test`, 3 pass).
- Existing tests: read-route tests now send the admin header through `adminRequest`;
  tests that exercise filtering name a real person.
- `make test-db` (throwaway pgvector Postgres): exit 0, no failures. The two known flaky
  tests (`TestPeriodicTickerReclaimInterleavings`, `TestReindexHeuristicCLI_RateAndProgress`)
  passed in this run.
- `go vet ./...` clean; `gofmt -l` clean on every file touched. Dashboard `npm run build`
  green (`tsc -b && vite build`), embed synced.
- Outside `make test-db`: `go test ./...` without a DB shows failures in
  `internal/hooks` (1) and `internal/skills` (3); the same tests fail on base `483d03d`,
  unrelated to this change. `internal/context` passes with a DB.
- Not run: anything against the hub, a deploy, a live MCP session.
