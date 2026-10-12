# R1a lead mode — plan review R3

**Verdict: APPROVED**

The five open R2 findings are resolved at the plan-contract level. No new blocking defect was found in the R3 delta. No further plan review is needed before implementation.

## Scope and evidence

Reviewed `docs/ai/2026-10-12-graph-context-r1a-lead-fields.md` (draft r3) against the supplied R2 review and local repository HEAD `55d90dc49a06065999a47af7200d1d2fac6c3d7a`.

This was a delta review of F1, F5, F8, F9 and F10, not a fresh review of previously resolved findings. The prior review supplies the R2 contract and finding history; no separate R2 plan snapshot was supplied. Source tracing covered request decoding and wiring, root authorization, node/search serialization, body-cache schema, eval failure output, tracing and compatibility normalization.

Local source inspection only. No hub, network, database, builds, tests, deployment, commits or pushes. No secrets or customer data were inspected. Only this review file was written. Approval concerns the implementability of the plan, not successful execution of its future tests or measured performance.

- graph: not-applicable: documentation-only re-review; relevant existing paths traced manually
- fan_out: inline: Round-N delta
- Security: manual authorization and quiet-output delta review; no dedicated scanner.
- Coverage/ledger tooling: not run because its temporary-file workflow conflicts with the explicit single-output-file restriction. Finding dispositions are recorded below.

## Open-finding dispositions

### F1 — Resolved: all thread-derived enrichment shares root eligibility

**Plan:** A2a, A2.4, A3, A5; acceptance 3 and 8.

A2a now explicitly covers thread summaries, summary-derived title fallbacks, root author fields and activity/bot aggregates on all four read paths. An ineligible reply is omitted, or returns 404 on node, rather than being retained with an ambiguous mixture of node-local and unauthorized thread-derived values. Lookup errors are distinct 503 failures. This eliminates the undefined denied-root field representations raised in R2.

This boundary applies to the existing title fallback as well as new fields: `internal/graph/handlers/node.go:79–100` already joins `thread_summaries`. A3 drops ineligible-root rows in default search as well as hybrid. Acceptance 3 includes cached-summary/title sentinels, missing/deleted/hidden roots and mismatched scope.

A5 now restricts aggregates to live Slack message types and the asker's `aclVisibleSQL`, explicitly excluding files and other node types. This addresses the attachment visibility distinction in `internal/graph/handlers/acl_guard.go:279–298`. Acceptance 8 verifies that matching-thread non-message and hidden rows cannot affect aggregates.

The message-type discovery instruction is actionable: `slack_thread` is mentioned in the initial schema and included alongside `slack` in `migrations/20260705000001_fix_bare_slack_scope.sql`. The implementer must complete the specified type inventory rather than silently assuming only `slack` exists.

### F5 — Resolved: misses use the actual body-cache table

**Plan:** A2.7; acceptance 7.

The replacement `NOT EXISTS` query tests `graph.artifact_bodies.node_id`, matching the cache schema at `migrations/20260527000001_graph_schema.sql:109–119`. It no longer references a nonexistent node column or reads full bodies. It covers selected Slack roots and non-Slack items, preserves suppression/enqueue semantics, and specifies 503 on lookup failure.

Cache-present/cache-absent fixtures and exact miss/enqueue assertions cover the original failure mechanism.

### F8 — Resolved: quiet mode covers error channels, not only request echoes

**Plan:** A1; acceptance 2.

Quiet mode now replaces failed-run error strings with bounded categories and explicitly sanitizes both the early unreachable-server return and every error returned to the CLI. This closes the paths from `evalClient.do` (`cmd/agent-mem/eval_common.go:203–229`) through warmup, `FailedRuns` and summary printing (`cmd/agent-mem/eval_relevance.go:185–209,390–393`).

The test contract captures stdout, stderr and returned errors, with URL-bearing transport failures during both warmup and measured requests, plus HTTP and decoding failures. Non-quiet output remains unchanged. No production-output safety claim is made before these tests exist.

### F9 — Resolved: the gate precedes lead-only decoding and validation

**Plan:** Flag and opt-in; acceptance 1.

The legacy `resolveRequest` is explicitly unchanged, inactive requests retain its decoder, and lead-only options are decoded/validated only when the effective gate is active. This addresses the current pre-processing decode at `internal/graph/handlers/resolve.go:183–190`. The optional integer distinguishes omitted/default from explicit zero.

The flag is explicitly routed through `Deps` and the actual constructors in `internal/graph/handlers/router.go:85–95`. Inactive output must omit lead fields and perform no lead-only queries. The env/header/value matrix includes wrong-type and out-of-range options, so it can catch compatibility regressions before candidate processing.

### F10 — Resolved: compatibility comparisons exclude only documented nondeterminism

**Plan:** Acceptance 1; Verify 2.

Acceptance now names the existing request-ID removal and timing normalization, uses untied fixtures, and prohibits other field/order/null normalization. Status and relevant headers remain compared; request IDs are checked separately. The env-off/header-on response-header exception and intentional A1 trace-write exception are explicit.

This matches the intent of `internal/graph/handlers/compatibility_test.go:135–175` and the random ID injection in `context_trace.go:50–101`. Live comparison is narrowed to ranking/count metrics on the same data revision; latency is reported rather than required to equal a previous run.

## R3 delta and implementer handoff

No additional finding survives the delta review. Two implementation details follow directly from the accepted contracts and should not be mistaken for new behavior:

- **Decode the same request payload, not the exhausted stream.** The active branch's “decoded a second time” wording requires retaining/replaying the payload, or decoding the active request shape once and deriving the shared fields. A second decoder directly on the already-consumed `r.Body` is not a replay. Keep inactive decoding untouched.
- **Apply trace normalization only where tracing exists.** The existing compatibility helper expects a leading `request_id`; node, epic and neighbors are not trace-wrapped. Keep those bodies byte-exact and assert their existing absence of trace IDs. On traced successful responses, remove only the generated ID for comparison and assert header/body agreement separately. Do not add tracing to previously untraced routes to satisfy the test.

Neither point changes the requested API or adds scope; both are ordinary implementation consequences of the explicit legacy-compatibility requirement. The supplied acceptance suite remains the implementation gate. Previously resolved F2, F3, F4, F6 and F7 were not reopened.

**APPROVED**
