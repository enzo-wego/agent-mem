# Review: graph context Phase 0a, r3

**Verdict: CHANGES REQUIRED**

Reviewed the r3 plan, both prior reviews, the probe manifest, relevant parent-plan context, and repository code at `5d18336297b631f5cea3bb7c7cf81dcecc66cccc`.

This is a static handoff review, not an implementation or production audit. No source edits, tests, database queries, deployments, commits, issue updates, or external posts were performed. The requested review document is the only file written; the requested Herdr completion signal follows. Live hub state, available EnzoBot cases, credentials, and historical probe observations were not independently verified.

The narrowed scope is sound. Do not restore the seven-route safety harness, S1–S9, ACL changes, ranking changes, p-agent changes, or human label approval as prerequisites. Most r2 findings are answered. The remaining blockers concern the promised read-only preparation and the reliability of the no-write acceptance proof, not deferred product behavior.

## Numbered findings

### 1. [must-fix] The mutation-counter proof needs a backend-aware flush barrier

**Plan:** `docs/ai/2026-10-11-graph-context-phase0.md:258–265`; acceptance 5.

`pg_stat_force_next_flush()` requests a statistics flush for the backend executing it, not every backend that handled the preceding requests. Resolve and search use `*pgxpool.Pool` (`internal/graph/handlers/resolve.go:77–80`, `search.go:36–42`), and search runs retrieval arms concurrently (`search.go:213–260`). Calling the function through a separate observer connection, or once through the pool, does not establish that every request connection has published its pending statistics. Clearing an observer's snapshot does not flush other backends either.

The proposed positive controls can all pass on the observer/control connection while a forbidden mutation on another pooled connection is still invisible. Conversely, previously unflushed fixture writes can appear during the measurement. Thus the specified zero-delta check can falsely certify no writes, or fail for setup work. The stop condition about unreliable flushing is useful, but does not detect this false-success case by itself.

**Required revision:** Specify barriers before and after the measured requests that account for every participating backend, plus fresh observer snapshots. For example, use a dedicated request pool, flush/finish setup before the baseline, then close and await the request backends and poll fresh statistics with a bounded timeout; or choose an instrumented/read-only-transaction proof instead. State how flush completion is established, not just which SQL function is called. Run mutation controls through the same request-pool execution path, including a different connection from the observer. Setup, cleanup, trace-writer activity, and controls must have separate measurement windows.

**Check:** A deliberately injected UPDATE and balanced INSERT/DELETE on a handler connection distinct from the observer are detected reliably; an untouched request produces zero delta across repeated runs. A control executed only on the observer is insufficient.

**Confidence:** High on the backend-scope mismatch; no scratch-Postgres experiment was run during this read-only review.

### 2. [must-fix] Exporting canonicalization unchanged does not make it run inside the preparation transaction

**Plan:** `docs/ai/2026-10-11-graph-context-phase0.md:198–206,270–271`; acceptance 6.

The plan requires `eval prepare` to run its oracle reads in a read-only transaction and to call the existing canonicalization function, “exported for this use without changing it.” That function is a method on `Resolve`, whose `db` is a concrete `*pgxpool.Pool` (`internal/graph/handlers/resolve.go:77–80`). Its URL/root lookups use `h.db.QueryRow` (`:474–495`), and epic expansion uses `h.db.Query` (`:519–524`). It does not accept a transaction or a transaction-capable query interface.

Opening a transaction elsewhere and executing `SET TRANSACTION READ ONLY` does not redirect these pool calls into that transaction. Merely exporting the method therefore fails the stated contract. With a one-connection pool, holding the transaction while the method acquires another connection can instead block. This is a known repository integration gap, not something that needs live hub access to discover.

**Required revision:** Authorize a behavior-preserving extraction accepting a small query interface implemented by both `*pgxpool.Pool` and `pgx.Tx`, including epic expansion. Resolve passes its pool; preparation passes its read-only transaction. Alternatively, specify another concrete transaction-bound execution design. Explicitly distinguish direct oracle reads from the HTTP preparation replays: an HTTP replay does not inherit the CLI transaction and must independently use the no-side-effects flag. Define lookup/transaction errors as preparation failures rather than silently saving incomplete oracles.

**Check:** URL lookup, reply promotion, and epic expansion used by preparation demonstrably execute through the transaction; a write attempted through the same handle is rejected. Resolve's existing canonicalization results remain unchanged. An unrelated read-only transaction is not evidence.

**Confidence:** Verified from the current concrete database handle and call sites.

### 3. [should-fix] The new membership-error trace classification requires additional observability

**Plan:** `docs/ai/2026-10-11-graph-context-phase0.md:79`.

`askerScopeSet` returns only `(eeid, scopeSet, noFilter)` (`internal/graph/handlers/acl_guard.go:18–35`). A membership-build failure and a successful build with no memberships both return the same observable tuple: nonzero eeid, public-only scope set, false. Search receives only that tuple (`search.go:161–163`). It cannot distinguish `filtered-membership-error` from `filtered` using its existing variables.

Specify a behavior-preserving diagnostic result or hook from the helper's existing error branch, retaining a wrapper for other callers if convenient. Do not infer failure from scope-set size or rerun identity/membership queries. Include a successful-zero-memberships case and an injected builder failure in trace tests. This is implementable observability work, not a reason to change ACL behavior.

### 4. [should-fix] Search representation provenance is not the proposed single two-valued field

**Plan:** `docs/ai/2026-10-11-graph-context-phase0.md:92–93,122–134`.

Ordinary search does not use `hybridDisplay`: it returns node title and `artifact_index.summary` (`internal/graph/handlers/search.go:431–439,489–492`). The citation to `search_hybrid_display.go:138–173` on the ordinary row is therefore misleading.

Hybrid title can fall back to a node-body first line (`search_hybrid_display.go:138–139`), while thread summary and overview independently override title and summary (`:167–173`). A nonempty thread summary with an empty overview yields a thread-summary title and an artifact-index summary. One `display_source: node|thread_summary` cannot faithfully identify both sources.

Define separate title/summary source fields, or explicitly make this a coarse rendering-mode field rather than a source claim. Current-content comparison must use the ordinary or hybrid renderer appropriate to the original request. Add an untouched mixed-source item to the drift-label test. No historical snapshot or new display behavior is needed.

### 5. [should-fix] Specify relevance output when the first measured request fails

**Plan:** `docs/ai/2026-10-11-graph-context-phase0.md:233–242,272–277`.

Ranking metrics use the first measured run, but failed/timed-out runs are otherwise described only in the latency section. If run 1 times out and runs 2–10 succeed, the plan does not say whether ranking is unavailable or uses the first successful response. An error response must not become an empty successful result with zero recall, and a case with no successful runs must not silently disappear from the aggregate. Search can also return HTTP 200 with degraded arms; this is observable today through `arm_errors` (`search.go:175–195,398–400`).

Specify first-run failure handling, distinguish failed samples from legitimate empty results, and show the number of scored cases alongside `n/a` cases. Preserve degraded-search diagnostics in the saved result. Also say whether instability counts only successful ordered lists. This is measurement clarity, not a new relevance pass/fail threshold.

### 6. [should-fix] Baseline provenance must avoid a self-referential commit hash

**Plan:** `docs/ai/2026-10-11-graph-context-phase0.md:288–293`.

Commit 1 is required to contain the goldens, while those same goldens must record the SHA of the commit they were recorded on. That SHA cannot be embedded into its own committed contents. This does not invalidate the baseline approach, but a literal implementation of the handoff is impossible.

Record the baseline code/fixture in commit 1, then attach its SHA and goldens in a subsequent artifact-only commit; or store the recording SHA in a later provenance sidecar while keeping golden response bytes immutable. Require the two-process equality check over normalized response bytes, not a provenance envelope that differs by recording metadata.

### 7. [nit] Replace the linker placeholder with the actual symbol

**Plan:** `docs/ai/2026-10-11-graph-context-phase0.md:307–311`.

The build-argument solution answers the Docker stamping problem. Make the command unambiguous: for a string variable declared in this executable's `package main` (`cmd/agent-mem/main.go:1`), use `-X main.buildRev=${BUILD_REV}`. Substituting the module import path for the placeholder “main package path” risks targeting the wrong symbol. The existing exact-merged-SHA acceptance will catch a failed stamp; retain it.

## r2 disposition, finding by finding

1. **Answered, with a provenance clarification remaining.** The fixed clock seam, fixed fake embedder, no-tie fixture, and two-process baseline equality address the score/time/order problem. Finding 6 concerns how the recording revision is stored, not a demand to reopen the compatibility strategy. Fixture authors should pin settings and timestamps, including membership freshness, rather than use time-relative inserts.
2. **Answered.** Three variants per file preserve both resolve identities and the absent search identity header. Null omission, preserved `types`, prose-only variants, and per-variant aggregation are explicit. All six manifest primary requests fit the conversion. `hybrid_request.X-Asker-User: null` is transport metadata to omit, not a query parameter; the named-variant table already states that rule.
3. **Substantially answered; transaction integration remains finding 2.** Canonical seeds, metadata artifact, reply predicate, unknown metadata, and search seed exclusion are explicit. No best-effort trace is needed as an oracle. The direct DB execution boundary still needs a concrete adapter.
4. **Partially answered; finding 1 remains blocking.** In-memory caches are explicitly excluded and mutation counters improve on row counts. The proposed observer-side flush and controls do not yet establish visibility of all pooled request writes.
5. **Answered.** Docker/Compose changes are authorized; the current-Dockerfile stop condition is gone; the deployment command injects the revision and acceptance rejects unknown. Finding 7 is a small exact-command clarification, not the old deployment blocker.
6. **Answered.** Output/skip correlation, child-only variable removal, both DB variables, `agentmem_test`, serial uncached execution, pgvector, readiness, migrations, and trap cleanup are specified.
7. **Answered.** The hard gate is now an in-process paired benchmark covering resolve with/without query and both search modes. Hub timings are correctly observational round-trip timings; no production tracing-mode switch is required.
8. **Substantially answered.** Omission/empty distinction, returned byte length, truncation, deleted nodes, and honest current-content labels address the original concern. Finding 4 corrects the remaining search provenance model. The CLI must interpret `body_included` from the serialized response, not merely `include_bodies`: `resolveArtifact.Body` has `omitempty` (`resolve.go:52`), so even `include_bodies: true` does not serialize an empty body today.

## File:line claim audit

All plan paths abbreviated as `handlers/...`, `hydrate/...`, or `acl/...` were interpreted under `internal/graph/`.

- `resolve.go:282`: correct `noFilter` decision.
- `acl_guard.go:18–35`, including `:28–31`: correct helper/error branch; the error is not returned to Search (finding 3).
- `resolve.go:42–57`: correct artifact shape, including omitted empty body.
- `hydrate/budget.go:49–60`: correct title fallback query; `:121–124`: correct body truncation. `Hydrated` currently exposes neither title provenance nor an explicit truncation flag (`:20–28`); those require internal instrumentation, not invented response fields.
- `search.go:375–406`: correct hybrid and ordinary serialization regions; both use `results[]`.
- `search_hybrid_display.go:138–173`: correct hybrid fallback/override region, not the ordinary renderer. There is no returned per-item `arm_scores` field in `hybridResult` (`:14–41`); the plan's null-when-absent rule is appropriate.
- `resolve.go:433–535`: correct canonicalization plus epic expansion; concrete pool use prevents transaction binding by export alone.
- `ingest_content.go:45`: correct thread timestamp input. `:417` is the explanatory comment immediately before its persistence logic, a reasonable region citation rather than the assignment itself. `resolve.go:321` reads that metadata.
- `resolve.go:424–427`: correct body-fetch enqueue site; the enqueue writes `graph.jobs` at `:605–611`.
- `acl/scopes.go:37–53`: correct process-local cache fill, now explicitly excluded. Its SQL also uses PostgreSQL `now()` for Slack membership freshness (`:68–73`), which the Go clock seam does not freeze; fixtures must stay away from that cutoff.
- `resolve.go:546,565–575`: correct scoring clock and recency use. `search.go:53`: correct default injectable clock. `resolve.go:299–303`: correct non-tie-breaking sort. `resolve.go:67–73,348`: correct response timing field and assignment.
- `Dockerfile:6`, `.dockerignore:1`: correct unstamped build and excluded `.git`.
- `Makefile:42–44`: correct existing test target. `helpers_test.go:18`: correct exact database guard. `testdb_test.go:18` additionally allows `agentmem_test_keysearch`, but the aggregate target correctly selects the stricter shared name.
- `internal/graphmcp/client.go:177–185`: shared HTTP request construction/header site exists for adding the caller tag.
- D6 matches the payments target and command in `CLAUDE.md:63–83` and explicitly overrides the stale VPS guidance for this plan. No deployment-target dispute is reopened here.

## Acceptance disposition

1. **Checkable as specified.** Exercise both child-variable fault injections and preserve the Go test process exit status.
2. **Checkable with the small provenance correction in finding 6.** Compare normalized outputs while retaining status/content type, array order, and null/absent distinctions. Fixed clocks and no-tie fixtures answer the prior blocker.
3. **Checkable.** Await asynchronous persistence with a bounded deadline. Include empty-body omission and mixed-source search cases when implementing the stated labels; error responses remain plain text.
4. **Checkable.** Recorder on/off can be selected in-process. Writer lifecycle/failure tests and retention tests are separate from the p95 gate. No live timing threshold is required.
5. **Not a sound proof yet.** Finding 1 must be resolved; positive controls need the same backend visibility conditions as the handler requests.
6. **Blocked on the preparation execution contract in finding 2.** Probe conversion itself is now checkable. Findings 4–5 improve interpretation of the output rather than requiring more routes or better relevance scores.
7. **Checkable structurally.** Twenty files, all UC tags represented, and null human approvals are deterministic checks. Actual recent source availability is a runtime prerequisite, not verified here; do not fabricate missing real cases.
8. **Checkable after implementation and authorized deployment.** The build argument and controlled call resolve the prior known blocker. Verify the exact merged revision from the running container's trace, not the image-build log.

**CHANGES REQUIRED**
