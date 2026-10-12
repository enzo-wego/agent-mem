# R1a lead fields — plan review R1

**Verdict: CHANGES REQUIRED**

Reviewed `docs/ai/2026-10-12-graph-context-r1a-lead-fields.md` (draft r1, 153 lines), the umbrella design, the two requested prior reviews, and the affected local implementation/callers.

## Scope and evidence

- Checkout HEAD: `55d90dc49a06065999a47af7200d1d2fac6c3d7a`. The plan names `8e97d74` (#98). Both objects exist locally; `git diff --name-only HEAD 8e97d74` lists only `.beads/config.yaml`, so the inspected application source agrees with the named base.
- Read-only source investigation, no hub, database, model, network research, builds, tests, deployment, commits or pushes. No credentials or customer fixtures were opened. Only this review file was written; the requested Herdr notification follows.
- Findings are verified **plan/contract gaps against source**, not claims that a not-yet-written implementation already leaks production data. Performance risk is identified statically, not measured.
- `graph: not-applicable: documentation-only plan review; existing paths traced manually`
- `fan_out: inline: no subagent tool`
- No security scanner or temporary review artifacts were used under the file-write restriction. ACL and masking were manually traced.

## Required revisions

### F1 — HIGH: root remapping needs a new authorization and eligibility boundary

**Plan:** A3/A4/A6, lines 84–117; acceptance 7, lines 141–142.

Resolve authorizes the original visited IDs at `resolve.go:271–299`, then hydrates those IDs. A3 introduces a different ID after that boundary. A visible reply does not constitute a check that its derived root exists, is not deleted, passes the request's scope/type/epic rules, or is itself visible. The proposed shared loader also introduces root-author and thread-aggregate reads for node/search responses whose selected row may be a reply.

There is an existing defensive precedent: `search_hybrid_arms.go:130–178` only promotes a hit when the root passes `searchFilter.sql`; otherwise it retains the member. A3 cannot retain replies under its stated contract, so it needs an explicit alternative (for example, omit the artifact). Existing seed canonicalization likewise joins an existing, non-deleted root (`resolve.go:544–553`). The new plan states neither policy.

**Required:** Define the pipeline as original-candidate authorization → canonical mapping → authorization/eligibility of every newly introduced root → selection/enrichment. Fail closed on lookup errors. Specify absent/deleted/ineligible-root handling without fabricating a readable root or borrowing its author. Scope aggregate queries by exact channel/thread pairs, live Slack node types and the applicable visibility rule. For links, authorize both original endpoints and mapped endpoints before retaining relationship prose or IDs; a visible root must not launder an inaccessible member edge.

**Acceptance:** Synthetic visible-reply/hidden-root, missing root, deleted root, root outside epic, mismatched-scope member, and root-lookup error fixtures across resolve, node-by-reply and both search modes. Assert absence of forbidden IDs, author IDs, relationship prose and aggregate contributions. Keep the hidden-seed test too, but it is not sufficient by itself.

### F2 — HIGH: cap summaries after masking, not before it

**Plan:** A4 line 97; acceptance 5/7.

All three routes are masked outside their handlers (`router.go:86,92,95`; `mask.go:101–126`). An implementation that follows A4 by cutting the raw summary inside a handler changes what the masker can recognize. For example, a complete email crossing rune 600 is recognized by `redact.go:38`, but cutting it before its domain suffix leaves an unrecognized email prefix in the response. Phone/card candidates similarly depend on complete token shapes. Existing tests over intact values do not cover this new boundary.

**Required:** Explicitly mask the complete selected summary for non-admin callers before truncating it, while retaining the outer fail-closed masker. Define whether 600 includes the ellipsis (recommend total length ≤600), and how admin responses are capped. Do not use `hydrate.TruncateRunes` as a rune counter: despite its name, its contract is a byte cap (`budget.go:126–140`).

**Acceptance:** Full email/phone/card sentinels crossing the cutoff, Unicode around the cutoff, lengths 599/600/601, admin versus filtered callers, and nested link prose on the actual mounted routes. Assert no partial sensitive token survives and final summary length is bounded.

### F3 — HIGH: collapsing IDs loses stored reply links, score/hop provenance and reply-seed focus

**Plan:** A3/A6, lines 84–89 and 116–117; acceptance 4/6.

`seedLinks` queries edges whose endpoints equal the final hydrated IDs (`resolve.go:717–721`). If a seed links to a reply of another thread and A3 substitutes that reply's root before this query, the edge disappears: no stored seed→root edge need exist. Keeping only the highest-scored member also loses useful links carried by other members. Adding `from`/`to` to the current query does not fix this.

Separately, response score/breakdown/hop are found by equality with the original scored ID (`resolve.go:359–369`). A substituted root may have no matching entry, or have a different entry. Seed canonicalization already discards the original reply ID (`resolve.go:503–558`), so a late collapse cannot recover the requested seed's `focus_ts`. And today's synthetic THREAD links represent siblings of a seed; after collapse those endpoints become the same root. The plan does not say whether to remove these self-links, keep them, or invent cross-root THREAD links.

**Required:** Specify a canonical group record retaining authorized member IDs, winning score/breakdown, deterministic tie-break, seed identity/hop-0 precedence, and original reply-seed focus before canonicalization. Gather stored links over authorized members of the selected groups, map endpoints afterward, and retain one coherent winning edge tuple per kind using the existing confidence/tie rules. Define self-link handling and focus precedence when a reply seed differs from the best-scored reply.

**Acceptance:** Cross-thread edge to a reply; edge from a seed-thread reply; two members with different edge kinds; equal-score candidates; seed root plus a higher-scored member; reply seed with a different best reply; and root absent from the original candidate set. Verify root IDs, nonzero retained scores, correct hop, focus, edge direction and intact winning metadata. Clarify that “no reply node ids in the response” applies to artifacts/links, not the existing raw `graph_trace.seeds` request echo.

### F4 — HIGH: search has two contracts, and “search already returns roots” is false

**Plan:** line 89, A4, expected files and acceptance 5.

Default search does not call `canonicalizeThreads`; the call is inside `if hybrid` at `search.go:264–294`. Hybrid itself falls back to a member ID when its root is ineligible. The claimed universal root behavior is therefore false.

More importantly, hybrid output uses a separate `hybridResult` and `hybridDisplay` (`search_hybrid_display.go:14–44,60–219`), not the listed `searchResult`. The adapter explicitly copies fields, then replaces Slack `summary` with `thread_summaries.overview` at lines 184–186. Adding fields/capping summaries in `search.go` alone will either drop the new fields or have its summary cap/source overwritten. p-agent's Mem flow actually invokes `hybridSearch` (`socket.js:8146`; `client.js:46–53`), and Home renders its summary (`home-tab.js:1116–1117`).

**Required:** Name default and hybrid search separately, include `search_hybrid_display.go` in the implementation inventory, and define where final enrichment/capping occurs. Preserve existing hybrid card fields. Decide explicitly whether to retain the overview meaning or replace it with the one-line summary; that is an existing-caller behavior change, not an additive field. Define root-author enrichment for retained member IDs without changing node-by-id identity. State Slack-summary fallback when the thread summary is absent: the umbrella/parent promise thread summary then artifact summary, while A4 currently selects only the thread table for Slack.

**Acceptance:** Mounted default and hybrid HTTP responses, including linked hybrid rows, absent thread summary, retained member IDs, and a >600-rune overview. Verify the exact final JSON fields and compatibility with the Home/dashboard card shape. Do not claim R3's server-side prerequisite complete by testing only default search.

### F5 — MEDIUM: the new metadata-only path silently drops cache warming unless its miss contract is specified

**Plan:** A2 lines 76–80 and A1; “additive” claim line 18.

Today `Greedy` returns `missed`, which supplies both `cache_misses` and the `fetch_body` enqueue loop (`budget.go:72–74`; `resolve.go:356,432–443`). A2 removes the only producer on the false-body path without replacing it. The umbrella explicitly uses `include_bodies:false, no_side_effects:false` to warm the cache. Existing `TestNoSideEffectsRequestsUseReadOnlyPoolAndSkipEnqueue` also requires an unflagged bodyless request to enqueue and flagged requests to retain `cache_misses` (`no_side_effects_test.go:90–92,128–134`).

The `max_items` contract is also incomplete: seed fallbacks are appended after selection today, so the response can exceed the cap; removing fallback instead breaks consumers that require hop 0 (`GraphSearchPage.tsx:698–705`). Equal scores currently inherit map iteration order, so top-N introduces an unstable cutoff. Negative limits have no defined handling.

**Required:** Define metadata-only miss discovery without fetching full bodies, the miss/enqueue population, and preservation of the header override. Define cap/default/clamping, whether seeds consume slots or are a bounded exception, deterministic ordering, and behavior when seeds exceed the cap. Preserve the dashboard's hop-0 anchor deliberately. Clarify `context_tokens` units/rounding and that it is a new meaning on the false-body path.

**Acceptance:** Flagged/unflagged bodyless candidates, explicit false header overriding true body flag, cache-miss JSON, seeds at/above the limit, invalid limits and repeatable equal-score ordering. Explicit compatibility tests for the dashboard and p-agent Mem resolve calls, both of which omit `include_bodies`.

### F6 — MEDIUM: “one batched query” does not bound thread-aggregate work

**Plan:** A4 line 98; A5 line 112; verify line 149.

The cited channels query (`channels.go:549–556`) is constrained to one channel and computes a JSON-expression thread key across that channel's nodes. The migrations provide scope/type/update indexes but no index on the normalized thread-key expression. A multi-channel batch can still scan all rows in every selected busy channel (or the whole table if implemented as an unconstrained grouped subquery). A limit of 30/100 artifacts does not bound those row reads. Aggregating activity and bot presence independently compounds the work; running enrichment before search's final limit also magnifies it.

This is a verified access-path risk, not a measured claim that the proposed SQL exceeds 186 ms.

**Required:** Specify a bounded pair-filtered aggregate over selected `(scope, thread_ts)` keys, compute activity and bot presence together, and choose an indexed thread lookup or provide measured evidence that current indexes suffice at representative scale. State placement after final selection/linked expansion, query count, cancellation/error behavior, and avoid nested pool queries while rows remain open. Include any required index migration in scope.

**Acceptance:** Synthetic high-volume channels with many unrelated threads, multiple channels sharing the same timestamp, a hot thread, and max-sized batches; `EXPLAIN (ANALYZE, BUFFERS)` evidence on the scratch DB plus query-count and cancellation assertions. Benchmark default/hybrid search and node too, not only resolve. No production probe is requested for this review.

### F7 — MEDIUM: bot provenance is not yet a complete, checkable prerequisite contract

**Plan:** A4 lines 99–101, A5; umbrella P3.

A4 defines root bot status but leaves failure/missing-author behavior to `omitempty`. A missing boolean cannot distinguish “known human” from “lookup unavailable,” which matters to the later bot filter. `has_bot_replies` means “thread contains a node” in the table, so a bot-authored root alone can make it true despite its name. It only includes answer-list bots, while `authored_by_bot` also includes `people.is_bot`; that distinction needs an explicit fixture rather than an inferred common meaning.

The umbrella P3 additionally requires bot messages in summaries to be marked `[bot answer, unverified]`. Copying cached `thread_summaries.summary` does not provide that: the current summarizer is not changed, and the expected files contain no summary-generation/cache-invalidating work. A human-root thread with bot replies can therefore still return an unmarked summary. A boolean is not the promised text attribution.

**Required:** State false-versus-unknown/error semantics and whether enrichment errors fail the request or carry an explicit unknown state. Define whether root-only bots count as replies and whether generic `is_bot` replies count. Either deliver the umbrella summary-label contract here with safe cache handling, or explicitly identify it as an outstanding prerequisite owned by a later round; do not label P1–P5 fully delivered. This does not ask to flip `people.is_bot`, change alerts, remove edges, or implement the out-of-scope detector.

**Acceptance:** Answer-list bot, generic bot, human, missing person, loader failure, root-only bot, human root with bot reply, and cached mixed-authorship summary. Define non-Slack author behavior too: “root author” is otherwise undefined for those artifacts.

### F8 — HIGH: the published verification command prints content, and the trace test needs a real no-write boundary

**Plan:** goal lines 14–15; A1; acceptance 2; verify lines 147–153.

The exact recommended `eval relevance` command prints the serialized request body and request URL at `cmd/agent-mem/eval_relevance.go:177–178`. Bodies can contain the case query; it is not a counts-only command. Disabling server traces does not prevent this local output from entering terminal/session logs. The plan's “leave no content behind” wording and no-context handoff must not imply otherwise.

The trace change itself is directionally correct: both routes are wrapped, eval sends the header, and body-only suppression is clearly distinguished. But acceptance only counts trace enqueues. The predecessor trace finding explicitly requires both no fetch-body jobs and no content-bearing trace writes. The existing read-only test deliberately lets traces write using a separate normal pool (`no_side_effects_test.go:77–80`); passing it alone does not certify the new property.

**Required:** Specify a content-safe evaluation/reporting path before telling the conductor to run it (for example, approved no-content output support, or explicit safe handling that does not forward raw stdout/stderr to the agent transcript). Do not silently expand this into a claim of zero vendor retention or zero client-side persistence. Test the mounted masked resolve/search middleware with a recorder/store spy and a missing-body fixture, asserting no enqueue and no eventual store writes after draining the recorder, plus zero fetch jobs. Cover `1`, case-insensitive `true`, header/body override, body-only behavior, errors, and request-ID header preservation. State whether JSON `request_id` is also preserved.

For the later live gate, replace `<deploy>` with an exact timestamp/revision procedure, wait for preexisting trace work to drain, and report unsuccessful requests as failures rather than vacuous zero traces. Identify resolve-asker as the 155 ms baseline population, fix the data revision/request mode, and measure the new false-body path explicitly. The canned eval does not automatically prove the new path merely because resolve appears in its name.

## Code-fact and line-reference audit

All numbered source references in the supplied Facts section were checked against the local application source, which matches the named base as noted above.

| Claim/reference | Result |
|---|---|
| `resolveRequest` 26–39; artifact 48–62; unfilled Summary/Via literal 376–382 | Correct. |
| Score sorting 305–309; Greedy 316; author/thread join 320–345; blanking 372–375 | Correct. Author join is best-effort and only covers hydrated rows. |
| Seed fallback 397–430 | Range correct, but “title/summary” is imprecise: the query uses thread summary as **Title** fallback; it does not fill the Summary field. |
| `seedLinks` 695–802; link struct 65–71; THREAD 774–776; order 673 | Correct. Stored edge direction is already retained internally in `linkPick`; current lookup is direct seed/member IDs, not canonical groups. |
| No-side-effects 436–444 | Correct: nonempty header overrides body, `true` is case-insensitive, only fetch enqueue is gated. |
| `Greedy` 37–123, NULL body and oversized skips, title join 49–61 | Correct with an important omitted exception: when no body fits, the top oversized body is truncated and returned (114–120). Preserve this on true-body calls. |
| Thread summary table/key; artifact summary/kind; no last-reply column | Confirmed in migrations. |
| `channels.go` 549–556 | Correct grouping precedent; actual expression is maximum update epoch in milliseconds, restricted to one scope and live rows, not a ready-made cross-channel batch. |
| People schema, unique Slack ID, no answer_bots table or seeded ID in Go/migrations | Confirmed by schema and repository search. |
| Slack IDs 47–54 and Slack-file shape | Correct; SlackFile constructor follows at 57–59. |
| Node 27–38 and identity at 48 | Correct. Existing Summary is artifact_index; body default is present; node ID may remain a reply. |
| Search 65–82 and identity at 115 | Correct for default search only; hybrid uses another struct and adapter (F4). |
| Router 92/95; trace 50–93; eval header 95 | Correct; node is masked at router 86 but is not trace-wrapped. |
| Migration/test commands and helper files | Exist and match the cited workflow. DB helpers require scratch DBs; the review did not run them. |
| “search already returns roots” | Incorrect for default search, and conditional for hybrid (F4). |
| “All response changes are additive” | Incorrect as a behavior claim: summary source/capping, omission of an existing empty search summary, selection/caps/token accounting and collapsed IDs change existing responses. Document intended compatibility rather than assert universal additivity. |

## Existing callers and compatibility boundary

- **p-agent Mem/Home:** `socket.js:8128` resolves without `include_bodies`; `:8146` uses hybrid search. Preserve hop-0 canonical anchors, hybrid cards and useful summaries. `client.js:64–69` currently cannot forward `body=0`; the later p-agent adapter must add it. No p-agent change is demanded in R1a.
- **p-agent automatic context:** `src/graph-context/index.js:16–22` explicitly requests bodies. A3 still changes its returned IDs/content selection; keep the existing Greedy truncation fallback and test that intended difference.
- **Dashboard:** `dashboard/src/api.ts:535` omits the body flag. `GraphSearchPage.tsx:698–705` requires a hop-0 artifact. Top-N must not accidentally remove that anchor.
- **MCP:** `internal/graphmcp/server.go:115–145` defaults bodies to true, validates token budgets and returns a generic response map. Additive fields should deserialize safely; collapse remains an intentional behavior change. Existing MCP input types do not expose `max_items` or node `body=0`; explicitly defer that surface rather than claim every transport can use the new options.
- **Eval:** transport uses the no-side-effects header and reads request IDs from headers, so header preservation is sufficient for that caller. Collapsed IDs change the scoring population against existing labels/oracle; R1b owns relabelling. Do not interpret changed precision/recall as a controlled comparison without recording that boundary. The eval's current content-bearing stdout is F8.

## Acceptance audit and prior-finding disposition

- **pi r2 R8:** A2 directly addresses body-cache/budget starvation. Closure remains conditional on preserving misses/seed anchors and specifying selection limits (F5), then testing the actual false-body route.
- **pi r2 R3:** R1a covers the right server endpoints but not all their real response shapes (F4), root/enrichment authorization (F1), or unknown provenance (F7). HTTP identity transport, safe allowlisting, destination filtering, bounded parsing and search classification remain owned by p-agent R4; explicitly retain those obligations instead of claiming the entire cross-repo finding closed here.
- **offline trace R1:** A1 selects a valid server contract. Close after combined server no-write tests and content-safe verification instructions (F8), not after a client-header fake test.
- Acceptance 1 and 8 are executable once implemented on an isolated scratch DB. Include the ordinary non-DB tests relevant to MCP and the new pure helpers; `go vet` is not a substitute for them.
- Acceptance 3 needs explicit expected artifacts, scores, misses and body behavior, not “old result unchanged” without a pinned fixture.
- Acceptance 4 conflicts with A3 if the root itself wins: A3 only sets focus when the winner is a reply, while the acceptance unconditionally expects the best reply. Define precedence and account for raw seed echoes (F3).
- Acceptance 5 needs field applicability/types, UTC/RFC3339 representation, null/false/unknown rules, summary fallback, and ellipsis length. `author_slack_id` is a string, not “true”; non-Slack nodes should still get summary/activity under A4, so “all fields absent for non-matching types” is not a complete oracle.
- Acceptance 6 needs stored-edge direction, multiple seeds/kinds and collapsed self-link behavior (F3).
- Acceptance 7 needs new-boundary fixtures, not only unchanged tests (F1/F2).
- The deploy instruction “usual hub command” is not self-contained. Copy the approved local-build/push/pull-only procedure and build-revision check from the repository runbook into the handoff or give its exact named command. Never ask the hub to build images. Deployment remains owner-gated and was not attempted.

The changes can remain within the proposed agent-mem round. No trust-tier, semantic-scoring, detector, label, alert-policy or edge-rejection expansion is required to resolve the central blockers. Re-review should focus on these eight findings and the resulting contract delta.

**CHANGES REQUIRED**
