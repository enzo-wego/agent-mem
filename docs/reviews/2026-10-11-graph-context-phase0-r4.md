# Review: graph context Phase 0a, r4

**Verdict: APPROVED**

Reviewed the seven findings in `docs/reviews/2026-10-11-graph-context-phase0-r3.md` against r4 and the affected repository paths at `5d18336297b631f5cea3bb7c7cf81dcecc66cccc`. Scope was the r4 changes, not a fresh review of the whole plan. No r1/r2 finding that r3 accepted was reopened.

This is approval of the implementation plan, not evidence that its future tests or deployment pass. No source changes, database queries, application tests, deployments, issue updates, commits, or external posts were performed. A temporary coverage-validation file was removed; this report is the only retained file written.

## Numbered findings and dispositions

1. **r3 finding 1 — resolved.** The proof at plan lines 272–291 no longer depends on asynchronous statistics visibility. Each connection in the dedicated request pool is made read-only by `AfterConnect`; setup and trace writes use a separate pool. The enqueue fake independently verifies suppression, including the unflagged cache-miss control. Constructors propagate the supplied pool to the ACL builder and BFS expander (`internal/graph/handlers/resolve.go:84–95`, `search.go:47–55`), so this is a workable integration design. Acceptance 5 explicitly requires zero write errors as well as zero enqueues.

   Implementation must observe database errors even when application code discards them, rather than equating HTTP success with zero write errors. For example, `resolve.go:602–610` discards the enqueue `Exec` error. A connection-level query tracer/error collector can satisfy the stated requirement without changing response behavior. The dedicated pool prevents the forbidden persistent writes regardless; the collector establishes the stronger zero-attempt assertion. This is an implementation obligation already required by r4, not a remaining statistics-barrier defect.

2. **r3 finding 2 — resolved.** Plan lines 203–215 authorize the needed `Query`/`QueryRow` extraction, pass the preparation transaction explicitly, and separate HTTP replays from direct oracle reads. The cited concrete pool and query sites are accurate: `resolve.go:77–80`, `473–495`, and `519–524`. The extraction covers epic expansion as well as URL lookup and reply promotion. Preparation fails rather than saving a partial oracle on database/transaction failure.

   Preserve the existing normal no-match semantics: `pgx.ErrNoRows` at an optional URL/root lookup is not a database outage. Resolve currently skips an unmatched URL and retains an unpromoted Slack id (`resolve.go:473–503`); unknown epics expand to nothing (`508–535`). Diagnostics can be returned alongside the existing best-effort result so preparation rejects actual failures while Resolve keeps its current behavior. No unrelated transaction is being offered as proof anymore.

3. **r3 finding 3 — resolved.** Plan line 79 explicitly adds `askerScopeSetDiag`, retains the wrapper for other callers, and requires both successful-zero-membership and builder-failure tests. `internal/graph/handlers/acl_guard.go:18–35`, especially `28–31`, confirms that this is the exact missing diagnostic seam. The proposal does not infer errors from membership count or change ACL decisions.

4. **r3 finding 4 — resolved.** Plan lines 92–93 correctly distinguish ordinary search from hybrid rendering and give hybrid title and summary independent provenance. Ordinary title/summary come from `search.go:431–439,489–492`; hybrid body fallback and independent thread overrides are at `search_hybrid_display.go:138–139,167–173`. A thread-summary title with an artifact-index summary is now representable. Plan lines 120–129 require the original route/mode renderer for current-content comparison. The response-level `arm_errors` references at `search.go:175–195,398–400` are accurate; hybrid serialization also carries them at `381–382`.

5. **r3 finding 5 — resolved.** Plan lines 247–256 select the first successful measured response, keep failed cases separate from real empty results, print scored/n/a/failed counts, retain per-run arm errors, and restrict instability to successful runs. This makes the first-run-timeout case and the all-runs-failed case checkable without inventing relevance thresholds.

6. **r3 finding 6 — resolved.** Plan lines 312–319 put immutable goldens in commit 1 and its recording SHA in commit 2's provenance sidecar. Equality is explicitly over normalized response bytes, not a changing provenance envelope. There is no self-referential hash requirement.

7. **r3 finding 7 — resolved.** Plan lines 333–337 now name `-X main.buildRev=${BUILD_REV}`. `cmd/agent-mem/main.go:1` is `package main`; the plan explicitly authorizes adding the string variable. `Dockerfile:6` is indeed the current unstamped build. The exact deployed merged-SHA acceptance remains intact.

## Delta conclusion

No new blocking correctness, safety, or acceptance-checkability problem was found in r4.

One harmless wording inconsistency remains: “every Part C request … all succeed” at line 282 includes Part C's deliberately invalid 400 request. Read this as all test cases passing their expected status/body assertions, not every HTTP response becoming 2xx. Part C and the unchanged-response requirement already make the intended assertion clear; do not alter the invalid-request response to satisfy that sentence.

The no-write test and canonicalization error handling above deserve implementation attention, but neither requires another plan-review round or broader scope.

## Review receipts

- graph: not-applicable: documentation-only plan delta; cited code traced manually.
- fan_out: inline: Round-N delta.
- Static verification covered all seven r3 dispositions and the file:line claims changed by r4. No production-state claims were independently verified.
- Coverage validator: `OK: coverage valid`.

**APPROVED**
