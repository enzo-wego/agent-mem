# Review: graph context Phase 0a, r2

**Verdict: CHANGES REQUIRED**

Reviewed r2, the r1 review, the probe manifest, and repository code at `5d18336297b631f5cea3bb7c7cf81dcecc66cccc`. This is a static, repository-backed handoff review. No source edits, test execution, production access, deployments, commits, or external posts were performed. This requested review artifact is the only file written; the requested Herdr completion signal follows.

The narrowed scope is appropriate. Do not restore the seven-route safety harness, rejected-edge fixtures, ACL changes, or human label approval as prerequisites. r2 substantially answers r1, but its compatibility proof still cannot pass reliably, the supplied probes do not fit its executable case schema, and several acceptance gates do not prove their stated guarantees.

## Numbered findings

### 1. [must-fix] Compatibility goldens still vary beyond the permitted timing field

**Plan:** Part C, lines 217–234; acceptance 2.

Normalizing `graph_trace.took_ms` and faking search embeddings is insufficient. Resolve computes `now := time.Now()` in `internal/graph/handlers/resolve.go:546` and returns recency-dependent breakdowns and scores (`:565–575`). Search's injectable clock defaults to `time.Now` (`search.go:53`), drives windows (`:152–153`), and affects returned recency (`:476`). Consequently, the same fixed fixture produces different score JSON on the parent and feature commits. Resolve also gathers candidates from a map (`resolve.go:284`) and sorts only on strictly greater scores (`:299–303`); tied fixture scores can produce different ordered artifacts. Sorting the comparison would conceal the ordering change that this test is meant to catch.

**Required revision:** Pin the scoring/window clock in both baseline and feature executions, including how the parent-commit harness obtains that seam without modifying production behavior. Specify fixed fixture timestamps/settings/memberships and either avoid score ties explicitly or document how existing nondeterminism is controlled without changing production ranking. Pin the baseline commit and record the harness/fixture identity with the goldens. Require repeated baseline runs to produce identical normalized responses before accepting the goldens.

**Check:** Repeated old-code and new-code runs at different wall-clock times have identical normalized outputs; body/order/status/presence mutations still fail. This is the unresolved portion of r1 must-fix 2, not a request to cover deferred routes.

### 2. [must-fix] The six probes cannot be copied exactly into the proposed case schema

**Plan:** Cases, lines 130–163; acceptance 7; probe manifest.

Each manifest probe defines **two** resolve requests via `resolve_request.asker_eeid: [0, real_eeid]`, plus an unfiltered hybrid request with `types` and **no** `X-Asker-User` header. The proposed case has one `resolve.body`, one `search.params`, and one labels object per endpoint. It neither represents both resolve variants nor specifies which to discard. Its example supplies the real search asker instead of the manifest's absent header. `budget_tokens: null` in the manifest explicitly means omit the field; a verbatim copy is not an HTTP request. Manifest `variants` are prose, not complete independent request records. Six probe files plus 14 cases cannot simply become additional top-level cases without violating the prescribed 20-file count.

**Required revision:** Define a mechanical manifest-to-case conversion: named request variants within each probe file, exact omission/header/type semantics, which optional audit variants are included, and labels/results/request IDs keyed by variant. State whether aggregates weight each request variant or each case, so probes do not silently receive double resolve weight. Preserve the primary two resolve calls and primary hybrid call if 'copied exactly' remains the requirement.

**Check:** A dry-run prints the exact replay HTTP requests for all six probe IDs and matches the manifest's primary requests without inventing a selected identity or dropping a variant. This is the remaining portion of r1 must-fix 9; supplying the manifest and making labels explicitly draft were good fixes.

### 3. [must-fix] Canonical seed and reply classification lack a complete eval data contract

**Plan:** Case schema, reply oracle, and metrics, lines 140–186.

Metrics require 'the case's canonical seed ids', but that field is absent from the schema. The ordinary resolve response's `graph_trace.seeds` contains the original request strings (`resolve.go:344`), not canonical IDs. Canonicalization includes URL resolution, reply promotion, and epic expansion (`resolve.go:433–535`), so merely parsing URLs in the eval client is insufficient. A best-effort trace is not a reliable oracle: it may be dropped, expires, caps arrays at 50, and is absent in the tracing-off run.

The reply predicate is discoverable: Slack IDs contain the own timestamp and ingestion stores `metadata.thread_ts` (`handlers/ingest_content.go:45,417`); resolve reads that metadata at `resolve.go:321`. However, specifying a predicate in the plan does not say how the base-URL-only eval gets it for returned ordinary-search items. `searchResult` has no thread timestamp (`search.go:67–84`). The command specifies an API key, not a hub DB connection or an exported node-metadata oracle.

**Required revision:** Choose an explicit acquisition path for canonical seeds and reply metadata, usable with tracing off and without requiring a retained trace. For example, persist authoritative canonical seed IDs and a versioned reply-ID oracle with case preparation, with defined treatment of previously unseen results; or supply a read-only metadata lookup contract. Specify null/missing thread timestamps, non-Slack nodes, missing/deleted nodes, and lookup failures. Define whether a search call in a combined case excludes the resolve call's seeds too. These are measurement rules, not ranking changes.

**Check:** A reply permalink, a non-Slack URL, and an ordinary-search reply can be scored correctly with tracing disabled; missing oracle data is reported, not silently counted as non-reply/non-seed.

### 4. [must-fix] Equal row counts do not prove the no-write contract, and cache writes are real

**Plan:** No side effects, lines 197–207; acceptance 5.

The new flag's stated scope includes cache writes. Both handlers call `acl.Builder.For`, which writes an in-memory cache on a miss (`internal/graph/acl/scopes.go:37–53`). This is an actual additional write beyond resolve's job enqueue, not a hypothetical deferred-route issue. Equal table counts cannot detect this cache write, UPDATEs, or balanced INSERT/DELETE operations. Acceptance 5 can therefore pass with the contract violated.

**Required revision:** Explicitly decide whether process-local caches are excluded from the no-side-effects guarantee or implement a non-populating lookup for the flag. Keep the default path unchanged. Replace row-count-only proof with a mechanism that detects forbidden mutations, such as instrumented mutation/job/cache hooks with positive controls and/or read-only request DB transactions plus a separately scoped trace writer. Enumerate the inspected resolve/search dependencies. Retain counts as supplemental evidence, not the proof.

**Check:** An injected UPDATE of an existing row, a job enqueue, and (if included) an in-memory cache write each fail the proof. No normal-path behavior change is required. r1 must-fix 8's seven-route conflict is resolved; this finding concerns only the newly promised two-route proof.

### 5. [must-fix] The revision/deployment acceptance has a known Docker build blocker

**Plan:** `build_rev` at line 72; acceptance 8; stop condition at line 285.

The current Dockerfile simply runs `go build` (`Dockerfile:6`). `.dockerignore:1` excludes `.git`, so automatic Go VCS stamping cannot supply the merged revision inside that build. No existing `build_rev`, `vcs.revision`, or linker-flag mechanism was found in `cmd`, `internal`, or the Dockerfile. The plan says the worker adds it if absent, but also says to stop if it cannot be baked with the **current Dockerfile**. That condition is already true for the documented build path; silently reporting a runtime environment value is not proof of the compiled revision.

**Required revision:** Authorize and name the build plumbing changes: a revision build argument or generated build-info input passed into the image build and compiled into the binary, populated from the intended merged commit. Update the documented deployment command accordingly; refuse an empty/unknown revision for acceptance. Remove the contradictory current-Dockerfile stop condition or explicitly classify deployment as blocked rather than an autonomous completion gate.

**Check:** A container built by the prescribed command, with `.git` excluded, reports the exact expected revision in a controlled trace. D6 sufficiently resolves the old target dispute; this does not reopen the VPS choice.

### 6. [should-fix] The skip detector and variable-removal acceptance are specified incorrectly

**Plan:** Test database, lines 236–245; acceptance 1.

Approved DB names, both variables, and serial execution now correctly match the repository. However, Go test JSON puts the skip reason in `Action: "output"` records and the terminal `Action: "skip"` event separately. Looking for a skip event 'whose message mentions' the variable can miss the failure. Also, the target sets both variables from `TEST_DATABASE_URL` (`Makefile:42–44`), so unsetting either in the caller's environment does not make the target fail.

**Required revision:** Correlate output and skip records by package/test, preserve the test process exit code, and define a deliberate fault-injection mechanism that removes a variable from the child test process after target setup. Use `agentmem_test` for the aggregate target because `helpers_test.go:18` does not accept `agentmem_test_keysearch`. Define cleanup of a target-created container on success/failure.

**Check:** A known test skipping for either missing DB variable is detected by the wrapper, and unrelated intentional skips remain permitted. Do not weaken the DB guards.

### 7. [should-fix] The latency procedure does not isolate or reliably enable/disable server tracing

**Plan:** Writer overhead and latency, lines 103–105 and 188–195; acceptance 6.

`AGENT_MEM_TRACE=off` must affect the serving worker, not merely the CLI calling `--base-url`. No procedure says how to restart or provision the hub with that setting, prove which mode served each sample, or restore it. The +5 ms bound is stated as **handler** p95, whereas an HTTP CLI measures client round-trip latency; tracing-off removes the trace-based timing source. Sequential passes against an explicitly changing hub also confound trace overhead with graph changes, model latency, and cache state. 'Resolve without query' has no mandatory case allocation; all six probes have queries.

**Required revision:** Define the actual measured interval, server-mode switching/verification and restoration, and required query/mode sample coverage. Prefer a paired controlled scratch-fixture benchmark for the hard overhead gate, while reporting hub timings as observational. If the live gate remains, prescribe repeat/uncertainty handling rather than attributing every difference to tracing.

**Check:** The report demonstrates that both worker modes ran, states whether measurements are handler or round-trip, and can distinguish a noisy live result from an instrumentation regression.

### 8. [should-fix] `trace show` can report content drift when only representation differs

**Plan:** Item digests and CLI, lines 89–96 and 112–119.

Resolve commonly omits bodies (`include_bodies: false` in every primary probe) and can return a truncated body (`hydrate/budget.go:121–124`). Hashing a current full body against an absent/returned-prefix digest marks an unchanged source as changed. Current titles also need route-specific rendering: resolve uses thread-summary fallback (`hydrate/budget.go:49–60`); hybrid search may replace title and summary from thread summaries (`search_hybrid_display.go:138–173`). Reading `nodes.title` alone does not reconstruct either representation. Search records a summary digest, but the CLI instruction only mentions title/body hydration.

**Required revision:** Define absence versus empty hashes and compare like representations, or label the difference explicitly as 'current full content differs from returned representation; historical change unknown'. Record enough representation metadata to compare returned prefixes if desired. Define missing/deleted current nodes and summary comparison. The existing 'current, may differ' warning is correct but does not make a categorical `changed` label accurate.

**Check:** An untouched node with omitted/truncated response body is not falsely declared historically changed; regenerated summaries and deleted nodes produce honest diagnostics.

## r1 disposition, by finding

- **1 (must-fix): substantially answered.** Existing target, names, both variables, serial execution, and pgvector retained. Remaining gate details are finding 6 above.
- **2 (must-fix): partially answered.** Deterministic fixtures and timing normalization replace live equality; deferred routes explicitly excluded. Scoring clocks and tie behavior remain unresolved (finding 1).
- **3 (must-fix): answered.** Header on all handler responses, additive JSON success field, unchanged plain-text errors, and pre-handler exclusions are explicit.
- **4 (must-fix): substantially answered for narrowed scope.** Resolve/search item mapping and handler-derived ACL identity are specified; node/person/epic/neighbors/cluster are explicitly deferred. Do not demand their mappings. Clarify search `public-only` as unresolved identity versus a resolved asker with only public memberships: `askerScopeSet` returns an eeid even when building memberships fails (`acl_guard.go:28–31`).
- **5 (must-fix): answered in principle.** Metadata-only/non-reconstruction guarantee, revision, hashes, and current-content warning are explicit. Finding 8 concerns correctness of the remaining drift claim, not historical snapshots.
- **6 (must-fix): explicitly moved out by name.** S1–S9 and self-tests are Phase 0b.
- **7 (must-fix): explicitly moved out by name.** Rejection fixtures and expected-failure machinery are Phase 0b.
- **8 (must-fix): scope contradiction answered.** Only resolve/search are promised read-only. Their actual proof remains insufficient (finding 4).
- **9 (must-fix): partially answered.** Manifest exists, identity placement is explicit, source DB/table is named, and human checking is outside autonomous acceptance. Replay conversion remains missing (finding 2).
- **10 (should-fix): substantially answered.** Bounded queue bytes/count, timeout, independent context, drain, counters, retention, and tests are specified. Best-effort loss should still qualify 'every call'; truncated rows must not silently count as complete item populations in stats.
- **11 (should-fix): largely answered.** Formulas, first-run metrics, instability, timeout reporting, quantiles, output artifact, and no-coverage handling are specified. Oracle transport remains finding 3. Edge sampling is absent from this resolve/search-only r2; no request to restore it.
- **12 (should-fix): target conflict answered by D6.** CLAUDE.md supplies the host/path/deploy command and controlled curl replaces organic traffic. Revision build plumbing remains finding 5.

## File:line claim audit

All paths abbreviated as `handlers/...` in the plan resolve under `internal/graph/`.

- `resolve.go:282`: correct, `noFilter := req.AskerEEID == 0`.
- `acl_guard.go:18–35`: correct helper region; the return tuple, not membership-set size alone, must define trace classification.
- `resolve.go:42–57`: correct artifact shape.
- `search.go:375–406`: correct hybrid and ordinary serialization region. Both lists are `results[]`; ordinary has `score_breakdown`, hybrid also has it. No top-level per-item `arm_scores` field was found in `search_hybrid_display.go:14–43`; recording null when absent follows the plan.
- `resolve.go:424–427`: correct fetch-body enqueue region.
- `resolve.go:67–73,348`: correct trace type/timing assignment; incomplete as an inventory of time-dependent output (finding 1).
- `Makefile:42–44`: correct existing test-db recipe.
- `helpers_test.go:12–21` and `testdb_test.go:12–21`: correct guards, but their accepted-name sets differ as noted above.
- `AGENTS.md:48–66`/D6: the provided project instructions describe the VPS deployment; CLAUDE.md:63–83 explicitly describes the payments Mac mini. D6 names the override rather than leaving the target unresolved.
- Probe primary requests are present, but their multi-variant schema differs from the plan. Probe-f explicitly documents its different eeid source, overriding the manifest introduction's generalized person-lookup claim.
- The exact historical count '37,078 of 42,285' and p-agent/runtime/deployment assertions were not verified against live systems. Repository code supports a reply predicate, not those historical counts.

## Acceptance disposition

1. **Implementable after clarification:** DB setup is corrected; skip parsing and fault injection need finding 6.
2. **Blocked as written:** uncontrolled score clocks/ties defeat deterministic equality (finding 1).
3. **Checkable:** await asynchronous persistence with a bounded timeout; preserve error bytes/status/content type. Unknown-ID diagnostics are specified.
4. **Checkable but weak performance evidence:** the 2-second ceiling is a failure-mode test, not proof of the separate +5 ms p95 bound. Define accounting of oversized/failed/shutdown entries consistently.
5. **Insufficient proof:** equal row counts cannot establish no mutations (finding 4).
6. **Not reproducible as specified:** probe variants, metadata oracles, and server tracing-mode measurement need findings 2, 3, and 7. Draft labels are an acceptable explicit limitation.
7. **Count/tag/null checks are checkable:** the manifest conversion must first define how 20 files represent all required requests. No Enzo approval should be fabricated.
8. **Known build-path blocker:** implement revision stamping explicitly (finding 5); a stop-and-report clause is honest but does not make this completion criterion achievable.

**CHANGES REQUIRED**
