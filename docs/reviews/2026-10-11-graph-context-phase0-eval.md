# Phase 0a.2 eval plan review

Verdict: **CHANGES REQUIRED**

Reviewed the draft against checkout `db82f1d`. Part B of the approved parent was used as the contract, not re-reviewed. No source changes, hub requests, or external posts.

## Must-fix findings

1. **Specify the persistent input that makes “last prepare replay” recoverable.**
   Plan: `docs/ai/2026-10-11-graph-context-phase0-eval.md:34-45,56-62,77-81`.
   `prepare` is specified to save canonical seeds and a global node metadata map only. Neither records which returned ids belong to which case/variant, or distinguishes returned ids from ids included solely because they are labels or seeds. The separate `label-sheet` process therefore cannot reconstruct its required per-case candidate set. Existing `runRelevance` also retains only result counts, not candidate ids (`cmd/agent-mem/eval.go:251-262`). Replaying HTTP requests during labelling is not equivalent: the plan explicitly measures unstable results, and requires the *last prepare* results.

   **Required change:** define a persisted per-case/per-variant returned-id manifest, its path and publication/refresh behavior, and how `label-sheet` consumes it and rejects missing or mismatched state. Keep any repository artifact metadata/id-only; fetch current title/summary/body separately through a read-only path, or place content-bearing state outside the repository. Update the expected-files section accordingly. Add a fake-server test that prepares two different candidate sets, changes subsequent responses, and proves each sheet still uses its own saved replay ids with canonical seeds excluded. Without this, the new command's central acceptance condition has no specified, checkable input.

2. **Remove the existing test prohibition on the labelled state this plan enables.**
   Plan: `docs/ai/2026-10-11-graph-context-phase0-eval.md:46-55,67-71,93-96`.
   `TestGraphContextCasesCoverManifestAndUseCases` reads the actual repository cases and requires `checked_by == nil`, `labelled_by == "none"`, and every label list to be empty (`cmd/agent-mem/eval_test.go:39-48`). Thus the prescribed post-merge drafting and Enzo verification necessarily make the test suite fail, even with perfectly valid labels. The requested new metric tests do not address this existing invariant.

   **Required change:** explicitly replace those repository-case assertions with validation of allowed labelling states and the Part B label invariants (disjoint lists, no canonical seeds), permitting human-checked cases. Keep “imported probes start unlabelled/unchecked” assertions in the isolated import test (`cmd/agent-mem/eval_test.go:142-175`). Add synthetic valid labelled/checked and invalid-label fixtures; no real labels or customer content are needed in this dispatch. Acceptance should require these fixtures to pass/fail appropriately.

## Verification and acceptance notes

- Confirmed the current revision is `db82f1d`; all 20 cases have empty canonical seeds and `labelled_by: none`; `node_meta.json` and the prepare/label-sheet commands are absent.
- The plan's `eval.go:150-275` and `eval.go:218` references are accurate: one request per variant and API key from the process environment.
- `CanonicalizeSeeds` exists at `internal/graph/handlers/resolve.go:518-575`, accepts the Query/QueryRow interface at `:510-513`, and returns database diagnostics separately from optional no-match results.
- `ingest_content.go:45,417` correctly concern `thread_ts`, but do not establish the id layout themselves. The direct layout evidence is `internal/graph/handlers/ingest_content.go:559-567` and `internal/graph/ids/ids.go:46-53`: `slack:<channel>:<own ts>`, own timestamp in colon component 3. This is discoverable as the plan directs, not a blocker.
- Scope item 4 is checkable without a hub run: `internal/graph/handlers/search.go:370-379` truncates before `addLinkedThreads`; `search_linked_threads.go:13-14,200` permits 10 additional linked rows. `TestLinkedOnlyKeptSourcesExpand` at `search_linked_threads_test.go:330-341` explicitly expects two results for `limit=1`. The harness counts `results` directly (`eval.go:252-254`), not arm lists. No handler change is needed.
- Ran `go test ./cmd/agent-mem -run 'TestGraphContextCasesCoverManifestAndUseCases|TestRelevanceJSONContainsDocumentedResult' -count=1`: passed. Full database tests and vet were not run for this read-only plan review. The historical hub measurements and claimed 1-in-5 flake frequency were not independently verified.
- Other acceptance items are locally checkable. Preserve the parent distinction that ranking uses the first successful **measured** run, not warm-up. For the post-merge handoff, specify `go run ./cmd/agent-mem ...` or rebuild the CLI from updated main; skipping a worker redeploy does not update an installed CLI binary.
