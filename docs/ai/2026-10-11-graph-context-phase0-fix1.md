# Phase 0a fix round 1

Branch `feat/graph-context-phase0` (HEAD 78e62ab), worktree
`/Users/enzo/go/src/github.com/agent-mem-wt-gc0`. Plan:
`docs/ai/2026-10-11-graph-context-phase0.md` (approved). Source of these items: a
deep-review of 78e62ab (2026-10-11). Same scope rules as the first dispatch: no hub
eval, no merge, no deploy, no container restarts, scratch DB only.

Fix every item below. Add a test for each behaviour item, and make sure each new test
fails on 78e62ab's code (say in the summary how you checked).

## Must fix

1. **Goldens depend on host timezone.** `TZ=UTC make test-db` fails the three search
   goldens (`created_at` decoded in local time). Pin `time.Local = time.UTC` for the
   compatibility test (or normalise timestamps before compare) and re-record the
   goldens if needed. Acceptance: `make test-db` passes with `TZ=UTC` and with
   `TZ=Asia/Singapore`.
2. **Success body changes beyond `request_id`.** `context_trace.go:65-71` round-trips
   the body through `map[string]any`, which re-orders keys and turns numbers into
   float64. Insert `"request_id":"<id>",` right after the leading `{` of the original
   bytes instead (no re-marshal). Change `compatibility_test.go:116-140` to compare
   **raw bytes** after removing only the top-level `request_id` member and the timing
   fields named in the plan, and to assert `request_id` was present before removing
   it.
3. **Search `arm_errors` never traced.** The `traceResponseKey{}` branch
   (`context_trace.go:154-158,167`) is dead. Pass `arm_errors` from the search handler
   to the recorder and store it in `request`. Test asserts the stored value for a
   hybrid call with a failing arm.
4. **Resolve `acl_mode` recomputed.** Set the trace's ACL mode from the handler's own
   `noFilter` at `resolve.go:290` (as search does) and record the decoded
   `AskerEEID`, not a re-parse of the raw body. Test: body `{"Asker_EEID":5}` is
   recorded as `filtered` with eeid 5.
5. **ACL diagnostic tests missing.** Add tests for `askerScopeSetDiag`: a resolved
   asker with zero memberships gives `filtered`; an injected membership-builder failure
   gives `filtered-membership-error`, and old callers' results are unchanged. Add the
   smallest seam needed to inject the failure (an interface or func field), default
   path unchanged. Assert `acl_mode` values in the trace tests.
6. **`trace show` not like-for-like.** Render the current representation with the
   original route's logic: resolve uses the hydrate path (`hydrate/budget.go:49-60`
   thread-summary title for slack nodes, `body_full`, same truncation length);
   ordinary search uses node title + `artifact_index.summary`; hybrid uses
   `search_hybrid_display.go` (body first line, thread summary, overview). Reuse those
   functions; do not copy their logic. Print `not comparable (body not returned)` when
   no body was returned. The not-found message prints the writer counters. `--since`
   accepts `Nd` as days. Tests: an untouched Slack thread with a thread summary and a
   truncated body prints `same as returned`; an edited node prints `differs`.
7. **Resolve `title_source` mislabelled.** Use `thread_summary` only when the hydrate
   path actually used it (slack / slack_thread with a non-empty summary). Test an
   empty-title Jira attachment and a Slack thread with no summary.
8. **Request caps not applied.** Apply the plan's caps (strings 2,000 chars, arrays 50
   entries) to stored `request` for both routes, cutting on rune boundaries, and store
   canonical seed ids for resolve. Test with an oversized request.
9. **Eval cases and command incomplete.**
   - Replace the generic queries in `case-01..14.yaml` with each thread's real
     triggering message text, read from Slack with the token the hub already uses.
     Mask customer PII in the text (names, emails, phones, booking refs, card data)
     as `[customer]`, `[email]`, `[phone]`, `[booking-ref]`, `[card]`. Keep the
     `source_thread` permalink.
   - Labels: labelling needs the hub (node ids come from `eval prepare` replays), so it
     is out of this dispatch. Set `labelled_by: none` and keep the label lists empty
     in every case; the harness prints `unlabelled` for such cases instead of
     scoring them.
   - `eval import-probes` must not hard-code `needs_context` or `labelled_by`:
     take `needs_context` from the manifest's use case (UC9 = no, others = yes) and
     set `labelled_by: none`.
   - `eval relevance --json` must write the documented object (build_rev,
     cases_sha256, node_meta_sha256, started_at, per_case, aggregate, request_ids),
     not `{}`. Test with an httptest server.
10. **Payment ref in committed files.** Org policy: no booking or payment references
    in committed files. Replace the original payment ref (value deliberately not repeated here) in
    `docs/ai/2026-10-11-graph-context-probes.yaml` and
    `testdata/eval/graph_context/cases/probe-c.yaml` with `[payment-ref]`, and scan
    all committed case and doc files for other refs (pattern hints: order / booking /
    payment ids in message text) and mask them the same way. Rewrite only these files;
    do not rewrite history.

## Should fix

11. No-side-effects test: add a request with `X-Asker-User` and one with
    `include_bodies: true` so `lookupAsker`, the ACL builder and the trace diagnostic
    query run on the read-only pool; check bodies match expectations, not only status.
    Accept `1` and `true` (case-insensitive) as on for the header; any other value is
    off, documented.
12. `make deploy-payments` and `make restart` must pass `BUILD_REV=$(git rev-parse
    HEAD)` like the CLAUDE.md command, so every deploy path stamps the sha.
13. `test-db` readiness: `pg_isready -h 127.0.0.1 -d agentmem_test` and fail when the
    loop runs out. `check-db-skips.go` fails on empty input.
14. Trace robustness: cut `error` and `caller` on rune boundaries and strip NUL bytes
    before jsonb; make `Close` not double count; limit the request body read to 1 MB
    for the trace copy (the handler's own limit unchanged). Give
    `TestPruneUsesThirtyDayCutoff` a real assertion (row at 29 days kept, 31 days
    deleted) and `TestCompatibilityFixtureHashIsStable` a real hash comparison.

## Verification to paste in the summary

`make test-db` (default TZ), `TZ=UTC make test-db`, `go vet ./...`,
`go test -race ./internal/graph/trace/... ./cmd/...`, and
`go run ./cmd/agent-mem eval relevance --dry-run | head -40`. Commit per item group,
push the branch.
