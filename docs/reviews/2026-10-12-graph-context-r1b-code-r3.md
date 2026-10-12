# R1b Part A code review, round 3 (code-r3)

Date: 2026-10-12. Reviewer: deep-review, Round N (fresh subagent, read-only).
Branch: `feat/eval-lead-snapshot` at 83db7f8. Prior round: `docs/reviews/2026-10-12-graph-context-r1b-code-r2.md` (80620e8, REQUEST_CHANGES, N1-N4).
Scope (owner-narrowed): confirm N1-N4 are closed, and review the delta `80620e8..HEAD` (55923b3, 83db7f8) for new defects. Closed r1 findings were not reopened and unchanged code was not reviewed.

**Verdict: APPROVE.** N1, N2 and N3 are closed. N4 is closed for every case code-r2 reported. One LOW residual of N4 remains (a time-prefixed labelled ref followed by `-` or `_`). It does not block and the author may defer it.

## Receipts

- graph: degraded: the code-review-graph build writes into the worktree, which this read-only review may not modify. The new helpers (`shieldOrRef`, `shieldBounded`, the `labelledRefs` extension) are called only from `Apply`, checked with rg.
- fan_out: inline: Round-N delta.
- `go vet ./...`: clean.
- `go test ./internal/redact/...`: PASS.
- `go test ./cmd/agent-mem/ -run 'Lead|Relation|Pseudonym|DryRun|Prepare'`: PASS (24 passed). Seven DB tests skip without a DB and are left to the conductor's `make test-db`.
- Discrimination runs and probes were done in `git archive HEAD` copies in the session scratchpad, outside the worktree. The copies were deleted afterwards.
- Coverage manifest: `validate-ledger.sh --coverage-only` reports `OK: coverage valid`.
- The worktree was left clean. No files were written except this one.

## Convergence ledger

| # | r2 sev | Status at 83db7f8 | Evidence |
|---|---|---|---|
| N1 | HIGH | **Closed** | `git rev-list --objects origin/main..HEAD` has no `cmd/agent-mem/agent-mem` path (count 0). 7f78f95 has the same parent as 5206eda (be38c84). `git diff 5206eda 7f78f95 --stat` shows only `cmd/agent-mem/agent-mem | Bin 39420738 -> 0 bytes`, and the diff with that path excluded is empty, so the tree otherwise matches what code-r2 reviewed. 5206eda is reachable only through the local reflog (`HEAD@{4}`). No branch, local or remote, contains it, and `origin` has no `feat/eval-lead-snapshot` head. `.gitignore:56` has `/cmd/agent-mem/agent-mem` (`git check-ignore -v` confirms). The binary is not in the working tree. |
| N2 | MEDIUM | **Closed** | `shieldOrRef` (`pseudonym.go:167`) is used for the Jira, hash, date and time shields (`:322-325`). `TestPseudonymizerRepeatedRefAliasInShieldShapes` fails against 80620e8's `pseudonym.go` on all 5 single-text cases and on the cross-row case (`row 3: "BKG-123456 again"`). HEAD probes: case variants (`bkg-123456` vs `BKG-123456`, `PAY2420`), a repeat placed before the label (`BKG-123456 then booking BKG-123456` → `ref-1 then booking ref-1`), date+time (`2026-10-12T18:30` → `ref-1T18:30`), and `payments#2424.` all alias. |
| N3 | LOW | **Closed** | Each strengthened test fails on the pre-fix code. `TestPseudonymizerLongLabelledToken` against 6f0482b's `pseudonym.go` gives `"ref ref-190XYZ12 end"`. `TestPseudonymizerShieldPlaceholderNotMatched` against 6f0482b leaks `person-1`. `TestLeadLabelSheetMasksBeforeCutAtBoundary` with `sheetText` flipped to `ps.Apply(truncRunes(s, limit))` fails on both the summary and the title (fragment leaked, alias missing), and passes unflipped. |
| N4 | LOW | **Closed for the reported cases; LOW residual (R3-1)** | `TestPseudonymizerShieldBoundariesAdjacent` fails against 80620e8 (`2026-10-12,ref-1`, `2026-10-12 ref-1`, `ref ref-156789 x`). HEAD probes: `2026-10-12,2026-10-13;2026-10-14` stays readable, while `18:30-19:45`, `09:00–10:00`, `1:23:45` and `12:34:56:78` are unchanged. The digit-run rejections still hold: `12026-10-12` and `2026-10-123` are masked as refs, as before, and `18:30:059` falls back to `18:30` correctly. `ref 12:3456789`, `ref 12:30pm` and `ref 12:34:5678` each mask the whole token. |

## New in the delta

### R3-1. LOW: a labelled time-prefixed ref followed by `-` or `_` still leaks its tail (N4 residual)

`internal/redact/pseudonym.go:383`

The extension only starts when the byte after the time is alphanumeric. Probes on HEAD:

- `ref 12:34-5678 x` → `ref ref-1-5678 x`
- `ref 12:34_5678 x` → `ref ref-1_5678 x`
- `ref 12:34:5 x` → `ref ref-1:5 x` (one digit)

In each case the tail stays in clear, and `ref-1-5678` reads like an alias. The owner's rule says the labelled token is the ref. Likelihood is low: real booking refs rarely start with `hh:mm`.

- Fix: start the extension when `s[t1]` is alphanumeric, `_` or `-` (the loop already accepts those), plus one probe line in `TestPseudonymizerShieldBoundariesAdjacent`.
- Confidence: Verified (probe).
- Non-blocking. The author may defer it.

### Delta checked, no finding

- `shieldBounded`:
  - Boundary bytes are inspected without being consumed.
  - The `hh:mm` fallback only fires on an 8-byte match whose `start+5` boundary passes.
  - A match rejected on its leading digit stays rejected.
  - Non-overlapping `FindAll` loses no valid token in the probes.
- `shieldOrRef` uses the same key as step 0 and step 9 (`"ref|"+normAlias`). Over-masking is possible when a date's digits equal an existing 8-digit ref (`booking 20261012 on 2026-10-12` → `ref-1 on ref-1`). That fails safe.
- The `@` check after the time extension still rejects `ref 12:34abc@x.com`. The email is masked and the time stays readable.
- `.gitignore` and the test edits.

Residual risks, noted and not raised (already-reviewed code, below the round-N HIGH bar):
- A labelled value can still show inside a longer shield-shaped token that is a different value: `ref 2420 then PAY-2420`, `org.payments#2424`.
- The row-order dependence across rows (`on 2026-10-12` before `payment 2026-10-12`) stays residual, as in r2.

No new path prints customer text.

## Open findings

1. R3-1 LOW: `internal/redact/pseudonym.go:383`. A labelled `hh:mm` ref followed by `-`/`_` leaks its tail. Non-blocking.

VERDICT: APPROVE
