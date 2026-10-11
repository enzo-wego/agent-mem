# Review: graph context quality, Phase 0

**Verdict: CHANGES REQUIRED**

Reviewed `docs/ai/2026-10-11-graph-context-phase0.md` against checkout `5d18336297b631f5cea3bb7c7cf81dcecc66cccc`, with the parent plan and previous review as context. This is a repository-backed plan review, not an implementation or production audit.

The narrower scope is appropriate. Independent canaries, non-seed relevance metrics, explicit expected failures, and tracing are meaningful improvements over the parent plan. Phase 0 need not implement ACL, trust, bot labeling, or rejection enforcement. However, the proposed handoff contains an impossible database-test configuration, an impossible literal response-comparison gate, incomplete trace semantics, and safety tests that can pass vacuously or conceal new failures. Those are Phase 0 problems, not requests to implement later phases.

No source files were edited. No tests, production queries, deployments, issue updates, commits, or external posts were performed. Only this requested review artifact was written; the requested Herdr completion signal follows. Runtime claims about p-agent being disabled, live data, labels, and the deployment target were not independently verified.

## Numbered findings

### 1. [must-fix] The prescribed scratch database cannot run the existing tests unchanged

**Plan:** “Database tests must actually run,” lines 187–193; acceptance 1.

The proposed database is `agentmem_eval_test`, but the current test helpers deliberately reject that name:

- `internal/graph/handlers/helpers_test.go:12–21` permits only `agentmem_test`.
- `internal/graph/handlers/testdb_test.go:12–21` permits only `agentmem_test` or `agentmem_test_keysearch`.
- `internal/graph/extractor/extractor_test.go:27–28` and `internal/graph/handlers/case_ref_sql_test.go:30–31` also require `agentmem_test`.

There is already a `make test-db` target at `Makefile:42–44`. It sets **both** `DATABASE_URL` and `AGENT_MEM_TEST_DATABASE_URL`, clears `AGENT_MEM_EVAL`, and runs graph packages serially with `-p=1 -count=1 -json`. The plan's replacement sets only the first variable and says `go test ./...`. Tests at `handlers/import_bamboohr_people_json_test.go:21–23` and `handlers/derive_person_roles_test.go:150–152` skip without the second variable. `handlers/testdb_test.go:31` explicitly warns that packages share tables and require `-p=1`.

**Required revision:** Extend the existing target rather than treating it as absent. Use an isolated container with the already-approved database names, or explicitly authorize updating every relevant guard and its tests. Preserve serial execution, both environment variables, disabled live-eval mode, and uncached execution. Specify migration/bootstrap, readiness, cleanup on failure, and a pgvector-capable image (`docker-compose.yml:3` uses `pgvector/pgvector:pg16`). Parse test JSON for actual database-related skips and preserve the test process's failure status. Do not weaken production safeguards to satisfy the naming requirement.

**Check:** A clean Docker environment runs the target successfully; both categories of DB tests execute; deliberately removing either required database variable makes the skip gate fail. Acceptance 1 cannot pass as currently written.

### 2. [must-fix] Literal before/after JSON equality is impossible, and the gate covers only two of seven routes

**Plan:** Non-goals; acceptance 2, lines 211–213.

Resolve already returns `graph_trace.took_ms` (`internal/graph/handlers/resolve.go:67–73`), populated from `time.Since(t0)` at `:348`. Removing only `request_id` does not make two legitimate executions identical. Search also uses a real clock (`handlers/search.go:53`), and querying a live hub before and after deployment does not hold graph contents, memberships, summaries, or caches constant. Suppressing this request's enqueue cannot stop background ingestion or refresh jobs.

The 20 relevance cases permit only resolve/search. Thus this gate cannot prove response preservation for node, neighbors, person, epic, cluster summary, either search response shape, or error paths. Search has separate hybrid and ordinary serializers at `handlers/search.go:375–406`.

**Required revision:** Separate deterministic compatibility tests from observational live baselines. Run old/new handlers against the same isolated snapshot, fixed clock and deterministic external dependencies, or define an explicit narrow comparison exception for already-variable timing fields without removing those fields from production responses. Specify ordered-array, null/omitted-field, status, and content-type comparison. Add fixtures for all seven routes, both search modes, empty/cache-miss responses, and representative errors. Record source revision, fixture/snapshot identity, parameters, and normalization policy with comparison results.

**Check:** The gate rejects an intentional body, ordering, status, or field-presence change but does not fail solely because the pre-existing timer changed. Do not change the route output to make the comparison pass.

### 3. [must-fix] “request_id on every response” conflicts with preserving existing error responses

**Plan:** Part A, lines 84–89; acceptance 3.

These routes use plain-text `http.Error`, not JSON error objects: examples include `handlers/resolve.go:175–182`, `handlers/node.go:99–108`, `handlers/person.go:91–99`, and `handlers/cluster_summary.go:124–126`. Converting those bodies to JSON to add a field changes more than an additive JSON property; appending JSON to plain text is not an additive field either. A success-only wrapper would miss the promised failed-call traces and omit a usable correlation ID for precisely the failures being debugged.

**Required revision:** Explicitly define success versus error behavior. For example, add the body field only to existing JSON success objects and carry the same ID in a response header on all handler responses, preserving error status/body/content type. Specify whether calls rejected before the handler (authentication, method/path routing) are in scope. Capture HTTP status in the trace; the present schema has only an ambiguous text `error`.

**Check:** Successful, invalid-input, forbidden, not-found, and handler-error requests have a trace/correlation contract without changing legacy error payloads. Include MCP error handling, which currently turns non-2xx bodies into errors (`internal/graphmcp/client.go:194–203`).

### 4. [must-fix] The trace schema has no defined mapping for most routes or for the ACL decision actually taken

**Plan:** Part A's `asker_raw`, `asker_eeid`, `acl_mode`, `request`, `items`, and CLI statistics.

The proposed item shape is essentially resolve's artifact shape (`handlers/resolve.go:42–57`), but other responses are not interchangeable:

- Node is one object plus `edges_in`/`edges_out` (`handlers/node.go:27–44`).
- Person returns profiles, manager chains, role evidence, and nested recent artifacts, with person IDs rather than node IDs (`handlers/person.go:45–84`).
- Cluster summary returns overview/highlights, nodes, edges, and a source-marker map (`handlers/cluster_summary.go:78–95`).
- Search has ordinary and hybrid representations (`handlers/search.go:375–406`).

It is unspecified what constitutes an item, how nested arrays are ordered/identified, whether edge/source references count, or how missing score/hop/link fields are represented. “0 non-seed items” is undefined for person and search and ambiguous for URL/reply seeds that resolve canonicalizes (`handlers/resolve.go:433` onward).

Identity behavior also differs today. Resolve uses body `asker_eeid` and ignores the header (`resolve.go:22–33,196,282`); header routes use `askerScopeSet` (`acl_guard.go:18–35`); person does not inspect asker identity in its handler (`person.go:87–139`); cluster denies every nonempty asker header (`cluster_summary.go:124–126`). A generic recorder that resolves a header and marks a call “filtered” can falsely certify a completely unfiltered person/resolve response. A denied or malformed request may have no ACL decision at all, yet the enum lacks that state.

**Required revision:** Supply a seven-route mapping: raw/effective request fields, identity inputs actually consumed, effective ACL mode (including denied/not-evaluated), response collections and JSON paths, seed definition, nullable field semantics, and statistics applicability. Record decisions from the existing handler, not a new policy computation. Capture both conflicting raw inputs where relevant without changing their current precedence. Define bytes versus Unicode characters for `body_chars`.

**Check:** Header-only resolve and person calls are not mislabeled filtered; denied cluster calls are not mislabeled successful filtered reads. Nested person artifacts and cluster sources can be located in a trace, and unsupported metrics display `n/a` rather than fabricated zeros.

### 5. [must-fix] Node IDs and lengths cannot substantiate the promise of “exact items … and why” later

**Plan:** Goal, lines 15–16; Part A's no-body policy and `trace show`.

The trace excludes returned titles, summaries, body contents, and any immutable content version/hash. `trace show` instead re-reads the current node. But current titles can come from `thread_summaries`, summaries from `artifact_index`, and bodies from `artifact_bodies` (`handlers/node.go:71–86`; `hydrate/budget.go:49–60`). They can change or disappear after the request. A root's current body does not establish which truncated prefix was actually returned. Cluster overview/highlights are synthesized output, not recoverable by reading each node (`cluster_summary.go:78–95`). Existing resolve links are not a complete explanation for every returned hop, so copying them cannot create absent provenance.

**Required revision:** Choose and state the achievable debugging guarantee. A metadata-only trace is reasonable for Phase 0, but label live hydration as **current, not historical** and record response/content digests, source versions where available, returned metadata, build revision, and missing-provenance markers sufficient to detect drift. If exact historical content is required, define an approved retained/versioned snapshot mechanism and its privacy policy. Do not silently claim IDs and sizes reconstruct historical content or why a hop was selected.

**Check:** Change/delete a node and regenerate its summary after a traced call; `trace show` must not present new content as the original response. Historical reconstruction is either demonstrably supported or explicitly reported unavailable.

### 6. [must-fix] The all-route safety assertions need an executable request/oracle matrix and positive controls

**Plan:** Safety fixtures S1–S9 and self-test, lines 106–130.

A tiny graph plus one prose assertion does not specify how to exercise seven different APIs. Person needs a person lookup with authored recent artifacts; epic needs membership/brief setup; search needs its index/query/mode; cluster needs its cache or model dependency. An asker-bearing cluster request currently gets 403 before graph traversal (`cluster_summary.go:124–126`). That can be a legitimate safe denial, but must not be reported as proof that cluster summary content was inspected.

The negative tests can pass on empty results, malformed requests, handler 500s, unavailable embeddings, missing fixture indexing, or omitted bot text. In particular, “every bot passage is prefixed” passes if no bot passage is ever returned, while S9's “error, not data” permits an unrelated validation/database failure. A deliberately faulty canned response proves an assertion can reject that response; it does not prove the real handler exercise reached the intended path. The named self-test example “wrong hop” has no corresponding specified fixture assertion.

**Required revision:** Specify fixture schema and each applicable route/request/status/response-path oracle, including normal versus hybrid search and explicit permitted-denial outcomes. Add positive controls that establish relevant data is reachable under an allowed/unfiltered counterpart and that the intended body/bot passage is exercised. Missing setup, unexpected statuses, malformed responses, unavailable dependencies, and zero executed coverage must be harness errors, never safety passes. Fix clock, embeddings/model outputs, job execution, and relevant cache states for synthetic tests; no real model/network call should decide determinism. Define exact length units and target assertions independently of response claims.

**Check:** Empty or 500 canned responses cannot pass a retrieval fixture; S9 accepts only the intended identity rejection; every declared fixture/route assertion executes. Include both passing and failing oracle self-tests so an always-failing validator cannot pass `selftest`.

### 7. [must-fix] S7 depends on a table absent from Phase 0, while fixture-level expected failures can hide regressions

**Plan:** S7; safety command and acceptance 5.

There is no `graph.edges_rejected` definition in the repository migrations (verified by search). The Phase 0 file list adds only `context_traces`; the parent defers rejection storage to later phases. An INSERT into that missing table is a setup failure, not evidence that today's route improperly returned a rejected edge. Inventing a production rejection schema in this phase would also prejudge a deliberately deferred design.

Separately, an expected failure keyed only to `S1` can forgive a new leak in node/person/epic just because resolve already leaked. The same problem applies when one assertion within S3 or S5 already fails. All future runs may remain green while safety worsens. Acceptance 5 also fails to distinguish explicitly generating an initial baseline from silently rewriting the allowlist during ordinary runs.

**Required revision:** Decide how S7 runs today without implementing later rejection policy: e.g. a documented fixture-side rejection oracle with a current active edge and a future adapter, or a separately reported unsupported capability that cannot count as measured pass/fail coverage. Pin expected failures at route/request/assertion granularity with reasons and baseline revision. Separate an explicit baseline-generation operation from normal verification; setup/runtime/schema errors must never be allowlisted. Specify unexpected-pass handling and fail on missing assertions or new failures.

**Check:** Adding a leak on a previously passing route makes the command fail even if another route in the same fixture remains expected-failing. A missing table or unavailable handler is not recorded as an ordinary expected safety failure.

### 8. [must-fix] `no_side_effects` has a known scope conflict with the stop condition

**Plan:** Side effects, lines 165–170; files list; final handoff stop condition.

Skipping resolve's enqueue is correctly motivated and its citation is accurate (`handlers/resolve.go:424–427`). However, the contract says “any other write except the trace row,” the harness always enables it, and the safety harness exercises other routes. Neighbors enqueues summary jobs and topic-link jobs (`handlers/neighbors.go:322,410`); cluster summary writes its cache (`handlers/cluster_summary.go:580–584`). Yet the file list limits the flag to resolve, and the worker is ordered to stop if honoring it requires shared paths beyond resolve. This is a foreseeable contradiction, not an unknown to discover during implementation.

**Required revision:** Either limit the no-write guarantee explicitly to live relevance's resolve/search calls, keeping isolated safety runs separate, or authorize its implementation on every invoked route with a concrete side-effect inventory and cache/model policy. Define body/header precedence, transport for GET requests, and whether external calls and in-memory caches are covered. Preserve ordinary request behavior.

**Check:** On a controlled database, before/after table and job snapshots show no disallowed writes for the promised routes, including cache misses. Do not infer read-only behavior just from sending an unimplemented header.

### 9. [must-fix] The required real requests, probe identities, and human approval are not available to the autonomous implementer

**Plan:** Relevance set, lines 132–142; acceptance 6–7.

Neither this plan nor the parent supplies exact probes A–F, their askers, or their complete parameters. The parent contains observations such as reply counts, not replayable requests. There are no supplied case YAML files defining them. “From EnzoBot threads of the last 30 days” does not identify the prior audit's six probes. The plan also requires a Fable labeller and five Enzo checks, but supplies neither an approved label artifact nor a mechanism/authorized handoff to obtain those checks without questions. An implementer must not fabricate `checked_by: enzo`.

There is also no request mapping from the case's Slack asker ID to resolve's existing **numeric body** `asker_eeid`; sending only `X-Asker-User` would evaluate resolve unfiltered today (see finding 4). Case schema omits search match mode/limit and other parameters despite trace and latency requirements referring to them.

**Required revision:** Attach the exact probe manifest or name an accessible authoritative artifact. Provide source acquisition and labeling instructions, complete endpoint-specific request schemas (including current identity translation and failure policy), UC tags, and a clear distinction between the 20 cases and six probes (included or additional). Supply approval records or explicitly end autonomous work at a draft-label artifact with human acceptance blocked. Do not make retrospective production/probe identities an inference exercise.

**Check:** A new implementer can replay every named probe and produce the intended filtered requests without inventing IDs, labels, approvals, or request parameters. Draft baseline results must remain distinctly draft; they do not satisfy the human-approval requirement.

### 10. [should-fix] Best-effort tracing needs measurable loss and lifecycle semantics

**Plan:** Part A's asynchronous writer, retention, and trace stats; acceptance 3.

“Every call” and “never … slows the request” are absolute claims incompatible with permitted drops and any instrumentation overhead. A bounded channel limits queued item count, not necessarily retained bytes when request queries/seeds or link explanations are large. The plan does not define queue capacity/limits, database timeout, independent write context, shutdown draining, drop-counter location, or how `trace show` distinguishes pending/dropped/expired IDs. Reusing the request context can cancel writes immediately after the response completes.

A single forced write-error test does not exercise full-queue behavior or establish that failures and latency statistics remain representative. Dropped traces bias stats, so the drop count must be visible alongside them rather than only in an unspecified log.

**Required revision:** State bounded best-effort coverage, size limits, timeout/shutdown behavior, and observable attempted/written/dropped/failed counts. Define latency boundaries and a tolerable overhead measurement rather than promising literally zero overhead. Add retention scheduling/indexes and tests for pruning only rows older than the cutoff. Bound or redact sensitive raw request/error/header capture deliberately.

**Check:** Slow/unavailable database, canceled request, saturated queue, shutdown, and 30-day boundary tests do not alter legacy responses or stall handlers beyond an explicit bound. A missing trace is distinguishable operationally from zero traffic.

### 11. [should-fix] Metric and sampling contracts still leave important results ambiguous

**Plan:** Metrics, latency, and `eval edges`.

The disjoint label sets, `n/a`, and unweighted aggregation are useful. Remaining gaps affect actual measurement:

- Non-seed exclusion must use canonical seed identities, not raw URLs/reply IDs; otherwise the same seed can inflate precision and recall. Define whether seed labels are prohibited or removed from the recall denominator as well.
- `duplicate replies = returned items whose node is a reply` requires an independent root/reply oracle. Neither resolve's artifact shape nor ordinary search provides a simple `is_reply` flag (`resolve.go:42–57`; `search.go:67–88`). Do not count every Slack message as a reply, or infer duplication from occurrence alone.
- Define whether metrics use one run or all ten runs and what happens when results vary. Specify failed/timed-out samples, p95 convention, minimum nonzero populations, and endpoint/query group sample counts. An endpoint with no cases cannot claim latency coverage.
- `--json` needs an output destination/schema, command failure rules, code/case/snapshot versions, and observed request IDs. Edge sampling needs a data source/credential policy, stable ordering before seeded sampling, manifest input/output behavior, treatment of missing/deleted edges, and a judging rubric. A manifest pins IDs, not body versions.

**Check:** URL and reply seeds cannot inflate metrics; no-traffic/all-`n/a` groups are conspicuous; failures cannot disappear from timing samples; a saved manifest and snapshot reproduce the edge sample.

### 12. [should-fix] The deployment gate still requires an explicit resolution of contradictory repository guidance

**Plan:** Acceptance 8, lines 225–227.

The plan names the payments Mac mini, consistent with `CLAUDE.md:63–83`. But `AGENTS.md:48–66` requires the VPS/GHCR flow and `make deploy`. Both targets remain in `Makefile:89–116`, and the payments path there differs from CLAUDE.md's example. Naming CLAUDE.md alone does not explicitly mark the other mandatory instructions stale or authorize the implementer to override them.

**Required revision:** Record the owner-approved target and override of stale guidance, exact host/directory and command sequence, migration order, how the running binary's revision is proven, and rollback behavior. Distinguish a controlled real HTTP/MCP call from waiting indefinitely for organic traffic; p-agent is intentionally off, so its traces cannot be the required source of traffic. Do not deploy during this review.

**Check:** The implementer has one authorized deployment path and can attach running revision/schema evidence plus trace IDs without choosing between conflicting instructions.

## File:line claim audit

The Phase 0 plan contains only a few explicit code-line citations:

- `handlers/resolve.go:425–429`: substantially correct. The enqueue loop is `:425–427`, preceded by its comment at `:424`; `:429` sets the JSON content type.
- `handlers/helpers_test.go:31–36`: correct for the missing-`DATABASE_URL` skip. However, `:18–19` now enforce the exact scratch name, which materially changes the proposed setup.
- `handlers/testdb_test.go:36`: correct for the missing-database skip; `:18–19` restrict database names, and `:31` documents serial package execution.
- “Add a `make test-db` target”: stale as a repository claim; it already exists at `Makefile:42–44`.
- MCP HTTP forwarding is confirmed by `internal/graphmcp/client.go:75–149,180–188`; successful response maps can carry additive fields. This does not prove every external typed consumer's compatibility.
- The listed resolve/search/node/neighbors/person/epic/cluster-summary handlers exist. Their output and identity contracts are not uniform; findings 3–4 identify the concrete differences.
- `graph.edges_rejected` is absent from current migrations. No production existence claim is made.
- CLAUDE.md does identify the payments Mac mini, but conflicts with AGENTS.md as described above. Live target status was not checked.

## Acceptance criteria disposition

1. **Blocked as written:** scratch name contradicts unchanged tests; second database variable and serial execution are missing.
2. **Cannot be checked literally:** existing timing output varies, live state is uncontrolled, and only two routes are compared.
3. **Partly checkable:** success trace existence and forced write failure can be tested, but errors, item mappings, async waiting, and loss behavior lack contracts.
4. **Insufficient proof:** faulty canned responses need paired passing controls and real execution coverage; “wrong hop” lacks an oracle definition.
5. **Blocked/unsafe as written:** S7 lacks a setup contract; fixture-level allowlisting can hide regressions and harness failures.
6. **Not reproducible yet:** exact cases/parameters/identity mapping and baseline state are absent; no-side-effects scope is contradictory.
7. **Externally blocked:** human approval cannot be created by an autonomous implementer. Draft results are acceptable only as explicitly incomplete acceptance.
8. **Needs an authorized procedure:** conflicting instructions, version/schema proof, and the real-traffic check require explicit resolution.

These revisions should keep Phase 0 observational. None requires changing today's ACL decisions, ranking, bot treatment, or rejection enforcement. The necessary change is to make the traces honest, the tests executable, and future safety regressions distinguishable from today's known failures.

**CHANGES REQUIRED**
