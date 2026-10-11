# Phase 0a.2 eval plan re-review (r2)

Verdict: **APPROVED**

Reviewed r2 against checkout `db82f1d`, using the r1 review and the approved parent's Part B contract. Both r1 must-fix items are resolved. No new blocking correctness, safety, or acceptance-checkability finding in the r2 changes. No further plan review needed before implementation.

## R1 findings resolved

1. **Persisted last-prepare input:** r2 lines 45-51 define `replay.json`, including per-case canonical seeds and ordered returned ids per variant, publication/error behavior, and refresh behavior. Lines 62-72 require label sheets to consume that saved per-case set, exclude seeds, reject missing/stale/absent-case state, and fetch current content separately through a read-only transaction. Lines 93-96 specify the two-case changed-server regression test. The expected-files section now includes the manifest (lines 105-107). This supplies a checkable input without committing response content.
2. **Labelled repository cases:** lines 80-88 explicitly replace the blanket unlabelled/unchecked assertions with allowed-state and label-invariant validation, retain isolated import assertions, and require valid labelled/checked and invalid overlap/seed fixtures. This permits the prescribed later human checking without breaking the repository-case test.

## Verified repository claims touched by r2

- `internal/graph/ids/ids.go:46-53` and `internal/graph/handlers/ingest_content.go:559-567` establish `slack:<channel>:<own ts>` for message ids; own timestamp is colon component 3.
- `internal/graph/handlers/search.go:370-379` truncates primary results before hybrid linked-thread expansion. `search_linked_threads.go:13-14,200` bounds expansion to three per source and ten overall. `search_linked_threads_test.go:330-341` expects two rows for `limit=1`.
- Linked rows are distinguishable without a handler change: `search_linked_threads.go:206` assigns `LinkedVia`; `search_hybrid_display.go:39-40,138-140` exposes `linked_via` and `linked_via_title`, and sets `match` to `["linked"]`. Primary/linked counts are therefore implementable using the existing response.
- `cmd/agent-mem/eval_test.go:39-48` contains precisely the repository-state prohibitions being replaced. The import test at `:142-175` retains its independent unlabelled/unchecked assertions.
- `internal/graph/handlers/resolve.go:510-575` supplies the transaction-compatible Query/QueryRow canonicalisation function and separate real-database-error diagnostics.
- The first-successful-**measured**-run rule is explicit at r2 lines 96-97, consistent with Part B. The updated-main `go run` handoff at lines 119-121 avoids relying on an unchanged installed CLI.

## Safety and acceptance

The delta keeps direct database access read-only, keeps HTTP replays under the existing no-side-effects contract, restricts committed new oracle/replay artifacts to ids and metadata, and requires content-bearing label sheets outside the repository. The parent's existing trace-row exception remains unchanged; this is not a claim that HTTP replays produce literally zero database writes. No hub execution, customer-content collection, source edits, or external posting was performed for this review.

The new manifest and label-state requirements are locally testable with synthetic fixtures, a fake HTTP server, and the scratch database. Publication guarantees remain implementation requirements: staging files alone must not be mistaken for proof that all error paths preserve both existing outputs.

Verification run:

```text
go test ./cmd/agent-mem -run 'TestGraphContextCasesCoverManifestAndUseCases|TestRelevanceJSONContainsDocumentedResult|TestImportProbesDerivesNeedsContextAndLeavesLabelsEmpty' -count=1
ok github.com/agent-mem/agent-mem/cmd/agent-mem 0.324s
```

Full database tests and vet were not run for this plan-only delta review. Historical hub measurements and the stated flake frequency were not independently verified.
