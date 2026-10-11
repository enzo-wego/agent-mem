# Phase 1 ACL plan review — round 3

Verdict: **APPROVED**

R2-1 is resolved. No new blocking finding or regression in the round-1 fixes was found. Approval is for the implementation plan, not an implementation, deployment, or measured production ACL result. D1 and D3 remain fixed.

## Scope and evidence

Reviewed the r3 plan, parent Phase 1, both prior reviews, current repository code and the sibling p-agent caller code. Current HEAD is `63c88db`, not the Facts section's `8b10d8e`. The intervening diff changes only `internal/worker/handlers.go`, `project.go`, `project_test.go`, and `server.go`; the graph handlers, MCP and eval code cited here are unchanged. No code or plan was edited. No tests, production queries, hub requests, deployment, or user configuration changes were performed. Only this requested report was written.

## R2-1 disposition: resolved

The revised Callers section now requires the same effective per-variant headers for both relevance and prepare: admin only on `resolve-admin`, a real identity and no admin on `resolve-asker` and `search-hybrid`. This matches the existing architecture:

- `cmd/agent-mem/eval_common.go:24,79-100,163-175` defines three variants, builds each request, and currently sends it through a transport without per-request headers. The plan explicitly changes that transport and carries the effective headers through to dry-run output, rather than merely changing the printed text.
- `cmd/agent-mem/eval_prepare.go:123-145` iterates every variant and saves IDs under its name. Per-variant headers preserve the distinction between the unfiltered comparison and real-asker replays.
- `eval_prepare.go:293-306,322-355` reads saved replay data, checks the case-file hash, opens a read-only DB transaction and reads saved candidate IDs for label sheets. The revised plan correctly leaves this as a non-HTTP path.
- Acceptance 7 now separately captures relevance and prepare requests for every variant, tests both presence and absence of admin, and checks label-sheet consumption of renamed saved keys without HTTP.

This closes the precise round-2 failure: unfiltered results can no longer be intentionally recorded under a real-asker variant by a blanket prepare admin header. Implementation must also preserve the existing replay/case hash consistency when renaming checked-in requests; the planned label-sheet test should exercise its real reader, including that check.

## Facts audit

1. **Shared helper confirmed:** `internal/graph/handlers/acl_guard.go:26-42` treats a trimmed-empty header as unfiltered and unknown/zero-eeid/lookup failure as public-only. A membership-builder failure returns a public set and an error.
2. **Identity lookup confirmed:** `search.go:750-768` matches Slack ID, email, GitHub login and Jira account ID, with no merge filter or numeric-eeid interpretation; NULL eeid becomes zero and operational errors propagate. The plan correctly qualifies the possible external-column match of a numeric string.
3. **Resolve confirmed:** `resolve.go:29` has only body eeid, `:208` ignores the builder error, and `:297-305` treats zero as unfiltered and any nonzero as resolved without a person lookup.
4. **Existing header routes confirmed:** `search.go:161`, `node.go:104`, `neighbors.go:148`, `epic.go:152`; `cluster_summary.go:124` rejects trimmed-nonempty asker headers.
5. **Other GETs confirmed:** inventory is `router.go:21-103` plus `neighbors.go:56-60`. Profile fields are at `slack_user_api.go:14-21,43-56`; name-map response at `slack_users_api.go:8-28`; subscription identities/topics/source refs at `detect_hot_topics.go:784-824`; job payloads/errors at `jobs_admin.go:24-90`; channel metadata and raw stored continents at `channels.go:20-44,673-700`. All are covered by the proposed policy.
6. **Visibility and membership confirmed as code behavior:** `acl_guard.go:51-58,84-117` implements public/unscoped/exact-membership checks and attachment parent gating. `acl/scopes.go:37-93` reads/caches member grants; the production graph writer found is Slack refresh at `refresh_slack_members.go:245`. This establishes the absence of automatic Jira/GitHub/Confluence grants through that writer, not the absence of manually inserted memberships in a live DB. The Facts sentence saying no filtered asker sees any such node should be read with that qualification.
7. **Derived scopes confirmed, not an exhaustive allowed-value set:** `fetch_body.go:369-396` derives colon-qualified sources, public scopes and bare source fallback; `ingest_content.go:640-662` additionally accepts an explicit nonempty metadata scope. The proposed exact source-component predicate handles both bare and qualified org scopes correctly.
8. **Auth confirmed with operational qualification:** `internal/worker/sync_handlers.go:334-354` implements the shared Bearer key with empty-key and sync-client exemptions. `context_trace.go:82` records caller provenance without granting authority. The hub's configured key was not inspected.
9. **MCP confirmed in code:** `internal/graphmcp/client.go:181-187` sends Bearer/caller but no asker. Seven tools start at `server.go:76,98,111,127,144,187,206`; the final registration extends past the cited line 206. Client methods for the two admin-only routes are at `client.go:129-143`; Probe calls settings at `:152-154`, invoked by `cmd/agent-mem/mcp.go:70`. The payments entry in `~/.claude.json` was not independently inspected. Docker env forwarding is correctly described.
10. **Dashboard confirmed:** `dashboard/src/api.ts:11-19` adds Bearer and no asker. Source-wide fetch searches found graph calls using this wrapper; direct fetches elsewhere target stats/health or a static geography asset. Central admin injection covers the graph callers found.
11. **Traces confirmed:** `context_trace.go:121-133` records header/body eeid; `:285-289` reads the diagnostic mode with a fallback. Modes are assigned at `search.go:165-171` and `resolve.go:297-303`. Only search/resolve are wrapped (`router.go:40-44`); `migrations/20261011000001_context_traces.sql:11` uses TEXT. New modes require no enum migration.
12. **Eval confirmed:** old executable request key at `testdata/eval/graph_context/cases/case-01.yaml:9-11`; no header argument at `eval_common.go:163-175`; hard-coded dry-run headers at `eval_relevance.go:169-173`; old seed/query key at `eval_prepare.go:80,372`. R3 now specifies a coherent migration and actual request tests.
13. **p-agent confirmed in sibling code:** `../p-agent/src/graph-context/client.js:34-76` wraps all five listed route families; `:79-116` sends no admin and only optional asker on selected GETs. Deployment flags and current production population counts were not re-measured.

These qualifications are evidence limits or citation precision, not new implementation blockers.

## Route coverage, identity behavior and unnamed callers

The literal table covers all **30 currently mounted GET routes**: four filtered GETs and 26 admin-only GETs, plus the read-like `POST /resolve`.

Filtered: `/search`, `/node`, `/node/{id}/neighbors`, `/epic/{key}`, and `POST /resolve`.

Admin-only GETs: `/jobs`, `/topic-rules`, `/person`, `/traces/stats`, `/slack-users`, `/slack-user`, `/channels`, `/channels/recent`, `/channel`, `/channel/topics`, `/cluster/summary`, `/continents`, `/channel-filters`, `/eligibility-gate`, `/business-root`, `/temporal-timezone`, `/boost-alphas`, `/epic-briefs`, `/purpose`, `/slack-members`, `/jira-updates`, `/gh-key-search`, `/subscriptions`, `/pins`, `/pins/board`, `/node/{id}/subject-queries`.

`/traces/stats` is conditional on a recorder (`router.go:45-47`), which acceptance explicitly mounts. Neighbors is a mounted subrouter; `cards=1` is a response branch (`neighbors.go:523,558`), not an omitted route. The planned middleware must encompass that subrouter as well as direct registrations. The route-walk and mounted-request tests address this.

No current graph read returning content or people data was omitted. New GETs are admin-only by default. Mutation routes, including subscription PATCH, and non-graph APIs/direct DB access remain outside this graph-read contract; this is not a claim to secure those interfaces.

The ordered resolver rules remove unnamed/public-only fallbacks: absent identity denies, unknown identity denies even alongside a valid assertion, conflicting canonical eeids deny, and lookup/scope errors fail closed. Only exact admin `1` short-circuits identity checks. Whitespace, invalid numeric values, merged records, and trace rejection handling are specified. Merge traversal must use the referenced people row ID, not eeid (`migrations/20260527000001_graph_schema.sql:27`). No policy path was found that intentionally gives an unnamed non-admin caller unfiltered graph reads.

D1 is applied consistently to exact source components `jira`, `github`, `confluence`, including bare scopes. Slack and GWS gain no org exception. Extending `parentVisibleSQL` as well as the outer SQL/Go predicates fixes Jira attachments without discarding parent gating, deleted-parent exclusion or the existing any-visible-parent rule. No D1/D3 contradiction remains in this specification.

## Caller compatibility and the new risk disclosure

- Dashboard has an explicit admin caller path, with header precedence and embedded rebuild tested.
- MCP retains five filtered tools, removes the two incompatible admin-only tools, validates env before graph HTTP calls, never sends admin, and leaves Probe usable. The Docker `-e AGENT_MEM_ASKER` handoff is necessary and remains an operator-approved change, not an automatic user-config edit.
- Eval now preserves real-asker versus admin semantics through both execution paths and preserves label-sheet's saved-replay behavior.
- The newly documented ordinary Slack identity degradation is verified: `../p-agent/src/channels/slack/socket.js:2721-2743` calls the graph profile lookup; the following fallback at `:2744` onward obtains Slack names without graph department enrichment. This path is separate from disabled graph context injection. The graph client returns null on non-2xx, so fallback remains viable. Cached identities can delay the visible effect. The risk disclosure correctly keeps `/slack-user` admin-only rather than weakening D3.
- The Home-tab outage and proposed later filtered-owner change remain explicit operator choices in O1. Setting an asker alone cannot restore its `/slack-user` lookup; O1 correctly says to drop that lookup.

## Acceptance review and prior findings

All five round-1 fixes remain intact: complete default-admin route inventory; org-aware attachment parents; ordered identity/error contract; explicit MCP tool removal and Probe handling; mounted hidden-content tests across route branches.

The acceptance gates target actual regression mechanisms: hidden IDs/titles/bodies/edge metadata, linked search results and neighbor cards, unknown and builder-error denial, org near-matches and attachments, mounted admin gates, trace modes/items, MCP no-request behavior, dashboard header precedence, and separately captured eval requests. Acceptance 7 directly catches R2-1. These are future requirements, not tests run or certified by this review.

Non-blocking handoff reminders: implement the merge cap fail-closed; exercise the actual mounted middleware and real replay hash validation rather than only helper mocks; resolve the existing checked-in Mac-mini versus supplied VPS deploy-instruction conflict before any conductor deployment. No deploy was attempted here.

**Terminal disposition:** R2-1 resolved; no further plan revision requested. Proceed to implementation under the plan's no-merge/no-deploy restrictions and retain O1 for Enzo's sequencing decision.
