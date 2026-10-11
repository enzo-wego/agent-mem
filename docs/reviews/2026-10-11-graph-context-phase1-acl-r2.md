# Phase 1 ACL plan review — round 2

Verdict: **CHANGES REQUIRED**

Reviewed the r2 plan against repository HEAD `8b10d8e`, the parent Phase 1 plan, and this reviewer's round-1 report. D1 and D3 remain fixed. All five numbered round-1 blockers are resolved in the specification. One new eval-contract inconsistency needs correction before implementation.

This was a static plan review, not an implementation approval. No code or plan was edited; no tests, deployment, production queries, or hub calls were run. Only this requested report was written. Graph tooling and implementation build/test passes are not applicable to this plan-only delta; review was inline using repository reads and call-site searches.

## Finding R2-1 — MEDIUM, verified: blanket admin on prepare destroys the real-asker replay distinction

**Plan:** `docs/ai/2026-10-11-graph-context-phase1-acl.md:169-177,218-221`.

The caller section says that `prepare` and `label-sheet` replays use the admin header. Acceptance 7 simultaneously requires no admin header on real-asker variants. Those instructions cannot both hold for the existing prepare path:

- `runEvalPrepare` loops over **every** entry of `evalVariants`, builds its request, sends it over HTTP, and saves the returned IDs under that variant's name (`cmd/agent-mem/eval_prepare.go:123-145`).
- The variants include real-asker resolve and hybrid search (`eval_common.go:24`), not just the old asker-zero comparison arm.
- Under the new resolver's first-match rule, an admin header bypasses the asserted identity. Adding it globally during prepare therefore records unfiltered results under `resolve-asker` and `search-hybrid`. These are no longer faithful replays of the corresponding relevance requests.
- `runLabelSheet` does **not** replay HTTP requests. It reads `replay.json`, verifies its case-file hash, takes the saved variant IDs, and reads their content through a read-only DB transaction (`eval_prepare.go:293-306,322-355`). There is no label-sheet HTTP header path to test. Its current saved-replay behavior is intentional and covered by the existing test at `eval_harness_test.go:544` onward.

**Required correction:** use the same per-variant effective headers in relevance and prepare: admin only for `resolve-admin`; a real identity and no admin for `resolve-asker` and `search-hybrid`. Keep label-sheet as a saved-replay/DB reader, with no new HTTP replay. Update acceptance 7 to capture prepare requests separately for each variant, assert the absence of admin on its real-asker requests, and test label-sheet against the renamed saved replay keys. If a separate admin-only candidate-pool replay is intended instead, give it a distinct name and specify it separately; do not silently relabel it as a real-asker replay.

This finding is introduced by r2's newly explicit caller/header instructions. It does not reopen D1 or D3: the admin comparison arm remains allowed and useful. The issue is the integrity of the evaluation evidence used to judge the ACL change.

## Round-1 disposition

| Round-1 item | Status in r2 |
|---|---|
| 1. Missing subscriptions/jobs and unresolved configuration-read policy | **Resolved.** Both appear in the literal route list. All other non-filtered GETs, including metadata-bearing configuration, are explicitly admin-only. Default-admin middleware and a mounted-route inventory test replace implementation discretion. |
| 2. Jira attachments still gated by the old parent predicate | **Resolved.** The org predicate explicitly applies inside `parentVisibleSQL` as well as the outer SQL and Go checks. Acceptance includes Jira/Slack attachments, deleted parents and multiple parents. |
| 3. Identity precedence and failure handling | **Resolved.** Asserted inputs, zero/empty absence, admin short-circuit, invalid numeric input, lookup errors, unknown identities, conflicts, merge traversal and membership errors now have ordered outcomes. Rejected traces include 400 responses. |
| 4. MCP tools incompatible with admin-only endpoints; Probe ambiguity | **Resolved.** The two incompatible tools are removed. Env validation belongs in the five graph methods, not the shared HTTP transport, so Probe remains usable. |
| 5. Tests fail to exercise actual hidden-content filtering | **Resolved.** Mounted-route fixtures now cover all five filtered routes, search branches/linked rows, neighbors cards, hidden roots/replies/attachments, response metadata, unknown askers and scope errors. |

The supporting round-1 requests concerning numeric-eeid wording, Slack profile versus name-map distinction, auth bypass modes, trace placement/new body identity, checked-in YAML migration and dashboard header precedence are also addressed. The eval transport requirement is now explicit, but its new prepare/label-sheet wording creates R2-1 above.

## Read-route coverage and unnamed callers

The literal route table matches the current graph router: **30 GET routes**, comprising four filtered GETs and 26 admin-only GETs, plus filtered `POST /resolve`.

- Filtered GETs: `/search`, `/node`, `/epic/{key}` and `/node/{id}/neighbors`. The first three are mounted in `internal/graph/handlers/router.go:34-50`; neighbors is mounted through `router.go:103` and `neighbors.go:56-60`.
- The 26 admin-only GETs are all accounted for in `router.go:21-102`: jobs, topic rules, person, traces/stats, both Slack-user routes, channels, channels/recent, channel, channel/topics, cluster/summary, continents, channel-filters, eligibility-gate, business-root, temporal-timezone, boost-alphas, epic-briefs, purpose, slack-members, jira-updates, gh-key-search, subscriptions, pins, pins/board and node subject-queries.
- `/traces/stats` is conditional on a recorder (`router.go:45-47`); the acceptance text correctly requires mounting one.
- `cards=1` is a neighbors response branch, not an extra route (`neighbors.go:523,558`). It is explicitly covered.
- `POST /resolve` is the read-like POST and is explicitly classified (`router.go:43-44`). Other mutation routes remain outside the stated read-only scope; the existing subscription PATCH is likewise a mutation, not an omitted read endpoint.

I found no omitted current graph read route that would retain unnamed unfiltered access if the proposed policy is implemented. The default-admin rule also closes the round-1 future-route omission risk. Its implementation must encompass both the directly registered routes and the mounted neighbors subrouter; a guard only inside `NewNeighbors` would not cover the other routes. The planned mounted-route tests are the appropriate place to verify this.

This is deliberately a graph-read contract, not a claim to secure every worker API or direct DB access. Existing non-graph settings/sync APIs and writes are not brought under asker authorization by this plan. That is consistent with D3's stated protection against caller mistakes rather than a hostile key-bearing process.

## Facts audit against code

1. **Shared helper — confirmed.** `acl_guard.go:26-42` treats a trimmed-empty header as unfiltered; unknown/zero-eeid and lookup failure become public-only. Scope-build failure returns the public set plus the error.
2. **lookupAsker — confirmed.** `search.go:750-768` matches the four external identity columns, has no merge filter or numeric-eeid branch, coalesces NULL to zero and preserves operational errors. The revised numeric-string qualification is accurate.
3. **Resolve — confirmed.** The request has body `asker_eeid` (`resolve.go:29`); builder failure is ignored at 208; zero selects unfiltered at 297; nonzero is treated as resolved at 305 without person validation.
4. **Existing header handling — confirmed.** The listed call sites are `search.go:161`, `node.go:104`, `neighbors.go:148`, and `epic.go:152`. Cluster summary rejects a trimmed-nonempty header at `cluster_summary.go:124`.
5. **No-asker GETs and their data — confirmed.** The router inventory above is complete. The richer Slack profile is at `slack_user_api.go:14-21,43-56`; the name map at `slack_users_api.go:8-28`; subscriptions at `detect_hot_topics.go:784-824`; job payloads/errors at `jobs_admin.go:24-90`; continents channel/group metadata at `channels.go:20-44,673-692`. These are appropriately admin-gated in r2.
6. **Visibility and memberships — confirmed as repository behavior, not live DB state.** The exact-membership predicates are at `acl_guard.go:51-58,84-117`. `acl/scopes.go:37-54,64-93` builds/caches membership-table grants. The production Go writer found is Slack refresh (`refresh_slack_members.go:245`). This proves no automatic org grants through that writer, not the absence of manually inserted Jira/GitHub/Confluence memberships in production.
7. **Scope strings — confirmed for derived scopes, not an exhaustive allowed-value set.** `fetch_body.go:369-396` emits the stated namespaced or bare sources and public scopes. `ingest_content.go:640-662` additionally honors arbitrary nonempty `metadata.scope` before deriving a scope. The exact-prefix proposal handles both bare and namespaced org scopes without admitting near-matches.
8. **Auth — confirmed with operator-state qualification.** Shared Bearer enforcement and the empty-key/sync-client exemptions are at `internal/worker/sync_handlers.go:334-354`. Caller provenance is recorded at `context_trace.go:82`; it grants no authority. The claim that the hub has a key was not independently verified against production.
9. **MCP — confirmed in repository code.** Bearer/caller headers and no asker are at `graphmcp/client.go:181-187`; seven tools are in `server.go:76-220` (the final epic registration extends beyond the plan's cited 206). The two admin-only client methods are at `client.go:129-143`. Probe calls settings at 152-154; startup invokes it at `cmd/agent-mem/mcp.go:70`. The user's `~/.claude.json` entry was not read; Docker env forwarding is an operator requirement, not verified repo state.
10. **Dashboard — confirmed.** `dashboard/src/api.ts:11-19` adds Bearer but no asker. Source searches found graph fetches using that wrapper; the other direct fetches are non-graph stats/health and a static asset. Central admin injection covers graph calls. The acceptance clause preventing caller headers from removing admin correctly addresses the existing merge order.
11. **Traces — confirmed.** Current modes are assigned at `search.go:165-171` and `resolve.go:297-303`; fallback is at `context_trace.go:285-289`; asker capture at 121-133. Only search/resolve are wrapped (`router.go:40-44`). `migrations/20261011000001_context_traces.sql:11` uses unrestricted TEXT. No mode migration is needed.
12. **Eval facts — confirmed.** The checked-in variant keys/body identities are visible at `testdata/eval/graph_context/cases/case-01.yaml:9-11`; headerless transport at `eval_common.go:163-175`; hard-coded dry-run headers at `eval_relevance.go:169-173`; old-key seed/query lookup at `eval_prepare.go:80,372`. The new proposed behavior, rather than these Facts, contains R2-1.
13. **p-agent client routes — confirmed by read-only inspection of the sibling checkout.** `p-agent/src/graph-context/client.js:34-76` wraps the listed routes, and its transport at 79-116 lacks admin and only optionally adds an asker for selected GETs. Current deployment flags were not checked.

## Resolver, D1 and caller assessment

The new ordered resolver contract closes the old empty-header/asker-zero fallback. Valid identity plus unknown identity is denied; identities resolving to distinct canonical eeids conflict; admin explicitly bypasses both. Following `merged_into` is compatible with the schema: it references `graph.people.id`, not eeid (`migrations/20260527000001_graph_schema.sql:15,27`). Implement that traversal by row ID. The three-hop cap is fail-closed.

The org-source rule correctly admits bare `jira`, `github`, `confluence` as well as their colon-qualified scopes. Exact prefix extraction excludes `jiraevil:X` and `jira-x`; GWS and Slack gain no org-wide exception. Applying it inside the attachment parent's predicate preserves parent gating while fixing Jira attachment visibility. No D1/D3 contradiction was found in these rules.

Dashboard admin access and the five remaining MCP tools have viable caller paths. MCP removal and fail-closed env behavior are explicit compatibility changes, not accidental failures. The documented `docker exec -e AGENT_MEM_ASKER` forwarding is necessary; the plan correctly does not edit the user's configuration automatically. Missing-env validation must leave Probe alone, as acceptance 5 requires.

O1 now openly presents the Home-tab outage as an operator decision; accepting it versus sequencing the p-agent change first is not a reviewer decision. Its proposed filtered-owner identity is consistent with D3. One additional non-blocking compatibility note: the sibling p-agent's ordinary Slack identity resolver also calls `/slack-user`, outside context injection (`p-agent/src/channels/slack/socket.js:2721-2743`). It falls back to Slack `users.info` at 2744 onward, so messaging need not stop, but department/canonical-name enrichment degrades even while graph context injection is disabled. Include that in the operator handoff; do not weaken the endpoint policy to preserve it.

## Acceptance and handoff

The expanded mounted-route, attachment, error, trace, MCP and dashboard tests now target the security regressions identified in round 1. In particular, testing actual hidden response fields rather than only HTTP success is the right gate. Effective eval headers must be tested on actual captured requests, separately for each variant in both callers, as described in R2-1.

The stated quality gates are future implementation requirements; this review does not certify them as passing. Likewise, the listed flaky tests and production population counts were not re-measured. The checked-in Mac-mini deploy guidance remains inconsistent with the supplied older VPS instructions; no deployment was attempted, and the conductor must resolve that operational instruction conflict before shipping.

**Required next revision:** correct the prepare/label-sheet contract and its acceptance test. No further changes to the D1/D3 policy, route inventory, attachment rule or MCP tool decision are requested.
