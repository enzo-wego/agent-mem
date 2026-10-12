# R1b Part A code review, round 1 (code-r1)

Date: 2026-10-12. Reviewer: deep-review (fresh subagent, read-only).
Branch: `feat/eval-lead-snapshot` at 25f07a7, merge-base `origin/main` 288d1a4. No PR yet; local diff reviewed.
Scope: brief `docs/ai/2026-10-12-graph-context-r1b-wrong-item-labels.md` (draft r4, approved), Part A only
(A1-A9, Gates, acceptance 1-2). Accepted addition: 25f07a7 Makefile `docker rm -fv` (fine).

**Verdict: REQUEST_CHANGES.** One HIGH privacy defect (finding 1): registered author names are not
masked when they start or end with a non-ASCII letter or a punctuation suffix. One MEDIUM owner
question (finding 2): refs that look like Jira keys or `word#digits` are kept verbatim because the
spec-mandated shields run first. The masked sheets go to pi (OpenAI) and Opus. Once text has been
sent it cannot be recalled, so both should be settled before Part B.
Everything else is LOW, apart from finding 3 (MEDIUM, cheap hardening). The structure (A1-A4, A6, A7),
the strict decoding and the `validateLeadReplay` wiring are sound, and the tests are real.

## Receipts

- graph: degraded: the code-review-graph build writes into the worktree, which this read-only review may not modify. Callers were traced with rg.
- fan_out: ran (4/4) (structural, temporal, tests-and-claims, outward).
- Execution, run by the temporal bundle, the only bundle allowed to execute:
  - `go vet ./...`: clean.
  - `go test ./internal/redact/...`: PASS (12 tests, 0 skipped).
  - `go test ./cmd/agent-mem/ -run 'Lead|Relation|Pseudonym|DryRun'`: PASS, with 1 skip (`TestPrepareLeadOutcomes`, which needs a DB and runs under `make test-db`; the conductor runs that).
  - Pseudonymizer and path-guard probes, run as scratch-module tests outside the worktree.
- Coverage manifest: `validate-ledger.sh --coverage-only` reports `OK: coverage valid`.
- Gate `rg "t\.Skip|\.only\(|TODO"` over the changed files: nothing new. The only hit is the pre-existing `t.Skipf` at `eval_acl_test.go:210`. No stub tests and no empty bodies.

## Findings

### 1. HIGH: author names with non-ASCII or punctuation edges are never masked

`internal/redact/pseudonym.go:182`

```go
re, err := regexp.Compile(`(?i)@?\b` + strings.Join(words, `\s+`) + `\b`)
```

- RE2's `\b` is an ASCII word boundary. A name whose first or last character is not `[A-Za-z0-9_]` never matches.
- Probes (`AddPerson`, then `Apply`) that came back unchanged:
  - `محمد علي`
  - `Zoë said`
  - `Ann (Ops)`
  - `J.D.`
- A display name with a suffix, such as "Ann Lee (OOO)", is registered whole, so plain "Ann Lee" in prose is not linked either.
- A6 says author names are masked, and the sheet header claims it. Arabic display names are realistic for MEA.
- The sentinel tests use ASCII names only, so they cannot catch this.
- Impact: author names in clear text in sheets sent to external models. Plausible, and cannot be undone once sent.

Fix:
- Use the Unicode-aware boundary that `reAtName` already uses, captured as groups: `(^|[^\p{L}\p{N}_])` … `($|[^\p{L}\p{N}_])`.
- Also register the display name with a trailing parenthetical or emoji suffix stripped.
- Add tests with an Arabic name, an accented name at an edge, and a suffixed name.

Confidence: Verified (probe).

### 2. MEDIUM, owner question (blocks Part B until answered): step-1 shields keep real refs verbatim

`internal/redact/pseudonym.go:42-45`

Probes that came back unchanged:
- `booking BKG-1234567890 failed`
- `pnr XY-123456789012`
- `order#123456789 failed`
- `ref 8812-34-5678901`. The unanchored date shield takes `8812-34-56`, and the 5-digit remainder is not ref-like.

The code follows the brief's regexes exactly. The brief requires `payment PAY-2420` to stay unchanged, and step 6 lists `#` as a label separator, but the hash shield fires first. So this is a gap in the spec, not a deviation from it. It does contradict the sheet header's "refs with a digit" masked.

Options for the owner:
- (a) Cap the Jira digit run (e.g. `\d{1,6}`), or shield only known project keys.
- (b) Skip the hash shield when the left part is a label word.
- (c) Anchor the date and time shields with non-digit boundaries.
- (d) Accept it as residual, and name it on the approval page and in the sheet header.

Confidence: Verified for the behaviour. Whether real lead summaries hold refs in these shapes is a live-data fact this review cannot query.

### 3. MEDIUM: a value already aliased stays visible elsewhere in the same text

`internal/redact/pseudonym.go:230-237`. The behaviour is locked in by `pseudonym_test.go:37`:

`"AB1234 and AB1234 again: payment ref AB1234"` → `"AB1234 and AB1234 again: payment ref ref-1"`.

- The same happens across one case's rows: a title "payment ref AB1234" and a summary "AB1234 retried".
- The unlabelled short token is outside the A5 grammar, so this is accepted residual on paper. But the pseudonymizer already knows the value is a ref.
- Cheap fix: after step 8, replace any `reRun` token whose `normAlias` is already a key in the `ref` namespace. Flip the test expectation to `ref-1 and ref-1 again: payment ref ref-1`.
- Getting the same effect across rows needs a pre-pass over all rows of a case before rendering. That is optional.

Confidence: Verified (existing test).

### 4. LOW: the lead parser accepts `null` and empty values that the validator later refuses

`cmd/agent-mem/eval_common.go:689` and `:706-713`

- `json.Unmarshal("null", &int)` succeeds, so `"hop": null` becomes hop 0 and the item silently becomes a seed.
- `"kind": null` or `"kind": ""` and `"from"`/`"to": null` pass prepare as `ok`.
- `leadItemsValid` then refuses `Kind == ""` with `lead_items` in all three readers, after replay.json has been written.
- The replay decoder (`strictField`) rejects null, so the producer and the consumer disagree.
- The server's types make this unreachable today (`Hop` is a non-pointer int; `Kind` comes from `graph.edges.kind`). A2 still says "integer hop" and "string kind".

Fix: reject a raw `null` for node_id, hop, authored_by_bot, kind, from and to, and reject empty `kind`, in `parseLeadResponse`.

### 5. LOW: the outside-repo guard is case-sensitive and only knows the current checkout

`cmd/agent-mem/eval_prepare.go:100` (`checkPrivateTarget`) and `:456` (`insideDir`)

- APFS is case-insensitive, and `EvalSymlinks` keeps the case as typed.
- `/users/enzo/.../agent-mem-wt-lead/testdata/newsnap` and `/Users/Enzo/.../AGENT-MEM-WT-LEAD/newsnap` were both accepted (probe on a verbatim copy).
- `../agent-mem/newsnap`, the hub checkout, is also accepted.
- Part B uses `$PRIV`, so this only bites on operator error.

Fix: walk the parents comparing with `os.SameFile` against the repo root, or case-fold on darwin.

### 6. LOW: `readRows` silently drops a top-level `cases` key

`cmd/agent-mem/eval_relation.go:64` and `:206-208`

- `relationHeaderKeys` includes `"cases"`. A model that writes the relation_labels shape (`{"cases": {...}}`) instead of rows has every row discarded.
- relation-assemble then writes all rows as `insufficient/empty_evidence/missing`, and relation-check passes it.

Fix: drop only the seven scalar header keys, and refuse (`row_shape`) when `cases` is present.

### 7. LOW: name regexes can match shield placeholders

`internal/redact/pseudonym.go:118-135` against `:215-219`

- Placeholders encode the shield index in the letters `a`-`j`, and step 3 is case-insensitive.
- Probe: `AddPerson("U1","E")` turns the fifth shielded `PAY-5` into `person-1` and leaves raw private-use characters in the output.
- This loses evidence but leaks nothing. Realistic names ("Ed", "Bea") need 44 or more shields.

Fix: use a placeholder alphabet outside `\w` (e.g. more private-use code points).

### 8. LOW: the labelled-ref token has no trailing boundary

`internal/redact/pseudonym.go:51`

- `[A-Za-z0-9-]{3,39}` stops at 40 characters.
- Probe: a 46-character token after `ref` leaves `90XYZ12` visible, and the remainder is too short for step 7.

Fix: require a trailing `(?:[^A-Za-z0-9-]|$)` boundary, or alias the whole run.

### 9. LOW: lead relevance seeds differ from the relation seeds, and two doc sentences are wrong

- `cmd/agent-mem/eval_relevance.go:338` excludes seeds by `canonical_seeds`. For a reply seed, the lead hop-0 root has a different id, so it counts toward precision@10, while sheets and relation-check treat hop 0 as the seed. The brief says "existing metric code", so this only needs a note in the doc.
- `docs/ai/2026-10-11-graph-context-phase0-eval.md`, resolve-lead paragraph: it says a failing lead run gets `--quiet` category `lead_off` or `decode`. Non-2xx and transport failures report `HTTP <n>` or a transport error.
- The same doc, line 189, and `eval_prepare.go:348`: `BUILD_REV` is the eval host's env, not the server's. Say "client-side". In Part B it will almost certainly read `unknown`.

### 10. LOW: `seed_mismatch` fires only after model labelling

`cmd/agent-mem/eval_relation.go:176-179`

- An ACL-dropped seed passes `prepare` and `label-sheet`, and only fails at B6, after the Opus and pi rounds.
- Run the same check in `prepare` (or in `validateLeadReplay`) so B1 fails fast. Keeping it in relation-assemble, the worker's call, is correct.

### 11. LOW: relation-check checks that `adjudication` and `build_rev` are present, not their values

`cmd/agent-mem/eval_relation.go:344-355`

- `adjudication: "anything"` passes. So does a `build_rev` that differs from replay.json.
- An empty `snapshot_sha256` on both sides also passes, because `validateLeadReplay` does not require it.

### 12. LOW: test weaknesses (none hides a current defect)

- `eval_lead_test.go:1182`: the "no snapshot dir left" assertions cannot fail for lead-outcome errors, because the dir is created only after every request has succeeded. The real `RemoveAll` cleanup (`eval_prepare.go:375,379`) has no test.
- `TestLeadLabelSheetCapsAfterMasking` (`eval_lead_test.go:486-506`) cannot tell mask-then-cut from cut-then-mask. Put a 14-character ref across the 200 or 600 boundary.
- `testdata/relevance_golden.txt` arrived in the same commit as the code. The old-variant output paths are textually unchanged, so the risk is low, but its "before the change" origin is unproven. A one-off base-binary run on the fixture would confirm it.

## Requirements verified

- **A1:**
  - `buildEvalRequest` deep-copies the `resolve-asker` body (recursive map and slice copy) and sets `include_bodies:false` and `max_items:30`.
  - Headers are asker plus `X-Graph-Lead: 1`, with no admin header.
  - `askerQuery` is used, not `caseQuery`.
  - `TestLeadRequestDoesNotMutateSharedBody` uses a shared anchor map.
- **A2:**
  - The outcome enum, legacy decoding (no reader checks the outcome for old variants), the required `artifacts` array, unique non-empty ids, the integer hop (1.5 and "1" are rejected), the confidence default and the `authored_by_bot` enum all hold.
  - The items carry ids, enums and numbers only.
  - `eligibility_sha256` uses one shared function on both sides: marshal, `UseNumber`, re-marshal with sorted keys.
  - `prepare` fails with `prepare: lead_<outcome> <case>` before any write.
  - `validateLeadReplay` runs in all three readers and refuses a missing variant, an outcome other than `ok`, non-2xx, missing or bad items, and a recomputed-digest mismatch (including an empty stored value). The label-sheet snapshot header is compared against the recomputed value.
- **A3:**
  - Target `Lstat`, parent `EvalSymlinks`, refusal inside the repo (but see finding 5), `Mkdir 0700`, and `O_CREATE|O_EXCL|O_WRONLY 0600`.
  - The snapshot is written before replay.json is renamed in, and removed on any later failure. So replay.json never points at a missing snapshot.
  - `build_rev`, the worker's call: the server never sends `X-Graph-Build-Rev`, so this falls back to the env var or `unknown`. Acceptable under A3 (doc wording: see finding 9).
- **A4:**
  - `unreachable_must` is excluded from the lead denominator only.
  - `roots` and `seed_groups` are reported.
  - Warm-up and runs share `evalCallOK`. `warmup_ok` and `runs_ok` are counted.
  - Old-variant paths are unchanged.
- **A5:**
  - Step order matches.
  - The labelled ref needs a digit.
  - The tail rule fires after the real `capRunes(600)` cut.
  - `AddPerson` links id and name.
  - `Mask` runs after unshield.
  - Every spec example in A8 passes.
  - The `redact.go` refactor keeps `Mask` output identical for server callers.
- **A6:**
  - `--variant all` matches today's output except the `## [r<k>]` heading. The worker's call to refuse `--mask` and `--snapshot` there is fine.
  - `resolve-lead` needs `--mask` and `--snapshot`, checks all three digests and every snapshot row, uses one Pseudonymizer per case with `AddPerson` first, and applies `Apply` to the full text before cutting.
  - `source_thread` is replaced by the case id.
  - The Seeds block has no `[r<k>]`.
  - The manifest holds ids only.
  - The header text matches.
  - Errors are fixed categories.
- **A7:**
  - The header is computed from replay and a fresh case hash. The worker's call to drop model header keys is fine, but see finding 6.
  - The merge rules hold.
  - Unknown case or node ids give exit 1.
  - The output is `O_EXCL 0600`.
  - relation-check covers: the duplicate-key walker (objects and arrays, trailing data), the exact header key set, `DisallowUnknownFields`, version, input_mode, the three digests plus a fresh case hash, the exact case and item sets, the enums, disputed or missing rows forced to `insufficient`, and `seed_mismatch` including `thread_root`.
  - Its output is counts only.
- **A8:** every bullet maps to a test. The `validateLeadReplay` matrix runs all 12 mutations through all three readers with exact categories and unchanged digests. Sentinels are planted in all six places and checked in stdout, stderr and every output file.
- **A9:** paragraphs exist for resolve-lead, `--snapshot`, `--variant/--mask`, both relation commands, the rubric and the buckets.
- **Privacy:**
  - No new path prints customer text. JSON errors are never wrapped.
  - The pre-existing old-variant `prepare` transport error can echo a search URL containing the query (`eval_prepare.go`, `%w` of `*url.Error`). It is outside this diff but worth one line when convenient.
- **Acceptance 1-2:** vet and the redact tests pass. `TestEvalDryRunLeadVariant` passes in-process. `make test-db` is left to the conductor.
