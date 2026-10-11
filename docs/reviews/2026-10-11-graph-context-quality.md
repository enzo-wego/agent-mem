# Review: graph context quality plan r2

**Verdict: CHANGES REQUIRED**

Reviewed `docs/ai/2026-10-11-graph-context-quality.md` against checkout `5d18336297b631f5cea3bb7c7cf81dcecc66cccc`. The plan was untracked in this checkout. This is a repository-backed plan review, not a production-data audit or an implementation review.

The direction is useful: explicit asker handling, withholding low-trust guesses, thread deduplication, provenance, and rejection feedback should improve context. However, the proposed gates do not establish the stated safety property. A permitted thread can carry a restricted resource's content through a cached summary; bot-generated references can look human-authored after thread fetching; and model-facing tools other than resolve remain escape paths. An implementer following the document literally could meet its numerical gates while still returning unauthorized or incorrectly attributed context.

**Handoff is blocked.** Q1 is explicitly unanswered, several contracts require decisions, and the baseline/probe fixtures are absent. The implementer cannot resolve these without additional authority or context.

## Scope and verification

- Read the plan and traced ACL, resolve/BFS/hydration, thread fetching and summarization, indexing/identifiers, topic linking, edge reconciliation, MCP, and relevant tests and deployment guidance.
- All code references below are repository-relative and refer to this checkout. Plan references identify its sections and line ranges.
- No source changes, deployments, database queries, network research, test execution, issue updates, commits, or external posts were performed. Only this requested review artifact was written. The requested Herdr completion signal follows it.
- Production counts, the precision sample, the A/B outcome, and p-agent behavior are **not independently verified**. The referenced p-agent files are outside the requested repository.
- Manual source tracing was used. A structural code graph and dedicated security scanner were not run; there is no implementation diff to inventory. No subagent tool is available.

## Numbered findings

### 1. [must-fix] ACL on the returned node does not protect content imported into its cached summary

**Plan:** Phase 1 shared filter; Phase 2 filling `summary` from `artifact_index` (lines 171, 193–194).

**Evidence:** `internal/graph/handlers/summarize_thread.go:181–209` builds a global summary from the thread plus linked resources. `linkedResourceBlock` at `:293–324` reads referenced resource titles and up to 500 body characters without checking the eventual asker's access. `index_artifact.go:118–137` copies that summary into the thread's index. Resolve hydration already uses a thread summary as a title (`internal/graph/hydrate/budget.go:51–63`); `/node` also returns indexed summaries (`handlers/node.go:71–78`). Hybrid search replaces titles/summaries with cached thread summaries and exposes decisions (`handlers/search_hybrid_display.go:160–177`).

**Failure:** An asker may see Slack thread A but not referenced GWS document B. Filtering B out of the artifact list still permits A's generated title/summary to reveal B's content. Masking PII does not remove unauthorized non-PII information. Rejecting A→B later also does not remove already synthesized content.

**Required change:** Give derived content a complete dependency/provenance set and authorize every contributing source, or withhold it/regenerate from authorized sources only. Cover title fallbacks, overview, decisions, snippets, and cached summaries, not just body fields. Invalidate or quarantine existing unsafe caches. The existing epic brief source check (`handlers/epic.go:260–264`) is a useful precedent, not proof that thread summaries are safe.

**Checkable gate:** A fixture with an allowed Slack root and a forbidden linked document containing a unique canary must expose neither the document's title nor canary anywhere in any model-facing response, including cached and summary-only responses. Re-run after permission revocation and link rejection.

### 2. [must-fix] The plan does not close all model-facing read paths

**Plan:** Phase 1 names resolve, neighbors, node, search, and “cards”; later safety fields are described primarily for resolve.

**Evidence:** MCP exposes `graph_person` and `graph_epic` (`internal/graphmcp/server.go:188–227`), as well as cluster summaries (`:126–140`). `handlers/person.go:319–339` returns recent artifact IDs, URLs, and titles without a scope predicate. `handlers/cluster_summary.go:122–126` denies requests only when `X-Asker-User` is nonempty; an absent identity still permits unfiltered traversal and model synthesis. `/epic` has its own ACL and brief handling (`handlers/epic.go:152–264`). All these routes are mounted in `handlers/router.go:35–58`.

**Failure:** Configuring an asker in MCP will not fix `graph_person`. A model can obtain forbidden titles there after resolve correctly refuses them. Leaving the cluster endpoint's absent-header rule unchanged preserves another implicit unfiltered model path. Applying `min_trust` only in resolve also leaves search/neighbors/epic free to deliver weak relationships without the new labels.

**Required change:** Enumerate every MCP tool and model-facing HTTP endpoint in a route matrix. For each, specify identity handling, authorization of nested/derived fields, trust filtering, rejection filtering, labeling, and whether it is explicitly admin-only. “Cards” must identify concrete routes/helpers. Either provide the same safe contract or deny non-admin access; do not imply that protecting resolve protects the answering model generally.

**Checkable gate:** Run the same forbidden-node and low-trust canaries through every exposed MCP tool and its HTTP route, including nested arrays, source citations, summaries, and error payloads.

### 3. [must-fix] `trusted` remains a caller-controlled bypass, and MCP opts into it on configuration failure

**Plan:** Asker table and MCP fallback (lines 153–167); Q3 (306–307).

**Evidence:** The existing API-key boundary explicitly treats asker identity as advisory (`internal/graph/handlers/router.go:7–17`). `internal/graphmcp/client.go:185` sends the shared bearer key. There is no role/capability distinction in that middleware: `internal/worker/sync_handlers.go:344–353` checks a single key and skips enforcement in local/sync-client mode.

**Failure:** Moving from implicit unfiltered mode to `trusted: true` is useful error prevention, but it does not authenticate the asker or authorize elevation. The proposed missing-`AGENT_MEM_ASKER` fallback automatically restores unfiltered model context, precisely when identity is unavailable. A warning is not an access barrier. “Logged with caller” is also undefined when all callers share one credential.

**Required change:** State the trust boundary honestly. Bind trusted mode to a server-authorized admin/service capability, or explicitly constrain this to advisory ACL for trusted integrations and do not claim a per-user security guarantee. MCP should fail closed when asker configuration is absent unless a separately configured, explicit admin mode is enabled. Define conflict behavior for trusted plus asker inputs and record a server-authenticated caller identity. Resolve Q3 before handoff.

**Checkable gate:** Ordinary integration credentials cannot elevate by either header or body flag. Missing MCP configuration denies retrieval. An explicitly authorized admin client is the only tested exception. Include local/sync-client deployment behavior.

### 4. [must-fix] Asker resolution and its compatibility alias lack a complete, non-circular contract

**Plan:** Asker table, `/node` identity alias, and p-agent compatibility (lines 153–177).

**Evidence:** Resolve currently reads only `asker_eeid` from its body (`internal/graph/handlers/resolve.go:22–33,195,282`), while other endpoints read the header. `lookupAsker` (`handlers/search.go:728–745`) neither validates a numeric eeid nor filters inactive/merged people. The proposed alias returns the shape of `slackUserProfile`, not the existing node response (`handlers/slack_user_api.go:14–22`).

**Missing decisions:** Header versus body precedence; conflicting valid identities; nonzero unknown/negative eeids; inactive/merged identities; malformed numeric input; lookup database failure versus unknown identity; and whether `asker_slack_id` accepts only Slack IDs or every header identity form. The table can also be read as requiring a body identity even when MCP supplies a valid header.

The alias is a bootstrap hazard: the current caller is asking `/node?id=person:slack:...` to learn its eeid before it can supply one. If `/node` first enforces `asker_required`, the alias cannot repair the lookup. Merely delegating to the Slack lookup also requires stripping `person:slack:` and verifying the exact response shape p-agent reads. The plan provides no repository-local consumer fixture to prove that compatibility claim.

**Required change:** Specify a single resolver and exhaustive precedence/error matrix. Define a narrowly authorized identity-discovery path that cannot become a content bypass. Supply a captured/synthetic p-agent contract fixture demonstrating alias request and response parsing without changing p-agent. If that cannot be verified in this repo, label the compatibility claim as blocked rather than guaranteed.

**Checkable gate:** Table-driven HTTP and MCP-client tests cover every input combination, including bootstrap without a known eeid and lookup errors. Unknown or conflicting principals never receive org grants or unfiltered context.

### 5. [must-fix] Org-wide grants are undecided and cannot be implemented safely from the proposed scope list alone

**Plan:** Phase 1 org scopes and Q1 (lines 148–150, 298–301).

**Evidence:** The repository has `graph.people.active` (`migrations/20260728000001_people_job_title_active.sql:8–10`), but the existing lookup and member-scope builder do not enforce active status. Scope derivation can produce `jira:<project>`, `github:<repo>`, and `confluence:<space>` as well as coarse source names (`handlers/fetch_body.go:369–396`). GWS falls through to the coarse source name. The production membership writer in this repo is the Slack refresh (`handlers/refresh_slack_members.go:245–248`).

**Failure:** “Admit scope in org_scopes” without an active canonical-person check grants organizational content to an invented or departed eeid if the caller supplies it. Bare `github` does not match `github:wego/payments` under the existing exact-match predicate. Conversely, broad source grants may authorize restricted repositories/spaces. “GWS stays per file” is not an implemented entitlement scheme in these paths.

**Required change:** Supply the approved audience and exact scope rows before dispatch. Specify active/merged-person handling, revocation/cache lifetime, prohibition on Slack scopes in this table, and behavior for unknown/unscoped source nodes. For GWS either implement file scopes plus their grant source, or explicitly deny it to ordinary askers until that exists. Do not silently substitute org-wide access for missing source entitlements.

**Checkable gate:** Unknown, inactive, merged, and outside-audience principals fail; allowed and restricted projects/repos/spaces are separate fixtures; coarse and qualified scopes are tested; changing grants or active status has a bounded, tested revocation delay.

### 6. [must-fix] Authorization must constrain traversal and complete provenance paths, not just final artifacts

**Plan:** Phase 1 filtering and Phase 2 relation paths (lines 171, 193–195).

**Evidence:** Resolve expands canonical seeds and structural corridors before ACL filtering (`internal/graph/handlers/resolve.go:201–257`); the final candidate filter is at `:279–294`. `bfs.Expand` reads edges in both directions without an ACL predicate (`internal/graph/bfs/expand.go:35–48`). `displayHops` keeps the first visited candidate and only its last edge kind (`handlers/resolve.go:122–145`), not a complete path.

**Failure:** A hidden seed or hidden intermediate node can establish a relationship between otherwise visible nodes. Once complete relations are exposed, serializing that path or an LLM `why` can reveal the hidden node or its content. Filtering the returned endpoint alone cannot justify an authorized relation. The current hop-one link code deliberately filters visible seeds (`resolve.go:374–385`), which needs to remain true for every hop.

**Required change:** Define permitted seed canonicalization, expansion, and path selection under ACL. Every emitted relation must have authorized endpoints, intermediates, and evidence sources; otherwise withhold it. Include seed ID, edge ID, from/to, direction, and evidence/source message in the relation contract. Define a deterministic choice among multiple paths that prefers a valid stronger path, rather than whichever unordered SQL row BFS sees first. Missing provenance must fail closed for high trust.

**Checkable gate:** Test hidden seed→visible node and visible seed→hidden intermediate→visible node; neither hidden metadata nor unsupported relations may escape. Test parallel structural/LLM edges, competing paths, reply promotion, cycles, and stable ordering.

### 7. [must-fix] Bot references can already be attributed to a human root, bypassing all proposed bot-source checks

**Plan:** Phase 3 bot-source filtering and root-author trust cap (lines 228–236).

**Evidence chain:**

1. `internal/graph/fetchers/slack.go:146–185` concatenates parent plus replies into one thread body, but `:238–242` attributes the fetched node to the parent.
2. `handlers/fetch_body.go:187–189,213–222` stores/extracts from that combined text.
3. `reconcileEdges` records the root as `source_msg_id` (`handlers/fetch_body.go:422–428`).
4. `materializeThreadReferences` accepts REFERENCES from every thread message including that root (`handlers/link_topics.go:683–699`).
5. `threadRawText`/`identifiersForNode` include thread text without bot exclusion (`handlers/identifiers.go:108–152`), and summary generation also includes all authors (`handlers/summarize_thread.go:117–164`).

**Failure:** Human asks a question; EnzoBot invents a Jira key or permalink in a reply; root fetching makes that reference appear authored by the human. Skipping source rows whose author is in `answer_bots` misses the root-derived copy. The target remains reachable through structural REFERENCES even if a REFERS_TO edge is rejected. The same bot-invented identifier can satisfy `inferred-high` verification, and a generated summary can repeat the bot answer without a bot label.

**Required change:** Preserve message-level provenance through fetching, extraction, identifiers, and summary generation. Exclude bot-only evidence from structural/high-trust relations even when embedded in a human root; quarantine existing contaminated root references and indexes or lower their trust when provenance cannot be recovered. Treat both existing `people.is_bot` and configured answer bots deliberately without changing alert policy. Root-author-only checks are insufficient.

**Checkable gate:** A human-root thread whose only occurrence of a false identifier/permalink is in a bot reply must not yield a default structural/high-trust neighbor through any edge, identifier, cached summary, or alternate path. A separate human reference should still qualify.

### 8. [must-fix] Read-time thread composition can duplicate content, omit the root, or silently emit unlabelled bot content

**Plan:** Phase 3 body reconstruction and fallback (lines 221–226).

**Evidence:** The fetch path writes a concatenated thread into both `graph.nodes.body` and `artifact_bodies.body_full` (`handlers/fetch_body.go:162–208`), while live ingest writes a single message body (`handlers/ingest_content.go:270–278`) and backfill writes individual messages (`handlers/backfill_slack.go:365–370`). These are not interchangeable message-only stores. Slack fetching also requests only `limit=200` and returns the first response's messages without pagination (`internal/graph/fetchers/slack.go:290–308`).

**Failure:** Composing root plus replies can repeat the root's already-aggregated replies. Composing only replies loses the original request. Existence of some reply rows does not prove completeness. Falling back to `body_full` with only the root author's bot status returns unlabelled bot replies for a human root. Hydration-only truncation flags cannot identify an already incomplete upstream transcript.

**Required change:** Specify a canonical message-only source and root inclusion rule, ordering, edit/delete reconciliation, and a completeness signal. Do not infer completeness from “some replies exist.” Unknown author/completeness must be explicitly labeled or withheld, not silently treated as human and complete. Define source truncation separately from budget truncation and require human-readable Slack author names during reconstruction.

**Checkable gate:** Cover zero, partial, and complete reply rows; aggregated versus single-message roots; missing/deleted replies; more than 200 messages; bot replies under human roots; and multibyte budget cuts. Each message appears once, the root survives, and unknown completeness/bot provenance cannot report clean labels.

### 9. [must-fix] Rejection needs a durable identity, write-path enforcement, and earlier schema creation

**Plan:** Phase 3 migration to `edges_rejected`; Phase 4 rejection table, sweep, and endpoint (lines 230–235, 253–260).

**Evidence:** Topic edges are upserted on `(from_node_id,to_node_id,kind)` (`handlers/link_topics.go:877–886`); cached positive judgments can recreate them (`:174–203`). REFERENCES and REFERS_TO have independent insertion paths. The same tuple is the edge uniqueness key (`migrations/20260527000001_graph_schema.sql:74–86`).

**Failure:** Moving an edge row out of `graph.edges` does not prevent its re-creation under a new numeric ID. A resolve-only rejection check does not prevent resurrection in other readers, derived summaries, or epic membership. Phase 3 also uses a table whose operational definition is deferred to Phase 4 despite the requirement that each phase independently pass and deploy.

**Required change:** Create the rejection schema before Phase 3 uses it. Define canonical tuple/content-version identity, directed versus undirected kinds, immutable rejection audit, transaction/concurrency semantics, retries, and explicit reinstatement policy. All edge writers and readers must respect active rejections, including cached judgments and materializers. Invalidate/recompute derived artifacts that relied on a rejected edge. Specify how a manual rejection becomes an eval fixture with seeds/query/asker and reviewable labels; accepting only edge ID and reason does not supply that context.

**Checkable gate:** Reject, rerun every applicable writer concurrently, restart, and query through all model-facing routes; the relation stays absent. Sweep and migration are idempotent. Restore/re-review behavior is explicit. Phase 3 runs on a clean database with only its prerequisite migrations.

### 10. [must-fix] A single REFERS_TO `source_msg_id` cannot represent multiple supporting messages, and deletion is not covered

**Plan:** Phase 3 provenance and pruning (lines 230–235).

**Evidence:** `materializeThreadReferences` collapses all sources to one root pair with `SELECT DISTINCT` and `ON CONFLICT ... DO NOTHING` (`handlers/link_topics.go:684–699`). `pruneStaleEdges` only reconciles edges whose from-node and `source_msg_id` equal the fetched message (`handlers/fetch_body.go:440–456`). Message deletion in `handlers/ingest_content.go:162–176` soft-deletes the node and returns without running that pruning path.

**Failure:** Two human messages can support one root→root edge. Recording one source then deleting it either incorrectly removes a still-supported relation or leaves stale provenance. A human and bot can both support the same pair, so rejecting “62 bot edges” without recomputing support can remove valid human links. Soft deletion leaves derived edges even if fetch-time pruning is correct.

**Required change:** Model support as a set of source message/edge records, or recompute the entire root-pair support set transactionally. Keep a relation only while at least one live eligible support remains. Handle edits, soft/hard deletes, author reclassification, missing sources, and migration of legacy NULL provenance. Derive migration targets from current data, not a recorded count.

**Checkable gate:** Two-source, human-plus-bot, edit, soft-delete, final-source removal, and repeated migration cases assert both visibility and accurate provenance.

### 11. [must-fix] Trust classification and ranking are not precise enough to implement or verify

**Plan:** Response contract and Phase 2 trust scoring (lines 101–108, 191–197); Phase 5 provenance (270–271).

**Evidence:** Existing SAME_TOPIC methods are `cosine-shortlist + llm-confirm`, `shared-identifier + llm-confirm`, and `same-case + llm-confirm` (`handlers/link_topics.go:848–874`), not the proposed enum. REFERENCES can be generated from bare Jira keys (`internal/graph/extractor/extractor.go:510–526`) as well as URLs/attachments. The GH key-search writer does not currently set the proposed method (`handlers/refresh_gh_key_search.go:535–538`). Legacy edges lack methods. Resolve expands all edge kinds but only selected kinds act as corridors (`handlers/resolve.go:97–101`; `bfs/expand.go:35–48`).

**Missing decisions:** Unknown/legacy methods and kinds; missing confidence or index; stale identifiers; two inferred hops; selecting among multiple paths; seed trust/exemption; root authored by a bot; exact trust-to-score formula; semantic range/normalization; ties; and minimum relevance within a trust tier. Phase 3 says bot answers stay, but the same plan caps bot-root artifacts at `inferred-low`, excluded by default.

“Deterministic” proves extraction, not substantive relevance or source truth. A ticket merely quoted as an unrelated example is a mechanically correct reference, not necessarily good answering context. The response must not conflate relation trust with content verification.

**Required change:** Provide a complete classification table and deterministic ranking algorithm. Unknown provenance must not silently become structural. Define explicit seed and bot behavior. Map every existing writer and legacy method, and complete necessary provenance migration before relying on it in Phase 2. Surface source evidence and label relation trust separately from factual verification; use query relevance or an explicit eligibility policy within structural candidates.

**Checkable gate:** Golden paths cover every class, missing field, threshold boundary, competing path, bot seed/non-seed, and structurally linked but irrelevant example. Default output contains no unknown/low-trust non-seed relation; repeated requests have stable order and scores.

### 12. [must-fix] Query ranking cannot satisfy the query-driven discovery use cases

**Plan:** UC4–UC8 and Phase 2 Sem implementation (lines 86–90, 188–190).

**Evidence:** Resolve's candidate population is seed-driven BFS (`handlers/resolve.go:201–257`); `scoreAll` only scores surviving candidates (`:540–583`). Embedding the query and comparing those candidates does not discover new ones. UC5 expects Confluence/Jira/PR resources on a topic even when there is no qualifying existing relation. The default high-trust rule also withholds inferred links without a shared identifier.

**Failure:** A fresh thread seed and a payment reference supplied only in `query` may have no edge to an older thread containing the same reference. No Sem score can make that older thread enter resolve. The same applies to topical documents not already connected by a permitted path.

**Required change:** Either explicitly add ACL-filtered exact-identifier and semantic candidate generation with provenance/trust semantics, or narrow the use cases and must-labels to reachable graph candidates and document a separate search workflow. Query-discovered items need a relation/method representation; the current hop-only enum has none. Define degraded embedding behavior without relaxing trust or adding recency filler.

**Checkable gate:** Use a new seed with no stored edges and a query that names an existing exact reference, plus a topical no-identifier case. The expected result must follow the chosen design, not depend on an incidental pre-existing SAME_TOPIC edge.

### 13. [must-fix] Enabling real askers before the safety phases activates existing consumers too early

**Plan:** Sequential deployment per phase; Phase 1 alias “without a p-agent change”; later p-agent plan waits for all gates.

**Evidence:** Phase 1 is explicitly intended to make the current p-agent lookup start resolving a real eeid and to restore Jira/PR nodes. Phase 2 adds trust filtering/labels, Phase 3 handles bot content, and Phase 4 handles rejected links. Current resolve emits raw bodies and does not include those safeguards (`handlers/resolve.go:350–370`).

**Failure:** Existing p-agent consumers do not wait for the future p-agent plan. Deploying the compatibility alias and org grants can immediately send newly available artifacts through an unlabeled/unfiltered-by-trust context path. Additive JSON fields do not prove that a consumer includes them in the model prompt, and the current consumer contract is not supplied.

**Required change:** Specify a rollout barrier: keep ordinary model context disabled or behind a safe-contract version/capability until ACL, trust, bot provenance, and rejection gates are satisfied together. Admin shadow evaluation can proceed independently. Provide a local contract test for the exact serialized context reaching the consumer, or explicitly withhold activation until the later client plan implements that contract. Include rollback that does not restore implicit unfiltered access.

**Checkable gate:** Partial deployments and old clients either receive the known-safe contract or no context. The compatibility fix cannot independently activate unsafe data delivery.

### 14. [must-fix] The eval cannot independently detect the safety failures it claims to count

**Plan:** Phase 0 fixture schema/counters and Phase 2 gates (lines 121–143, 206–208).

**Problem:** Fixtures contain asker, seeds/query, and `must/may/must_not` IDs, but no oracle for original body length, bot-message authorship, full thread membership, source entitlements, hidden text embedded in summaries, or expected relation support. A response saying `truncated:false` and `authored_by_bot:false` cannot be trusted to certify itself. Forbidden node-ID intersection does not detect forbidden text inside an allowed node. A zero count can therefore mean “not measured.”

The precision formula needs disjoint label sets, a zero-denominator policy, defined aggregate weighting, tie ordering, and a policy for unlabelled returned nodes. Including user-provided seeds can inflate the quality measure for suggested neighbors. Resolve and search need separate expected sets because a query search need not return every structural seed/epic. “100% of judgeable” permits a vacuous pass unless the minimum judgeable population is specified.

**Required change:** Add deterministic safety fixtures with independent source facts and forbidden-content canaries. Count unsafe text, unsupported/default-eligible relations, and unlabelled bot passages, not only artifact IDs. Report non-seed precision separately, define abstention and empty-set metrics, split expected results by endpoint, and require minimum judged denominators. Require nonzero executed coverage for each safety counter. Human checks of 5/20 retrieval cases and 10/50 edges must not be presented as full human verification of the remaining agent labels.

**Checkable gate:** Intentionally faulty responses with a hidden canary in an allowed summary, a missing bot prefix, a wrong source hop, and a silently shortened body must each fail the harness. ACL/forbidden-content assertions are zero-tolerance independently of aggregate relevance precision.

### 15. [must-fix] The promised baseline and verification gates are not reproducible from this handoff

**Plan:** Phase 0 and “Verification per round” (lines 119–143, 287–294).

**Missing inputs:** The 20 cases; approved labels; probes A–F; the A/B seed; source snapshot; actual baseline results; executable sampling/judging procedure; and a usable local-stack setup with the same graph data. None of the probe names identify a request. Production counts in the evidence table are observations, not reproducible fixtures.

`go test ./...` can pass while the relevant database tests skip: `handlers/helpers_test.go:31–36` skips without `DATABASE_URL`, and `handlers/testdb_test.go:36` has the same behavior. The helpers delete graph tables (`helpers_test.go:54–73`), so an implementer must be directed to an isolated migrated scratch database, never the hub.

The timing procedure also leaves request depth/budget/include_bodies, cold versus warm state, concurrency, embedding-cache behavior, timeouts, and grouping of query/no-query p95 unspecified. Resolve enqueues body fetches for cache misses (`handlers/resolve.go:425–429`), so repeated baseline requests can change the evaluated graph unless jobs/snapshot state are controlled.

**Required change:** Supply versioned fixtures, expected outcomes, baseline commands/results, scratch-DB setup and safety guard, exact evaluation parameters, sampling manifests, and human approval records before Phase 1. Define nonzero exit status on any failed gate and fail if required DB tests are skipped. If these are Phase 0 deliverables, explicitly state that autonomous handoff stops there until those deliverables and approvals exist; do not describe later phases as executable now.

**Checkable gate:** A new implementer can execute the documented commands against a clean isolated environment and reproduce fixture counts, counter failures, baseline metrics, and separately measured latency classes without discovering private thread IDs or asking what a probe means.

### 16. [should-fix] SAME_TOPIC post-filters need concrete identifier, incident, time, and missing-data rules

**Plan:** Phase 4 rules (lines 243–257).

**Evidence:** `handlers/identifiers.go:24–36,58–104` extracts payment/action refs, Jira keys, GitHub PRs, and UUIDs. It does not define PagerDuty incident IDs or Sentry error fingerprints. `shared_ids` is candidate metadata, not a field returned by the judge (`handlers/link_topics.go:790–799,865–867`), and some edge methods have no `shared_ids`. Activity gaps currently compare activity windows (`:612–666`), with non-Slack timestamps explicitly described as ingest-time approximations.

**Missing decisions:** Which tags require shared identifiers; whether an empty `shared_ids` passes vacuously; canonicalization/type matching; acceptable identifier rarity; exact alert-channel configuration; fingerprint extraction; raw versus normalized root length; gap between creations or activity windows; exact 30-day boundary; missing index/time/metadata behavior; and whether legacy evidence quotes must still match current content. Keeping the judge cache hash unchanged is reasonable only if fresh post-filters run on both cached and fresh positives and existing-edge sweeps.

**Required change:** Define one versioned, deterministic predicate shared by insert and sweep, its inputs, conservative missing-data policy, and reason precedence. Identify the configuration/extractor for incident identity and fingerprint, or explicitly withhold such pairs when evidence is unavailable. Preserve and validate the existing quote-evidence checks instead of treating confidence alone as proof.

**Checkable gate:** Identical input produces identical insert/sweep decisions for empty/stale IDs, wrong identifiers, bot-only identifiers, 199/200 characters, confidence 0.799/0.8, 30-day boundaries, alerts with the same symptom but different incident IDs, and missing activity data.

### 17. [should-fix] Removing reply index rows once does not prevent them from reappearing or preserve reply-only evidence

**Plan:** Phase 5 index coverage and reply removal (lines 268–273).

**Evidence:** `handlers/index_artifact.go:111–145` creates heuristic summaries even for non-root Slack messages; `:195–211` upserts their index rows. The root-only condition at `:222–226` governs linking, not indexing. Identifier extraction gives non-root messages no identifiers (`handlers/identifiers.go:144–149`). Keyword search works on indexed summary/identifiers and node title, not the entire thread body (`handlers/search_arms.go:219–227`).

**Failure:** A one-time deletion is undone by queued/future index jobs. Removing reply keyword rows can also remove the only searchable occurrence of a term omitted from a generated root summary. “0 unindexed roots older than 1 h” is undefined for deleted/excluded/DM/empty roots, pending or permanently failed fetches, and roots with an index row but no embedding.

**Required change:** Define durable writer and reader root-only rules, cancel/ignore queued reply indexing safely, and preserve desired reply-derived searchable evidence on the root. Specify the eligible root denominator and distinguish row coverage, summary coverage, identifier coverage, and embedding coverage. State error handling and scheduling needed to maintain the one-hour SLO after the one-off backfill.

**Checkable gate:** Live ingest, backfill, fetch, and reindex cannot reintroduce reply hits; a term/reference occurring only in a reply remains discoverable under the chosen contract; coverage SQL uses the same eligibility predicate as the indexer.

### 18. [should-fix] Privacy masking is not defined at the full output boundary

**Plan:** Phase 2 hydrate masking and Q2 (lines 198–200, 302–305).

**Evidence:** Hydration is used by resolve, but other routes read fields independently (`handlers/node.go:71–78`; `handlers/search_hybrid_display.go:128–177`). Resolve can append a bodyless seed title outside the hydration path (`handlers/resolve.go:407–424`). Relation `why`, title, URL, author fields, snippets, decisions, and legacy `links` are not included in the proposed summary/body masking rule.

**Failure:** A card number/email in a title or LLM explanation survives masking. “Phone numbers” and “card-like numbers” are too ambiguous to implement consistently alongside numeric booking/order references. A raw URL query may also contain the same sensitive value. The later bot Slack-output policy does not protect data already sent to the answering model.

**Required change:** Define a single output-field policy across safe model routes, precise masking/detection rules with examples and allowlisted identifier types, and ordering before token accounting and serialization. Specify URL handling and clarify that `content_is_untrusted` is an instruction/data label, not sanitization or a PII control. Resolve Q2 or supply a concrete default test corpus.

**Checkable gate:** The same sensitive canaries are masked in every designated field and route, while named payment/booking reference examples remain intact. Explicit admin exemptions are separately tested.

### 19. [should-fix] Freshness and truncation fields need semantics independent of fetch time and retained membership rows

**Plan:** Response fields, stale-scope warning, age, truncation, and latency gates (lines 96–111, 172–173, 193–194).

**Evidence:** `acl.Builder` caches only scopes and cache time (`internal/graph/acl/scopes.go:22–25,38–53`). Its freshness rule excludes only Slack C/G scopes, with up to a cache TTL of delay (`:60–72`), not every Slack scope. The refresh job physically deletes expired rows (`handlers/refresh_slack_members.go:139–149`), so querying old member rows cannot always distinguish stale access from never having membership. Fetching updates `nodes.updated_at=NOW()` (`handlers/fetch_body.go:180`), and current non-Slack activity windows use ingestion timestamps (`handlers/link_topics.go:612–639`). Hydration budgets bytes, not characters (`internal/graph/hydrate/budget.go:93,117–123`).

**Required change:** Define persistent refresh-health evidence and `scopes_stale` behavior after cleanup, partial refresh, cache hits, and lookup failures. Define unique-asker/hour counting and its no-traffic denominator. Specify source creation/activity versus fetched/indexed times, unknown values, `age_days` reference time/timezone, and source revision/freshness. Define `body_chars_full` as bytes or Unicode characters and whether it describes raw, composed, or masked text. Include summary/title/labels/relation overhead in a stated budget policy; currently only bodies are counted.

**Checkable gate:** Tests control the clock and exercise expiry with/without housekeeping and cache hits, a freshly fetched old artifact, multibyte text, and budget overhead. Refetching an old ticket must not make its source activity look current.

### 20. [should-fix] Several “must return” values are absent from the response contract

**Plan:** UC2 ticket status; UC6 incident outcome; UC8 assignees; Phase 2 `focus_ts` (lines 84, 88, 90, 186–187).

**Evidence:** `resolveArtifact` has no status, assignee, incident outcome, or focus field (`handlers/resolve.go:42–57`), and the new contract only adds the fields listed at plan lines 100–111. `artifact_index` currently selects `thread_summary` or a heuristic fallback (`handlers/index_artifact.go:111–137`), not a documented “thread_summary first, then gemini” lookup hierarchy. Index row presence does not prove that those required facts exist in its summary.

**Required change:** State whether each expected fact is a structured field or required textual content, where it is sourced, how unknown/missing values appear, and what freshness is required. Add `focus_ts` to the actual request/response schema and define multiple reply seeds in the same root. Do not require an incident outcome when the source has none; represent unknown explicitly.

**Checkable gate:** Schema/golden-response tests can assert these requirements without a grader guessing whether prose implies a status, owner, or outcome. Include missing/unknown and multiple-focus cases.

### 21. [should-fix] Resolve the deployment instruction conflict before handing this to an autonomous implementer

**Plan:** Verification step 3 follows CLAUDE.md and builds on the payments Mac mini (line 292).

**Evidence:** This is consistent with `CLAUDE.md:63–89`, which calls the VPS retired and forbids `make deploy`. But `AGENTS.md:48–66` says to build locally, push GHCR, use `make deploy`, and deploy to the VPS. `Makefile:89–116` contains both VPS and payments targets. These are contradictory checked-in instructions.

**Required change:** The plan must explicitly identify the authorized deployment target/procedure and acknowledge which conflicting guidance is stale, backed by an owner decision. Include migration order, embedded-dashboard rebuild, binary/version verification, safe rollback, and when the eval may be run against production. The reviewer did not verify which host is currently live.

**Checkable gate:** An implementer follows one unambiguous approved command sequence and can verify the exact running revision and schema version without choosing between contradictory instructions.

## File:line claim audit

These checks distinguish accurate code observations from unsupported runtime assertions; they do not independently certify the live counts.

- **F1:** `handlers/refresh_slack_members.go:245–248` is the production member-scope INSERT found in this repo. `aclVisibleSQL` is at `handlers/acl_guard.go:88–95`; the cited `:78–86` is the attachment-parent predicate/comment. Ordinary scopes use NULL/empty/public/exact-list visibility, but attachments have an additional visible-parent rule. Do not flatten that exception away when adding org scopes.
- **F2:** Absent-header behavior in `acl_guard.go:19–24`, resolve's zero-eeid bypass at `resolve.go:282`, and MCP omission at `graphmcp/server.go:174–182` are confirmed. The p-agent claims and roster counts were not verified. “Public only” is shorthand: the code also permits ordinary unscoped nodes, but unresolved askers cannot read protected attachment types.
- **F3:** Reply emission is supported by BFS/thread expansion and lack of root-level deduplication. The claim that a root's body always holds the thread is too broad: it depends on whether fetch or single-message ingest last wrote it (finding 8). Reply URL seed promotion already exists at `resolve.go:487–504`; Phase 2 must extend it, not invent a separate incompatible mapping.
- **F4:** `Sem: 0` at `resolve.go:566` and the 0.50 semantic default at `scoring/combine.go:50` are confirmed. Current scoring ignores the request query and does not apply an eligibility score threshold.
- **F5:** Consumers of `people.is_bot` exist at the named alert/card/pin/person locations (`alert_policy.go:166–176`, `thread_cards.go:57`, `pins.go:83`, `person.go:164`). The specific bot identity and live counts require runtime evidence. Migrations should resolve the configured Slack ID rather than assume `people.id=10` on every database.
- **F6:** `deps.Gemini.Generate` at `link_topics.go:783` and no confidence floor in `upsertSameTopicEdge` are confirmed. The code also validates quoted evidence against both source summaries at `:804–806,839–845`; the plan omits that existing safeguard. The 76% measurement and stored minimum are unverified here.
- **F7:** Normal resolve assembly leaves `summary`/`via` unset (`resolve.go:365–370`). Index coverage counts are unverified.
- **F8:** REFERS_TO method with no `source_msg_id` is confirmed at `link_topics.go:685–686`. Fetch reconciliation/pruning uses source IDs as cited. However, “REFERENCES source_msg_id is the from-node id” has a repository-visible exception: GH key search writes `gh_key_search` (`refresh_gh_key_search.go:535–538`), and `fetch_body.go:425–426` deliberately preserves it. `links` is tagged `omitempty` (`resolve.go:55`), so an absent relation is normally omitted, not literally `links: []`.
- **F9:** The supported identity columns and lack of numeric-eeid lookup are confirmed at `search.go:728–745`.
- **F10:** Silent budget truncation is confirmed, but the cut is at `hydrate/budget.go:117–123`, not `:40–45` (which sets up the fallback/iteration).
- **F11:** Age filtering is at `acl/scopes.go:68–73`; `:60–62` is commentary. It covers C/G Slack channel scopes, not DM/non-Slack scopes. Five-minute caching and expired-row deletion matter to the promised stale flag.
- **F12:** The reviewed output paths do not implement the proposed PII masking. “Bodies are raw Slack/Jira/GitHub text” should not imply no normalization: `handlers/fetch_body.go:87–99` invokes source normalizers before storing/extracting. Normalization is not privacy masking or prompt-injection neutralization. A repository-wide absence of every possible sanitizer was not proven.
- **Embedding facts:** `internal/llmgateway/client.go:381` documents `gemini-embedding-001` compatibility; `migrations/20260709000001_topic_linking.sql:11` sets `halfvec(3072)`. Reuse the existing embedder contract rather than merely matching the model name.
- **Slack profile:** The eeid join/response is confirmed at `handlers/slack_user_api.go:43–56,74–75`; this does not verify the claimed one-line p-agent fix or the proposed `/node` alias shape.
- **Dashboard:** The listed `api.ts` call sites exist, and `authFetch` is at `:11–19`. Rebuilding/resyncing embedded assets is correctly required.
- **Deployment:** The plan correctly quotes CLAUDE.md, but checked-in AGENTS.md contradicts it (finding 21).

## Acceptance criteria that are not presently checkable as written

- Phase 0's baseline and probe reproduction: requests, fixtures, labels, snapshot, and baseline are not supplied.
- Phase 1's “0 ACL-dropped seeds for an asker who can see the source”: no independent entitlement oracle, approved org grants, or stale-versus-revoked policy is specified.
- “Nothing from the hidden channel, not even a title”: node-ID intersection alone cannot check derived text or other model tools.
- Bot/truncation zero counts: the YAML schema contains no independent expected authorship/completeness/length facts.
- Phase 2 precision/recall: empty denominators, aggregation, seed inclusion, endpoint-specific expected sets, and query discovery are unresolved.
- Resolve p95 and harness under five minutes: request mix, database/index/cache state, external embedding behavior, concurrency, and timeout accounting are unspecified.
- REFERS_TO 100% of judgeable and SAME_TOPIC 90% strict: judging rubric, reproducible sample population, treatment of unjudgeable cases, and required minimum sample sizes are missing.
- Phase 5 “0 unindexed roots older than 1 h”: eligible roots and the meaning of indexed are undefined; one-off deletion/backfill does not establish continued compliance.
- Ticket status, incident outcome, assignees, and focus: no sufficient output schema/source contract exists.
- `go test ./...` green: it does not prove that ACL/database integration tests executed.

## Required handoff shape

Before autonomous implementation beyond Phase 0, revise the plan to include: approved identity/privilege/org-scope decisions; a complete model-route contract; dependency-aware derived-content authorization; message-level bot/reference provenance; durable rejection semantics; deterministic trust/path/ranking rules; reproducible fixtures and independent safety oracles; and a rollout barrier keeping existing consumers on no context rather than partially safe context. The repo already has useful ACL, quote-evidence, and source-checking patterns to build on, but the plan must explicitly connect them to the full output boundary.

**CHANGES REQUIRED**
