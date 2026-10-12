# R1a lead mode — plan review R2

**Verdict: CHANGES REQUIRED**

Reviewed draft r2 of `docs/ai/2026-10-12-graph-context-r1a-lead-fields.md` against the supplied R1 review and repository source. References below use the current plan's line numbers.

## Scope and evidence

- Local HEAD: `55d90dc49a06065999a47af7200d1d2fac6c3d7a`. Its only difference from the plan's `8e97d74` base is `.beads/config.yaml`; inspected application source matches the named base.
- Read-only local source investigation. No hub, network, database, builds, tests, deployment, commits or pushes. Only this review file was written. No secrets or customer data were inspected.
- Findings concern implementable plan contracts, not an assertion that an unwritten implementation has already failed. SQL/schema and output-path defects below are verified statically; no runtime or latency result is claimed.
- `graph: not-applicable: documentation-only re-review; existing implementation traced manually`
- `fan_out: inline: Round-N delta`
- Manual ACL/masking review; no dedicated security scanner. No temporary ledger/coverage files were created under the user's file-write restriction.

## F1–F8 disposition

| R1 finding | R2 disposition | Evidence / remaining work |
|---|---|---|
| F1 — authorization after remapping | **Partially resolved; blocking** | A2.4 checks new roots and fails closed, and A2.8 checks original link endpoints. A3/A4 still allow thread-summary enrichment of retained replies without that boundary; A5 still lacks the live Slack-type/visibility restriction requested in R1. See F1 below. |
| F2 — mask before cap | **Resolved in the plan** | A4 explicitly masks full strings before a rune cap including the ellipsis; admin behavior and mounted boundary tests are specified. Search's final summary is covered by A3. Keep this lead-only. |
| F3 — groups, links, score/hop/focus | **Resolved in the plan** | A2 preserves reply-seed focus, authorized group membership, winning score/breakdown, seed hop precedence, deterministic ranking and mapped stored edges; self-links and synthetic THREAD links are excluded. Acceptance limits the no-reply-ID assertion to artifacts/links. |
| F4 — both search contracts | **Resolved in the plan, subject to F1** | Both serializers are named; enrichment is final-page; hybrid ineligible-root members are dropped; existing summary semantics are preserved and `lead_summary` has a fallback. Default search's remaining root-enrichment ACL gap is F1, not a second F4 finding. |
| F5 — misses and selection | **Partially resolved; blocking** | Seed exception, ordering, limit range and token meaning are specified. The concrete replacement miss query references a nonexistent column. See F5 below. |
| F6 — aggregate cost | **Resolved as a plan-level access-path fix** | A5 specifies paired inputs, matching expression index, combined aggregates, selection placement, context and row lifecycle. Acceptance adds representative volume, EXPLAIN, count and cancellation checks. This is not measured proof of a latency bound. |
| F7 — bot provenance | **Resolved in the plan** | Explicit enums, generic/list bots, root-inclusive thread semantics and loader failure are defined. Summary text attribution is explicitly deferred, not claimed delivered. Root-denied field representations still need the F1 clarification. |
| F8 — content-safe eval and no writes | **Partially resolved; blocking** | Mounted recorder/store tests, draining, fetch suppression, header override and request IDs address the trace boundary. `--quiet` still allows URLs through the existing summary error path. See F8 below. |

## Remaining blockers from R1

### F1 — HIGH: root authorization does not cover all new thread-derived fields

**Plan:** lines 124–142, 154–158; acceptance 3.

A3 protects a node-by-reply's **root author and aggregate**, but not its new `lead_summary`. A4 unconditionally sources Slack `lead_summary` from `thread_summaries.summary`. Default search retains reply IDs and has no equivalent explicit root-authorization rule at all. Only hybrid's ineligible-root members are expressly dropped.

Thus a visible reply whose root is hidden/deleted/missing can receive a cached thread summary under the literal A3/A4 algorithm. Making bot/activity fields unknown does not authorize the prose. This is precisely the new enrichment boundary from R1: the default `searchResult` currently carries `artifact_index.summary`; introducing the thread-table summary is a new data source. The node query also already borrows thread summary as a title fallback (`node.go:79–92`), so a lead branch reusing its response must not assume setting only new author/activity fields to unknown fulfills acceptance 3.

A5 additionally joins *all* live nodes matching scope/thread expression. It does not restrict to Slack message types or apply the applicable visibility rule. Scope membership alone is not the repository's authorization rule for every node type: attachments use `parentVisibleSQL` (`acl_guard.go:279–298`). A matching non-message row must not contribute bot status or activity merely because it has matching metadata. R1 explicitly requested live Slack types and applicable visibility, not only a paired index.

**Required correction:** State one shared eligibility boundary for every root-derived value on resolve, node and both search modes, including `lead_summary`, fallback titles and existing card fields retained in lead mode. For node/default-search replies whose root fails, either omit the row or retain only authorized node-local values and explicitly omit thread-derived values. Define empty/null/omitted representations per field: `last_activity_at` is RFC3339 and `author_slack_id` is an ID-or-empty string, so the blanket instruction “those fields are unknown” is not a wire contract. Scope A5 to live Slack message types and the appropriate visibility predicate. Keep legacy behavior untouched outside lead mode.

**Acceptance:** Execute hidden/missing/deleted/mismatched-scope root fixtures on node-by-reply and default search as well as resolve/hybrid. Put a synthetic sentinel in the root's cached thread summary/title and assert it is absent. Add a non-message/ineligible matching-thread row and verify it cannot affect activity/bot aggregates. Test lookup errors separately from ordinary denial.

**Confidence:** Verified contract omission against source; no live leak claimed.

### F5 — HIGH: the prescribed miss query cannot execute against this schema

**Plan:** lines 111–113.

`SELECT id FROM graph.nodes ... AND body_full IS NULL` fails because `graph.nodes` has no `body_full` column. The body cache is `graph.artifact_bodies`, keyed by `node_id`, with `body_full TEXT NOT NULL` (`migrations/20260527000001_graph_schema.sql:38–69,109–119`). Existing Greedy detects a missing cache row with a left join (`hydrate/budget.go:54–74`).

An implementer following the explicit SQL gets a database error on every nonempty lead resolve; ignoring that error instead silently disables misses and warming. Testing `nodes.body` is not an equivalent fix because it is not the lazy cache.

**Required correction:** Use a left join to `graph.artifact_bodies` or `NOT EXISTS` on its `node_id`, selecting IDs only, over every selected eligible item (Slack roots and retained non-Slack items). Preserve the stated header override and define miss-query failure as `503 lead_lookup_failed`, rather than an empty miss list.

**Acceptance:** Selected nodes with node-body present/cache absent, cache present, non-Slack cache absent, plus lookup failure. Assert exact misses and fetch jobs with/without suppression. No full-body fetch is required.

**Confidence:** Verified schema/query mismatch.

### F8 — HIGH: quiet summary output still prints transport-error URLs

**Plan:** lines 89–90, 189–190, 224–228.

Suppressing the normal request echo and retaining “only the summary tables” is not content-safe on failures:

1. `eval_common.go:203–220` returns the raw `http.Client.Do` error.
2. Transport errors can be `*url.Error`; their string includes the requested URL, including search query parameters.
3. `eval_relevance.go:208–209` stores that string in `FailedRuns`.
4. The **summary** prints every `FailedRuns` string at `:390–393`.

The early unreachable-server return at `:188` also wraps the original error for the CLI error channel. The new safety promise therefore fails on ordinary timeouts/disconnects even if every successful-run echo is removed. Once emitted to the agent transcript, flag rollback cannot undo disclosure.

**Required correction:** Define quiet mode across both stdout and returned/stderr errors. Print counts and safe error categories/statuses only; never stringify URL-bearing transport errors or arbitrary response/parser content in the quiet path. Preserve useful failed-request counts and non-success status. This need not change non-quiet output or the server response contract.

**Acceptance:** A local fake transport returns a `url.Error` containing a synthetic query sentinel, both during warmup and a measured request. Capture summary output and the command's error channel; neither may contain the URL/sentinel. Include an HTTP failure and decoding failure too. The existing success-only request-marker test is insufficient.

**Confidence:** Verified error-to-summary path; standard transport-error representation, no production execution.

## New defects in the opt-in / compatibility delta

### F9 — MEDIUM: lead-only request decoding must itself be behind the gate

**Plan:** lines 24–29, 108–109, 173–175, acceptance 1.

The plan adds `max_items` as a resolve request field but only describes gating the lead pipeline. Today's `resolveRequest` is decoded before any candidate processing (`resolve.go:183–190`), and unknown JSON fields are ignored. Adding a typed integer field to that shared struct changes flag-off behavior **before** the branch: a request containing `"max_items":"unused"` succeeds through today's decoder but fails after that addition, even when the env is false or the lead header is absent. Likewise, unconditional range validation makes previously ignored `max_items:0` invalid.

This is a concrete default-off compatibility hazard introduced by the new flag design, not a demand to support invalid lead requests. The owner explicitly requires the legacy path to stay identical.

**Required correction:** Compute the effective gate before decoding lead-specific options. Keep the legacy decoder/request contract for inactive requests; use a separate lead request shape or raw optional field decoded/validated only when active. Distinguish omitted `max_items` (30) from explicit zero (400) in the active path. New response fields must likewise be absent when inactive, not emitted with zero/unknown defaults. Wire the flag through the route constructors in `router.go`, where handlers are actually created.

**Acceptance:** Env false/true × absent/`1`/other lead header, with omitted/zero/out-of-range/wrong-type `max_items`. Compare inactive statuses and bodies to the legacy implementation, not just ordinary valid fixtures. Invalid options should be rejected only for env=true + header=`1`. Assert no lead-table queries on inactive requests.

**Confidence:** Verified Go decoder behavior and current decode location; implementation guard missing from the plan.

### F10 — MEDIUM: byte-equality and live “numbers equal” gates are not executable as written

**Plan:** lines 17–18, 181–184, 224–225.

Mounted trace responses generate a random request ID (`context_trace.go:52,95–101`) and include it in successful JSON. Resolve reports wall-clock elapsed time (`resolve.go:354`). Existing equal-score resolve ordering is intentionally nondeterministic. The repository's existing compatibility harness consequently removes only `request_id`, normalizes `took_ms`, and rejects tied-score fixtures (`compatibility_test.go:135–175`). Raw mounted responses cannot be byte-equal across runs even with unchanged source. Live eval summary “numbers” also include measured p50/p95 latency (`eval_relevance.go:388`), which cannot equal a prior run reliably on the same data revision.

Without naming this narrow nondeterminism boundary, a no-context implementer must either deliver an impossible gate or weaken it arbitrarily, potentially masking an actual flag-off regression.

**Required correction:** Specify the existing compatibility normalization exactly, or add deterministic test injection for clock/request IDs, with untied fixed fixtures. Compare all remaining raw JSON bytes, HTTP status and relevant headers; separately assert request-ID presence/format and header/body agreement. Do not normalize field sets, ordering, nulls or omission changes. Explicitly test both disabled-env and enabled-env/no-header paths, admin and filtered/masked callers. Define live equality for deterministic ranking/count metrics only; report latency distributions rather than requiring identical numbers.

Also narrow the “exactly as main” statement to explicitly acknowledge A1's intentional removal of eval trace writes. That exception is already described at lines 30–31 and should not be contradicted by the acceptance language; no additional trace flag is requested here.

**Confidence:** Verified existing nondeterministic output and compatibility-test precedent.

## Performance and regression conclusion

The new thread-key index and paired aggregate materially address R1's full-channel-scan concern. They do not bound work within a hot thread, nor make seeds bounded when seeds exceed `max_items`; the latter is now an explicit accepted contract rather than a new defect. Verify the planned EXPLAIN/cancellation/query-count tests at maximum ordinary batch size. No measured latency claim is made here, and no additional performance blocker is raised solely for lacking a numeric SLA.

The mask-before-cap change is adequately specified. It must remain inside active lead serialization, with the existing outer fail-closed mask retained; moving capping into shared serializers would violate the stated inactive path. No additional masking finding is raised speculatively.

The design is substantially clearer than r1, but F1, F5 and F8 remain open, and the new opt-in boundary needs F9/F10 to make the handoff safe and checkable. No trust-tier, detector, relabeling, alert-policy or summary-generation expansion is required.

**CHANGES REQUIRED**
