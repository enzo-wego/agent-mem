# R1b Part A code review, round 2 (code-r2)

Date: 2026-10-12. Reviewer: deep-review, Round N (fresh subagent, read-only).
Branch: `feat/eval-lead-snapshot` at 5206eda. Prior round: `docs/reviews/2026-10-12-graph-context-r1b-code-r1.md` (REQUEST_CHANGES, findings 1-12).
Delta reviewed: `6f0482b..HEAD` (be38c84, 5206eda). Brief: `docs/ai/2026-10-12-graph-context-r1b-wrong-item-labels.md` Part A, plus the owner decision on finding 2 (TIGHTEN a/b/c).

**Verdict: REQUEST_CHANGES.** The code fixes are sound and 1-6 and 9-11 are closed with tests that fail without the fix. Two things block:

- **N1, HIGH:** a 39 MB arm64 binary, `cmd/agent-mem/agent-mem`, was committed in 5206eda. It has not been pushed yet, so removing it is cheap now. Once it is pushed it stays in history.
- **N2, MEDIUM:** finding 3 is only half closed. A labelled ref is masked, but the same value is still shown in clear when it appears unlabelled in a shield shape (Jira-style key, date, time, `x#n`), in the same text or in a later row of the case.

Two LOW items do not block on their own: three regression tests cannot fail without their fix (N3), and boundary edge cases in the new regexes (N4).

## Receipts

- graph: degraded: the code-review-graph build writes into the worktree, which this read-only review may not modify. Callers were traced with rg.
- fan_out: inline: Round-N delta.
- `go vet ./...`: clean.
- `go test ./internal/redact/...`: PASS.
- `go test ./cmd/agent-mem/ -run 'Lead|Relation|Pseudonym|DryRun|Prepare|HubCheckout'`: PASS (41 passed). Seven DB tests skip without a DB, among them `TestPrepareLeadOutcomes` and `TestPrepareRemovesSnapshotWhenFinalWriteFails`. They are left to the conductor's `make test-db`.
- Discrimination runs were done in scratch copies outside the worktree:
  - The new `pseudonym_test.go` was run against the pre-fix `pseudonym.go` (6f0482b).
  - `sheetText` was flipped to cut-then-mask in a `git archive` copy.
- Coverage manifest: `validate-ledger.sh --coverage-only` reports `OK: coverage valid`.
- Worktree left clean. No files were written except this one.

## Convergence ledger

| # | r1 sev | Status at 5206eda | Evidence |
|---|---|---|---|
| 1 | HIGH | **Closed** | `replaceNames` uses Unicode boundaries, and suffix-stripped names are registered as well. Against the old code, `TestPseudonymizerUnicodeNames` fails on all 8 cases plus the inside-word check. Probes pass: `محمد علي`, `Zoë`, `Ann (Ops)`, `Ann Lee (OOO)`, `J.D.`, `Zoë 🌴`, a case-folded multi-space name, and `محمدعلي` (correctly not matched inside a longer word). |
| 2 | MEDIUM (owner Q) | **Closed per owner decision a/b/c** | `TestPseudonymizerLabelBeatsShields` and `TestPseudonymizerShieldBoundaries` fail against the old code on every must-mask case. The must-stay-readable strings (`PAY-2420`, `payments#2424`, `2026-10-12`, `18:30`) are tested on their own and in one sentence. |
| 3 | MEDIUM | **Partially closed, see N2** | Step 9 aliases repeated unshielded tokens, and the flipped expectation fails against the old code. A shield-shaped repeat still leaks. |
| 4 | LOW | **Closed** | `TestParseLeadResponseRejectsNullAndEmpty`: 7 shapes, unit test with no DB, fails without the fix. |
| 5 | LOW | **Closed** | `insideAnyCheckout` compares with `os.SameFile` while walking parents, and `hubCheckout` covers the hub. `TestHubCheckoutAndGuard` fails without the fix, and its case-variant branch runs on APFS. Sibling worktrees of the same hub are still not covered. That is acceptable for an operator-error guard (Part B uses `$PRIV`). |
| 6 | LOW | **Closed** | `TestRelationRowsRefuseCasesShape` fails without the fix. |
| 7 | LOW | **Closed in code; test does not discriminate (N3)** | Placeholders are now U+E010..E019 and input U+E000..E01F is scrubbed. A 50-shield probe with `AddPerson("U1","E")` leaks against the old code and is clean against the new. The shipped 8-shield test passes against the old code. |
| 8 | LOW | **Closed in code; test does not discriminate (N3)** | The token is now unbounded. Probe `ref AB12…(36 C)…90XYZ12`: the old code gives `ref ref-190XYZ12`, the new gives `ref ref-1`. The shipped all-`A` prefix test passes against the old code. |
| 9 | LOW | **Closed** | Doc paragraphs corrected: the `--quiet` categories, the reply-seed precision note, and client-side `BUILD_REV`. |
| 10 | LOW | **Closed** | `leadSeedsPresent` is shared by prepare and relation. The `TestLeadSeedsPresent` unit test and the DB subtest "seed not a hop-0 item" fail without the fix. |
| 11 | LOW | **Closed** | `adjudication`, `build_rev_mismatch` and `snapshot_missing` are checked by value. `TestRelationCheckValuesNotJustPresence` fails without the fix. |
| 12 | LOW | **Partially closed (N3)** | The cleanup test (`TestPrepareRemovesSnapshotWhenFinalWriteFails`, DB) is discriminating by construction. `TestLeadLabelSheetMasksBeforeCutAtBoundary` still passes with `sheetText` flipped to cut-then-mask (verified). The golden-origin note is not addressed. It is LOW, so the author may defer it. |

## New findings (delta)

### N1. HIGH: 39 MB build binary committed

`cmd/agent-mem/agent-mem` (5206eda, `Bin 0 -> 39420738 bytes`, Mach-O arm64)

- `.gitignore` ignores `/agent-mem` (repo root) only, so a `go build` inside `cmd/agent-mem/` was staged with the fix commit.
- No remote branch contains 5206eda yet (`git branch -r --contains` is empty).
- Once the branch is pushed or merged, the blob stays in history for every clone and only a history rewrite removes it. Low reversibility moves this to HIGH even though the raw impact is repo hygiene.
- Fix:
  - `git rm --cached cmd/agent-mem/agent-mem`.
  - Fold that into 5206eda (local rewrite, nothing pushed).
  - Add `/cmd/agent-mem/agent-mem` to `.gitignore`.
- Confidence: Verified.

### N2. MEDIUM: a labelled ref stays visible where it reappears in a shield shape

`internal/redact/pseudonym.go:283-286` (shields) against `:323-331` (step 9)

Step 0 now aliases shield-shaped values (owner decision a). Step 9 runs after the shields and only sees unshielded runs, so a repeat that a shield protects is restored verbatim at unshield. Probes on HEAD:

- `booking BKG-123456 failed; BKG-123456 retried` → `booking ref-1 failed; BKG-123456 retried`
- `payment PAY-2420 see PAY-2420` → `payment ref-1 see PAY-2420`
- `ref 2026-10-12 and later 2026-10-12` → `ref ref-1 and later 2026-10-12`
- With one Pseudonymizer across rows (as `resolve-lead` uses per case): row 2 `booking BKG-123456` → `booking ref-1`, then row 3 `BKG-123456 again` → `BKG-123456 again`. The alias is already known at that point.

Why it matters:
- A reader sees `ref-1` next to the clear value, so the mask is defeated for exactly the refs the owner chose to mask.
- Sheets go to external models, so the exposure cannot be undone.
- Realism depends on whether real refs fit `[A-Z][A-Z0-9]+-\d{1,6}`. This review cannot check live data. MEDIUM, consistent with the r1 rating of finding 3.

Fix (one helper, applied in step 1):

```go
shieldOrRef := func(lit string) string {
	if a, ok := p.alias["ref|"+normAlias(lit)]; ok {
		return p.shield(a)
	}
	return p.shield(lit)
}
```

Use it for the Jira, hash, date and time shields. Add a test for the four probes above, the cross-row one included. The row-1-before-row-2 order dependence stays residual, as in r1.

Confidence: Verified (probe).

### N3. LOW: three regression tests pass against the pre-fix code

- `internal/redact/pseudonym_test.go:169` `TestPseudonymizerLongLabelledToken`:
  - With an all-`A` 44-char prefix, the old 40-char cut has no digit, so step 7 masks the whole run anyway.
  - Use a digit in the first 40 characters, e.g. `"AB12"+strings.Repeat("C",36)+"90XYZ12"`. The old code gives `ref ref-190XYZ12`.
- `internal/redact/pseudonym_test.go:209` `TestPseudonymizerShieldPlaceholderNotMatched`:
  - 8 shields never produce the old `e`/`ed` placeholders.
  - Use 50 shields with `AddPerson("U1","E")`. The old code leaks `person-1`.
- `cmd/agent-mem/eval_lead_r1_test.go:114` `TestLeadLabelSheetMasksBeforeCutAtBoundary`:
  - The cut leaves `AB12C…`. It has a digit, so the step-8 tail rule masks it either way, and `AB12CD` can never appear.
  - Verified by flipping `sheetText` to `ps.Apply(truncRunes(s, limit))`: the test still passes.
  - Use a ref whose cut fragment has no digit (e.g. `ABCDEFGH123456` placed so the cut keeps `ABCDE`), and assert that `ABCDE` is absent.

The code behind all three is correct, so nothing is hidden today. The tests just would not catch a regression.

### N4. LOW: boundary edge cases in the new regexes

`internal/redact/pseudonym.go:50-51, 57`

- **Dates and times.** The date and time shields consume the trailing boundary character, and `replaceGroup` uses non-overlapping `FindAll`. A date right after a one-character separator is therefore not shielded, and step 7 masks it as an 8-digit ref:
  - `2026-10-12,2026-10-13` → `2026-10-12,ref-1`
  - `2026-10-12 2026-10-13` → `2026-10-12 ref-1`
  - This loses evidence in labeller sheets and contradicts "dates alone stay readable".
  - Fix: check the trailing boundary without consuming it (inspect `s[end]` in a loop instead of capturing group 3).
- **Labelled times.** `reLabelledRef`'s time alternative wins leftmost-first:
  - `ref 12:3456789` → `ref ref-156789` leaks 5 digits and looks like an alias.
  - Fix: reject the time alternative when the next byte is alphanumeric (same spot as the `@` check at `:344`).

Confidence: Verified (probe). Low likelihood.

## Delta checked, no finding

- `parseLeadResponse` null and empty-kind rejections.
- The `readRows` scalar-key split.
- The relation-check value checks.
- `snapshot_missing` placed before the eligibility check.
- The prepare seed check after meta is fetched and before any write (the failure leaves no files: covered by the existing `sameSnapshot` assertion in the DB test).
- The `prepareServer.leadRoot` fixture change.
- The header text and both doc edits.
- `stripNameSuffix` termination.
- The `replaceNames` progress on rejected matches and on empty matches.
- PUA scrubbing of input.
- Label-before-mention ordering: `<@U…|payment 2024 team>` → `person-1`, and `payment 98765432@x.com` → `email-1`.

No new path prints customer text.

## Open findings

1. N1 HIGH: `cmd/agent-mem/agent-mem` (committed binary; remove before push, add to `.gitignore`).
2. N2 MEDIUM: `internal/redact/pseudonym.go:283-286`. A shield-shaped repeat of a labelled ref leaks (finding 3 residual).
3. N3 LOW: `internal/redact/pseudonym_test.go:169`, `:209`, `cmd/agent-mem/eval_lead_r1_test.go:114`. These tests do not discriminate.
4. N4 LOW: `internal/redact/pseudonym.go:50-51,57`. Adjacent dates are masked, and the labelled time alternative cuts a longer token.

VERDICT: REQUEST_CHANGES
