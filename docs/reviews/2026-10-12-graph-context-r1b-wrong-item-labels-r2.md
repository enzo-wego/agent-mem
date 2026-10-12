# R1b wrong-item labels plan review, round 2

**Verdict: CHANGES REQUIRED**

Artifact: `docs/ai/2026-10-12-graph-context-r1b-wrong-item-labels.md`, draft r2.

- Artifact SHA-256: `76f936b98e335025a7a4c7fd665b7bb4d5e2adb734efd58acc90a18ac8cc8c26`.
- agent-mem HEAD: `58b96244e7bfcd95de507208b1b862b55cd063cc`; origin/main: `288d1a4ab4775c4b859bc9aefbc125eb145ce272`.
- p-agent HEAD: `41c7a35dadfe04a31320541ec40830ec8e812a3c`.
- Convergence baseline: supplied `docs/reviews/2026-10-12-graph-context-r1b-wrong-item-labels-r1.md`. This is a plan review, not a PR or implementation review. “Fixed” below means fixed in the proposed contract, not implemented or tested.

No source edits, commits, pushes, credential reads, database queries, server evals, or customer-data reads were performed. No testdata files were opened. Dataset counts remain owner-supplied facts. Only the requested report is retained. A small in-memory synthetic regex probe and the review coverage validator ran; no application test suite ran.

## Summary

The separate relation-label file is a substantial improvement. It closes the label-semantic, freeze-baseline, stale-case-digest and append-collision problems. The revised evidence snapshot also removes the raw-body fallback mismatch.

The plan is not yet a deterministic handoff. Its snapshot lacks the author data required by its masker, its reference grammar can consume ordinary words and leave short references behind, and its new validator can accept incomplete labels without binding eligibility metadata. The final online acceptance also points at a hub checkout deliberately left on the old revision. These are contract defects, not claims about actual customer data.

## Convergence ledger against r1

| Finding | Status | Round-2 disposition |
|---|---|---|
| R1-F1 | **fixed** | Separate `same_case / related_context / different_case / insufficient` rubric, examples and distinct scoring denominators preserve retrieval-label semantics. |
| R1-F2 | **open, narrowed** | Per-case aliases, insufficient evidence and dispute exclusion fix the main design. A5/A8 still cannot guarantee one alias per person, and grammar precedence can destroy comparison evidence. See below. |
| R1-F3 | **open, narrowed** | Actual lead title/summary and confidence/endpoints/provenance are now captured. Binding of eligibility metadata and exhaustive cohort accounting remain incomplete. |
| R1-F4 | **open, narrowed** | Explicit residual authorization and masked query/use-case address major gaps. Missing author producer, short-reference grammar and already-truncated server text remain. |
| R1-F5 | **fixed** | New private parent, fresh exclusive output paths, explicit retention owner and vendor-retention caveat fix the principal lifecycle boundary. The plan does not promise vendor deletion. Session cleanup is explicitly best effort; do not upgrade that into a no-persistence claim. |
| R1-F6 | **fixed** | No relabelling or freeze-baseline move; B1 stops if canonical seeds change. |
| R1-F7 | **fixed** | Case bytes stay unchanged; separate labels do not invalidate `cases_sha256`. |
| R1-F8 | **fixed** | Separate labels avoid old-list collisions; A4 explicitly excludes reply-only must IDs from lead recall while preserving old-mode recall. |
| R1-F9 | **fixed** | Generation moves to the worktree; no hub pull/discard sequence remains. The conflicting final verification location is covered under R1-F12, not reopened as unsafe cleanup. |
| R1-F10 | **open, narrowed** | Numeric status/additive outcome and union default are fixed. Readers still lack explicit missing/failed/malformed lead refusal rules. |
| R1-F11 | **fixed** | Fresh node metadata, response-time bot enums and unexplained residuals replace unsupported causal claims. Actual population counts remain unverified. |
| R1-F12 | **open, narrowed** | Redaction gate, synthetic dry-run, text-only scans and better run counters are improvements. Exact final commands, warm-up semantics and a viable verification checkout remain unresolved. |

No old finding is silently dropped. New findings below concern the new r2 validator and model-input handoff, not minor discoveries in unchanged code.

## Remaining findings

### R1-F2 — HIGH / Verified contract gap: the promised alias identity has no identity-linking API

**Plan:** A5–A8, lines 159–179 and 214–219.

`AddName(name)` registers only a string. A raw mention `<@U123ABC>`, normalized `@Ann Lee`, and `Ann Lee` in prose have unrelated lookup keys after lowercasing and removing hyphens/spaces. Nothing associates the Slack ID with that name. A8 nevertheless asks for “one alias per person.” Sequential numbering is not identity resolution. The real response has `author_slack_id` (`internal/graph/handlers/lead.go:49–56`) and `author` (`lead_resolve.go:313–318`), but A3 saves neither.

**Required:** either define a private identity registration operation carrying ID plus display-name variants and supply that mapping, or explicitly narrow the guarantee to equal normalized strings, allowing different representations of a person to remain unlinked. Do not assert person equality that the input cannot establish. Keep the reference-equality tests independently strict.

**Check:** one person represented by ID/name/mention and two distinct people with the same display name; specify expected aliases or explicit ambiguity. No live name lookup is required during sheet generation.

### R1-F4 — HIGH / Verified contract defects: the specified mask cannot meet its own safety and evidence tests

**Plan:** A3/A5/A6/A8, lines 144–151, 159–190, 214–223.

Three concrete remaining failures:

1. **Missing author producer.** A3 permits only `{title, lead_summary}` per snapshot item. A6 registers “author names from the snapshot items.” There is no author field to read. The live response does provide `author` (`lead_resolve.go:181–190,313–318`), so this is a missing allowlisted snapshot field, not unavailable server data. Registered-author sentinel tests cannot be fulfilled by the declared schema.
2. **The labelled-ref rule consumes words as references.** A5 accepts alphabetic tokens after a label and permits whitespace alone as delimiter. Under its stated order, `booking reference AB1234` consumes `reference` as the value of `booking`; the remaining six-character `AB1234` does not qualify for the unlabelled rule. `payment failed with timeout` consumes `failed`, deleting symptom evidence. `payment PAY-2420` and `ref 2026-10-12` match despite the separate instruction to preserve Jira keys and dates. These overlaps require an explicit precedence rule, not an implementer's guess.
3. **“Full text before truncation” is only locally true.** `leadText` already caps summaries to 600 runes before A3 receives them (`internal/graph/handlers/lead.go:68–74`). The server's `Mask` intentionally preserves booking/payment references. An unlabelled mixed reference crossing that cap can arrive as a short fragment plus ellipsis, which A5 does not recognize. Applying the new mask before the sheet's second cap cannot recover the missing suffix. A8's crossing-600 test must exercise this real pipeline, not only an untruncated synthetic string.

**Required:** add the minimal private author fields or remove the unsupported registration claim; define multi-word label syntax, protected-token precedence and treatment of ordinary words. For upstream-truncated reference fragments, choose an explicit conservative tail-redaction/insufficient policy or explicitly authorize this residual exposure. The current “outside the grammar” caveat does not explain that an originally in-grammar value can become an unrecognized fragment upstream. Do not fetch full raw node text to quietly substitute different detector evidence.

**Executed synthetic probe:** Python `re.finditer` with the literal A5 label/token grammar produced:

```
'payment failed with timeout' => [('payment', 'failed')]
'booking reference AB1234' => [('booking', 'reference')]
'payment PAY-2420' => [('payment', 'PAY-2420')]
'ref 2026-10-12' => [('ref', '2026-10-12')]
```

This verifies grammar overlap, not a future Go implementation. No customer input was used.

**Check:** add those four cases plus the server-cap→snapshot→sheet boundary case and author-field schema round trip. Verify both non-disclosure and preserved same/different evidence.

### R1-F3 — HIGH / Verified contract gap: snapshot binding still does not bind the eligibility cohort

**Plan:** A2/A3/A7 and readiness, lines 134–151, 193–204, 277–282.

The private snapshot hash covers query/title/summary, but not replay `hop`, `links`, confidence or bot enums. Labels only carry the case and snapshot hashes. Replacing a replay's confidence, endpoints or bot provenance while retaining IDs and the same snapshot passes the stated A7 checks. R1c would then apply unchanged labels to a different eligible cohort without detecting it. R1's request to bind the replay itself is still unimplemented.

The counts also do not finish the R6/R8 handoff. `structural`, `same_topic_only`, and `no_link` do not cover a THREAD-only item or a nonstructural SAME_TOPIC+THREAD item; this repo explicitly emits THREAD links. A7 does not say whether counts are cross-tabulated by relation, how mixed kinds are classified, or how bot exclusions and low-confidence SAME_TOPIC are reported. Global 15/6/30 readiness can be met entirely outside the judge cohort. The parent A5 judges a much narrower population (p-agent parent plan lines 233–246; downstream review R6/R8).

**Required:** bind labels to finalized replay bytes or to a canonical digest of all eligibility metadata, and require R1c to validate that digest. Define exhaustive, mutually understood cohort buckets with label totals, including mixed, THREAD-only/other, no-direct-link, bot/unknown and below-threshold SAME_TOPIC. State that the global target is only a sampling target, not live-cohort readiness; unsupported cohorts stay inconclusive. R1c can still own detector-union arithmetic and final selection.

**Check:** metadata-only mutation invalidates the binding; mixed/THREAD-only/empty-link fixtures each have a defined bucket; a global target met only by ineligible items must not produce a ready judge cohort.

### R1-F10 — MEDIUM / Verified contract gap: additive compatibility does not define strict lead readers

**Plan:** A2/A6/A7, lines 134–143, 180–204.

Prepare now refuses non-ok lead results. That protects newly completed runs, but A6 and A7 do not explicitly reject legacy replays with no lead variant/items, failed lead variants, or missing snapshot rows. “Legacy replays still decode” is not permission to use them as lead truth.

The current response parser accepts `{}` as a successfully decoded empty response (`cmd/agent-mem/eval_common.go:242–284`); typed JSON decoding alone does not establish that a lead payload has its required fields. A2's `decode_error` gives no required-shape contract. A non-lead 200 without the off header can therefore be interpreted as an empty or partially populated successful lead response by a literal extension of the current parser.

**Required:** define the required lead response shape, including a legitimate empty artifacts array, item IDs and required metadata; reject malformed/missing fields using fixed categories. Keep legacy decoding for old modes, but require successful lead outcome, complete metadata and matching snapshot membership for lead sheets and relation-check. Explicitly reject unknown variants.

**Check:** legacy replay works for `all` but fails for `resolve-lead`; missing variant, off/failed variant, `{}`, absent artifacts, duplicate IDs and a missing snapshot row fail; a valid empty lead array succeeds where appropriate.

### R1-F12 — MEDIUM / Verified: final acceptance cannot run as written

**Plan:** Acceptance 2–3/6–7 and Verify, lines 310–332; A4, lines 153–158.

- Acceptance 3 runs `eval relevance --quiet` **on the hub**, while the plan explicitly leaves that checkout and its main revision unchanged. The existing CLI has three variants and resolves testdata relative to cwd (`eval_common.go:25`, `eval.go:14,52–56`). Running the old binary cannot produce lead results; running `$PRIV/agent-mem` from the hub still uses the old hub node metadata. The command also omits mandatory `--base-url`, whose default is empty (`eval.go:56`, `eval_relevance.go:151–154`).
- A4 does not say whether run counters include warm-up, but Acceptance 3 requires 11 runs per case. Today the warm-up is not parsed/scored; only ten measured runs are counted (`eval_relevance.go:217–275`). A successful ten-run aggregate can conceal a failed/off warm-up unless the change explicitly includes it.
- Acceptance 6 compares against local `main`, although the branch is created from merged remote main. A stale local main can include Part A in that diff; after updating main to the Part B merge the diff can instead be empty. The comparison needs an immutable Part B base captured before generation.
- Acceptance 2 names an “A8 fixture” as a CLI dataset but supplies no fixture path or data-directory flag. Current CLI uses the cwd-relative constant. An in-process test can supply `DataDir`, but that is a different check and should be named as such.

**Required:** run both before/after counts with the exact built binary in the worktree, pass the same explicit base URL and credentials environment, and preserve hub isolation. Define warm-up success accounting or change the assertion to 200 measured runs plus a separate 20-warm-up assertion. Capture `PART_B_BASE` before generation. Specify whether the synthetic dry-run is a named Go test or a CLI command against a created fixture root. Real graph drift may change failure counts; record that as inconclusive rather than treating identical code as proof of identical live results.

**Check:** the final command sequence is executable from the named cwd without changing the hub; an off/failed warm-up cannot satisfy the complete-run gate; the three-file comparison is stable before and after merge.

## New delta findings

### R2-F1 — HIGH / Verified: relation-check does not validate the advertised complete ground truth

**Plan:** A7/B3/B6, lines 193–204, 254–269.

A7 checks each supplied label's membership but never requires every non-seed returned item to have a label. A file with valid headers and `cases: {}` satisfies the enumerated membership/enum checks vacuously. A partially written draft can therefore pass the only Part B validation gate while violating Goal 4 and B3's “every non-seed lead root.” Missing truth is not the same as explicit `insufficient`.

It also compares labels' `cases_sha256` only with replay, not with the currently loaded case-file bytes. Knowing that case IDs exist does not establish those bytes are unchanged. Two mutually matching stale artifacts can pass. A7 names `build_rev`, version, adjudication, input mode and `check`, but does not explicitly define required exact values/compatibility constraints; at minimum a `disputed` row must not be accepted as a decisive same/different label.

**Required:** require exact case coverage and exact per-case non-seed ID coverage (empty maps allowed only for genuinely empty candidate sets), reject extra/missing/duplicate keys, compare current case bytes with replay and label digests, and validate the schema/version/input-mode and dispute constraints. Define seeds from replay canonical seeds with a hop-zero consistency check. Missing evidence gets an explicit `insufficient` entry, never an omitted row.

**Check:** empty file, one omitted case, one omitted item, stale current case bytes, duplicate JSON key, decisive disputed label and wrong input mode fail with fixed categories. A fully covered file containing explicit insufficient rows passes and reports their coverage loss.

### R2-F2 — MEDIUM / Verified: the sheet-only drafter cannot fill the required provenance header

**Plan:** A6/A7/B3, lines 180–204, 254–256.

B3 gives Opus only sheets and the rubric, prohibits case/snapshot access, and requires output in A7's complete schema. A6 does not put `cases_sha256`, `snapshot_sha256` or `build_rev` on sheets. These values cannot be inferred from masked prose or IDs. An isolated implementer must invent a header, violate the access boundary, or deliver a JSON file that cannot pass B6.

**Required:** let the drafter output only per-case row labels, then have the conductor/CLI insert mechanically verified provenance; alternatively provide a content-free manifest containing the exact required header and sorted case/row inventory. Do not have a model calculate or invent hashes. Include the replay binding required by R1-F3.

**Check:** run the drafting fixture with exactly the allowed inputs; final assembly fills provenance without accessing customer text or trusting model-generated hash strings, and relation-check passes.

## Downstream checklist assessment

| Requirement | Round-2 status |
|---|---|
| R3: inventory/query/revision | Sorted discovery and exact asker-query path are documented; full hash validation and model provenance handoff still need R2-F1/F2. The implementation must not reuse current `caseQuery`, which selects the admin query first (`eval_prepare.go:372–383`). |
| R6: scoring population | Unique per-case ID maps are suitable; complete coverage, exhaustive kind/cohort accounting and metadata binding remain open. Union scoring itself remains R1c work. |
| R7: truth semantics | The new rubric resolves the must/may reinterpretation. Remaining blockers concern whether masked evidence preserves the distinctions and whether every item is represented. |
| R8: equivalent inputs/cohort | Actual server title/summary capture is a sound fix. Exact replay metadata binding, strict reader validation and explicit cohort coverage are still required before claiming transferability. |

## Acceptance audit

1. **Part A gates:** feasible; the new redaction test command closes the prior execution gap. No tests are claimed green here. Give the changed-file scan an actual file list rather than the dangling `rg` invocation.
2. **80 synthetic requests:** feasible once the fixture invocation is specified (R1-F12); use the asker query rather than the legacy admin-first helper.
3. **Complete lead runs / old failures:** not executable as written (R1-F12). Requires an owner online run after correction, not performed here.
4. **relation-check:** currently too weak to certify the promised artifact (R1-F3/F10 and R2-F1/F2).
5. **Sheet scans:** mechanically checkable as a narrow regex audit, not proof of safety. Names, short refs, URL-contained refs and wrapped/multiline fields are not certified by those three patterns. A5/A8 plus explicit residual authorization remain the actual boundary (R1-F4).
6. **Exactly three Part B paths / no scratch files:** feasible with a captured immutable base; path checks do not certify content. Enforce allowlisted serialization and synthetic sentinel tests separately.
7. **Hub unchanged:** feasible and desirable; keep verification in the worktree to avoid contradicting it.
8. **Part C/results/issue:** feasible as owner-run aggregate measurements. No live counts or issue creation were attempted. The issue priority must use the stated non-seed population consistently.

## Review receipts and scope limits

Deep-review applied in Round-N mode against the supplied local R1 report. PR posting identity/history and implementation-diff discovery are not applicable; no GitHub calls were made. No new minor findings were added in unchanged code. The new R2 findings arise from newly specified r2 interfaces.

- `graph: not-applicable: plan artifact with no changed code symbols`
- `fan_out: inline: Round-N delta`
- Changed-symbol inventory: none; this is a plan artifact.
- Static traces: request construction → lead response → private snapshot → pseudonymization → sheets → adjudication → validator → R1c; outcome/legacy reader paths; hub/worktree verification paths; current warm-up/run accounting.
- Lifecycle checks: fresh snapshot, legacy/missing snapshot, partial labels, metadata-only drift, rerun requiring fresh paths, and final verification against an unchanged hub.
- Privacy: manual pass only; no dedicated scanner or external model call. Server-side persistence was not claimed absent or probed. No new no-trace contract is inferred from the eval header.
- Tests audit: A8 and acceptance mapped to current source. Future tests listed above are requirements, not execution evidence.
- Coverage validator: **`OK: coverage valid`**; temporary manifest removed.
- Reviewer identity script returned `{"harness":"pi","model":"openai-codex/gpt-6-astra","source":"env","harness_version":"1.1.0","effort":"low"}`. Installed standalone skill is `deep-review` 1.2; no plugin provenance is asserted.

The convergence table is the persistent plan-review ledger: seven R1 findings are closed, five remain narrowed, and two new r2 interface findings are open. Next review should verify only those seven open entries and the resulting plan delta.

**Final verdict: CHANGES REQUIRED.**
