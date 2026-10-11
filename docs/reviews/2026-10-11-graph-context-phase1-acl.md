# Phase 1 ACL plan review

Verdict: **CHANGES REQUIRED**

Reviewed the draft against repository HEAD `8b10d8e`, with the parent plan as context. D1 and D3 are fixed and are not reopened here. Code and plans were not modified. This is a static plan/contract review, not verification of an implementation or production database. No deployment, hub call, or tests were run.

## Blocking findings

### 1. HIGH — The read-route inventory misses subscriptions and jobs

**Verified.** The plan's Routes section omits two mounted GET endpoints:

- `GET /api/graph/subscriptions`, mounted at `internal/graph/handlers/router.go:91`, returns every subscription without asker filtering. `detect_hot_topics.go:784-824` selects and serializes subscriber Slack identity, topic, channel filters, source references, scope definition, scope summary and errors. This is content and identity data, not a content-free configuration response.
- `GET /api/graph/jobs`, mounted at `router.go:21`, returns raw job payloads and last errors (`jobs_admin.go:24-32,35-90`). It is not merely queue statistics. The plan does not authorize or sanitize this output.

Following the listed route changes literally leaves unnamed callers able to read these responses without the admin header, contradicting D3 and the stated goal. Require admin on both, and enumerate literal method/path pairs rather than wildcard route families. Add mounted-router tests so an omitted route cannot pass merely because all selected handlers were tested.

### 2. HIGH — “Attachment rules unchanged” leaves Jira attachments inaccessible under D1

**Verified.** `acl_guard.go:84-90` embeds the old exact-membership predicate inside `parentVisibleSQL`. Both `nodeVisible` and SQL-based attachment filtering depend on that helper (`acl_guard.go:93-117`). The plan explicitly changes `scopeVisible` and `aclVisibleSQL`, while calling attachment rules unchanged.

For an employee with no member scopes, a parent scoped `jira:PAY` becomes readable under the new org rule, but its attachment still fails the unchanged parent's exact-membership test. Changing only the outer predicate cannot fix that. This contradicts the plan's own explanation, “an attachment is visible only when its parent is,” and the intended org-wide Jira access.

Keep the parent-gated attachment policy, but explicitly reuse the new org-aware scope predicate inside `parentVisibleSQL`. Test a Jira attachment whose parent becomes visible through D1 and a Slack attachment whose only parent is hidden, through both SQL-backed reads and `/node`. Include deleted parents and attachments with multiple parents to preserve current semantics.

### 3. MEDIUM — Resolver precedence and error handling are incomplete

**Verified specification gap.** The priority table and the later conflict rule do not fully define these cases:

- Both body identities are supplied and resolve to different people, without a header.
- A valid header accompanies an unknown body identity, or an explicit body `asker_eeid: 0`.
- Admin is supplied alongside conflicting or unknown identities: does admin short-circuit identity resolution and all conflict checks?
- Person lookup has an operational DB error: the table specifies 503 for a scope-builder failure, but not for the preceding lookup. The current lookup preserves DB errors (`search.go:761-766`).
- Negative or overflowing numeric identities; a one-hop merge target that is itself merged, absent, or has no eeid.

“Inputs, in priority order” is not sufficient because the same section overrides priority with mandatory header/body agreement. Define which values count as asserted identities, whether all asserted identities must agree after canonicalization, and explicit error precedence. Do not silently discard a contradictory lower-priority identity. Test those combinations, whitespace-only input, and admin values other than exactly `1`. Specify whether conflict responses are traced as rejected too; currently only 403/503 are mentioned.

### 4. MEDIUM — Two MCP tools will remain unusable even with AGENT_MEM_ASKER

**Verified.** MCP exposes `graph_cluster_summary` and `graph_person` (`internal/graphmcp/server.go:127,187`). Their client methods call `/cluster/summary` and `/person` (`client.go:129-143`). This plan makes both endpoints admin-only while requiring MCP never to send admin.

Thus setting the env variable restores the filtered search/node/neighbors/epic/resolve tools, but cannot restore these two tools. The Risks section describes MCP failure as lasting “until” the env is set, which is materially incomplete.

Preserve D3. Explicitly document these tools as unavailable in this phase and remove/disable them with a clear tool error, or separately specify safe filtered implementations. Add tests for their chosen behavior with a valid asker. Also decide whether missing-env validation applies to `Client.Probe`, which calls `/api/settings` through the same `doJSON` path (`client.go:152-154`); startup invokes it at `cmd/agent-mem/mcp.go:70`. A guard added indiscriminately to `doJSON` can produce a startup failure rather than the promised per-tool error.

### 5. MEDIUM — Acceptance gates do not cover the likely security regressions

**Verified.** Acceptance 1 tests no-asker/admin on node/neighbors/epic, but does not require a *named* non-member to be denied hidden content on those routes. Acceptance 2 tests scope predicates, not all returned fields and route branches. Acceptance 3 only covers the selected admin-only routes, so finding 1 could survive all acceptance tests.

Require mounted-router fixtures for hidden Slack roots/replies/attachments across `/search` (legacy and hybrid, including linked results), `/resolve`, `/node`, neighbors both ordinary/cards, and `/epic`. Assert hidden node IDs, titles, bodies and relationship metadata are absent, not just that status is 200. Require unknown-asker and membership-error denial on all filtered routes, plus org bare scopes and attachment tests. Include a regression assertion that no non-admin named call receives the unfiltered response.

## Facts audit

Each bullet below corresponds to a Facts bullet in the plan.

1. **Confirmed:** empty trimmed header is unfiltered; lookup failure/unknown/zero eeid becomes public-only (`acl_guard.go:26-42`). NULL is coalesced to zero in `search.go:758`.
2. **Confirmed with wording correction:** lookup matches four external identity columns, has no merged filter and no numeric-eeid branch (`search.go:750-768`). “A numeric eeid matches nothing” is too absolute: a numeric string can match one of the external identity columns. Say that it is not interpreted as an eeid.
3. **Confirmed:** body only has `asker_eeid` (`resolve.go:29`); builder error ignored at 208; zero is unfiltered at 297; nonzero is marked resolved at 305 without validating a person.
4. **Confirmed:** header paths at `search.go:161`, `node.go:104`, `neighbors.go:148`, `epic.go:152`; cluster rejects a nonempty trimmed asker at `cluster_summary.go:124`.
5. **Incorrect/incomplete:** `/slack-users` is not a full people dump with email and department. It returns only the Slack ID-to-display-name map (`slack_users_api.go:8-28`). `/slack-user` is the richer profile (`slack_user_api.go:14-21,43-56`). Subscriptions and jobs are missing from the no-asker inventory, as above.
6. **Confirmed for ordinary nodes; qualify attachments:** exact-scope predicates are at `acl_guard.go:51-58,84-100`. `acl/scopes.go:64-93` reads `member_scopes`; production Go writer found is the Slack refresh at `refresh_slack_members.go:245`. This supports the described present-day absence of automatic Jira/GitHub/Confluence grants, not a proof about current live DB contents or manually inserted memberships.
7. **Confirmed but not exhaustive:** `fetch_body.go:369-396` emits namespaced/bare source scopes and public for Wego Hub/Claude artifacts. `ingest_content.go:640-643` first honors an explicit metadata scope, so ingestion permits other strings too. The proposed exact source-prefix rule correctly covers both `jira` and `jira:PAY`, and likewise GitHub/Confluence. It must not use a loose prefix such as `LIKE 'jira%'`. GWS stays denied absent explicit membership. Test bare scopes, near-matches such as `jiraevil:X`, empty/NULL/public, and GWS.
8. **Qualified:** shared Bearer check exists, but authentication is bypassed when APIKey is empty or SyncURL is set (`internal/worker/sync_handlers.go:344-354`). `X-Graph-Caller` records provenance, not authority (`context_trace.go:82`). Do not describe the key as universally enforced in all operating modes.
9. **Code confirmed:** MCP sends Bearer and caller, no asker (`graphmcp/client.go:181-187`). Docker env forwarding described in the plan is sound. The claimed payments entry in `~/.claude.json` was not independently read; it is an operator fact, not established from this repository.
10. **Confirmed:** graph dashboard requests use `authFetch`, whose existing implementation adds the Bearer header (`dashboard/src/api.ts:11-19`). Searches across dashboard sources found no alternate direct graph fetch. Adding admin there covers its graph requests, including subscriptions/jobs. Embedded assets still require rebuilding.
11. **Confirmed with citation precision:** `context_trace.go:285-289` reads a diagnostic string and falls back to not-evaluated; assignments of current modes are in `search.go:165-171` and `resolve.go:297-303`. Only search and resolve are trace-wrapped (`router.go:40-44`). `migrations/20261011000001_context_traces.sql:11` is unrestricted TEXT, so adding admin/rejected requires no enum migration.

## Complete graph read-route inventory

All paths below have prefix `/api/graph`; inventory source is `internal/graph/handlers/router.go:21-105` and the neighbors subrouter.

| Routes | Plan classification / required clarification |
|---|---|
| GET `/search`, `/node`, `/node/{id}/neighbors`, `/epic/{key}`; POST `/resolve` | Filtered resolver, explicit admin bypass. Neighbors cards is a query branch, not another route. |
| GET `/person`, `/slack-user`, `/slack-users` | Admin-only, explicitly listed. |
| GET `/channels`, `/channels/recent`, `/channel`, `/channel/topics` | Admin-only; expand `/channels*` into these literal entries. |
| GET `/cluster/summary`, `/pins`, `/pins/board`, `/node/{id}/subject-queries` | Admin-only, explicitly listed. |
| GET `/traces/stats` | Admin-only; conditionally mounted when a recorder exists. Test with that dependency enabled. |
| GET `/subscriptions`, `/jobs` | Missing; must be admin-only or explicitly sanitized/filtered. |
| GET `/topic-rules`, `/continents`, `/channel-filters`, `/eligibility-gate`, `/business-root`, `/temporal-timezone`, `/boost-alphas`, `/epic-briefs`, `/purpose`, `/slack-members`, `/jira-updates`, `/gh-key-search` | Currently deferred to implementation judgment as “config GETs”; no finalized per-route behavior. |

The config exception also needs an explicit boundary. In particular `/continents` is not devoid of channel metadata: its default includes channel IDs and names, including `enzo-private`, and a Slack group-name map (`channels.go:20-44`), and its handler returns raw stored JSON (`channels.go:673-700`). Calling all config GETs content-free is not justified. Either admin-gate channel/identity-bearing configuration or expressly document the permitted metadata exposure. More generally, “every graph read”/“a call with no asker gets an error” conflicts with the intended unnamed configuration exception unless the goal/contract states that exception precisely.

Write-only route authorization is outside this read review. Wildcards such as `/pins*` should not silently specify write behavior; record methods explicitly. The legacy non-graph API also exists and is not covered by this graph-only contract.

## Caller and eval implementation requirements

- **Dashboard:** the planned central header change is sufficient for the graph fetch paths found. No reason to add per-call fallbacks or invent an asker. Ensure the new header cannot be accidentally removed by the existing `RequestInit.headers` merge. Verify the embedded build, not just the source build.
- **MCP:** fail-closed env behavior is consistent with D3. Test missing, empty and whitespace-only env, ensure no graph HTTP request occurs, and never use admin as a recovery path. Address the two admin-only tools and startup probe described above.
- **Eval:** the case YAMLs are executable request definitions, not just labels. For example `testdata/eval/graph_context/cases/case-01.yaml:9-11` still names `resolve-asker0` with empty headers. `loadEvalCases` loads YAML directly (`eval_common.go:42-67`); `buildEvalRequest` requires the requested variant key (`:79-83`). Updating only `evalVariants` and the generator breaks existing files. Explicitly update all 20 checked-in case requests, or define backward-compatible load-time normalization while preserving labels.
- **Eval headers:** `evalClient.do` currently accepts no per-request headers (`eval_common.go:163-175`); request construction does not return them, and dry-run printing hardcodes the header text (`eval_relevance.go:169-173`). Carry effective headers through the real HTTP path as well as dry-run. A changed dry-run string alone proves nothing. Test captured requests for resolve-admin, real resolve, filtered search, prepare, and label-sheet, including the absence of accidental admin on real-asker variants. Existing prepare helpers also refer to the old variant (`eval_prepare.go:80,372`).
- **Traces:** put rejection guards inside the tracing wrapper, or otherwise ensure rejections are recorded. Extend asker-input recording to the new body Slack ID; currently it records only header identity and body eeid (`context_trace.go:121-133`). Tests should assert admin/rejected modes and empty items on rejection rather than only HTTP status.

## Non-blocking operational clarification

The plan's hub deployment instruction agrees with checked-in `CLAUDE.md:63-80` (Mac mini; do not use the obsolete VPS target). The supplied project instructions instead name the VPS and `make deploy`. No deployment was attempted. Resolve this guidance conflict before the conductor's deployment step rather than selecting a target from memory.

## Required revision summary

Add the omitted reads and a finalized method/path policy; apply the org predicate inside parent visibility; finish the multi-identity/error contract; acknowledge and handle the MCP tool incompatibility; and strengthen acceptance tests to assert real filtering across the mounted routes and effective eval HTTP headers. None of these changes requires reconsidering D1 or D3.
