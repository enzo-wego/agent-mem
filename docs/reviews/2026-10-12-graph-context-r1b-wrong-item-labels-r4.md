# R1b wrong-item labels plan review, round 4

**Verdict: APPROVED**

Artifact: `docs/ai/2026-10-12-graph-context-r1b-wrong-item-labels.md`, draft r4.

Artifact SHA-256: `fa22897272300f0e70b428a6d3912c07bc7aeefb00150e96b97a747ec2e35e53`.

Baseline: `docs/reviews/2026-10-12-graph-context-r1b-wrong-item-labels-r3.md`. Scope is exclusively its two open entries, the shared strict validation paragraph added at the end of A2, the corresponding A8 regressions, and defects introduced by that delta. Closed entries were not reopened.

## Closure evidence

### R1-F3 — fixed in the plan

A2 (lines 199–209) now requires all three readers—lead label-sheet, relation-assemble and relation-check—to invoke `validateLeadReplay` first. It explicitly recomputes the canonical eligibility digest from loaded items, compares it with the replay header, and compares any available snapshot/label header against that recomputed value. R1c must run relation-check or the same validation before scoring.

Thus the round-3 counterexample (confidence 0.95 → 0.50 with all stored digests retained) must fail, rather than passing equality checks between stale headers. A8 (lines 340–345) requires this exact mutation, endpoint-only drift and author-enum-only drift to be rejected through each of the three command entry points with unchanged stored digests. This is reader-level regression coverage, not merely a hash-generation test.

### R1-F10 — fixed in the plan

The same A2 prerequisite explicitly requires all 20 cases and their lead variants, exact `outcome: ok` without legacy fallback, HTTP 2xx, a present items array, shape-valid items and unique IDs before any of the three readers proceeds. This closes the final relation-command gap independently of whether sheet generation ran earlier.

A8 (lines 340–345) runs missing/failed outcomes and missing/malformed items through all three commands while preserving otherwise valid inputs and stored digests. The outcome-only counterexample is therefore rejected even though its eligibility digest is unchanged. Valid empty arrays remain permitted subject to seed consistency; old-mode compatibility is not changed by the lead-only prerequisite.

No actionable defect was found in this narrow delta. Approval concerns the proposed contract and regression requirements, not an implemented or executed validator.

## Updated convergence ledger

| ID | Status | Round-4 disposition |
|---|---|---|
| R1-F1 | fixed, carried | Not reopened. |
| R1-F2 | fixed, carried | Not reopened. |
| R1-F3 | **fixed** | A2 mandates reader-side eligibility rehash and comparison with all available stored copies; A8 tests unchanged-header metadata mutations at every reader; R1c validation is explicit. |
| R1-F4 | fixed, carried | Not reopened. |
| R1-F5 | fixed, carried | Not reopened. |
| R1-F6 | fixed, carried | Not reopened. |
| R1-F7 | fixed, carried | Not reopened. |
| R1-F8 | fixed, carried | Not reopened. |
| R1-F9 | fixed, carried | Not reopened. |
| R1-F10 | **fixed** | Shared strict case/variant/outcome/status/items validation precedes both relation commands as well as sheets; A8 exercises reader refusal and valid empty arrays. |
| R1-F11 | fixed, carried | Not reopened. |
| R1-F12 | fixed, carried | Not reopened. |
| R2-F1 | fixed, carried | Not reopened. |
| R2-F2 | fixed, carried | Not reopened. |

Open entries: **0**. New findings: **0**. No further review needed for this delta.

## Scope and evidence receipts

- `graph: not-applicable: documentation-only delta; no changed code symbols`
- `fan_out: inline: Round-N delta`
- Static trace: loaded replay → strict shared preconditions → recomputed eligibility → stored-copy comparisons → relation gate → R1c scoring prerequisite.
- Regression audit: mapped the two round-3 counterexamples to A8's three-command mutation checks; tests are specified, not executed.
- No source edits, commits, pushes, eval executions, testdata reads, `.env` reads, secret output, external model calls or task-tracker mutations. Only this requested report was written.

**Final verdict: APPROVED.**
