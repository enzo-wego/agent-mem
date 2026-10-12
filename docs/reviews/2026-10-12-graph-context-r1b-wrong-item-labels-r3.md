# R1b wrong-item labels plan review, round 3

**Verdict: CHANGES REQUIRED**

Artifact: `docs/ai/2026-10-12-graph-context-r1b-wrong-item-labels.md`, draft r3.

- Artifact SHA-256: `8042c273ca17e6989b11250441189c498e9c699ebf83f5a3077153cd3b429d62`.
- Inspected repo HEAD: `58b96244e7bfcd95de507208b1b862b55cd063cc`.
- Inspected origin/main: `288d1a4ab4775c4b859bc9aefbc125eb145ce272`.
- Baseline: the supplied r1 and r2 reports. Round-N scope is the seven open entries and the specified r3 delta only. Closed entries are carried forward, not reopened.

Five open entries are now fixed in the proposed contract. Two remain open, both narrowed to validation at the final relation-label gate. No new finding IDs are introduced. “Fixed” means the plan now specifies the repair, not that implementation or tests have passed.

## Remaining findings

### R1-F3 — HIGH / Verified contract gap: recompute eligibility at the reader, not just at prepare

**Location:** plan A2/A6/A7/A8, especially lines 278–285.

A2 now defines a suitable eligibility digest, and the cohort buckets are exhaustive: structural takes precedence, high SAME_TOPIC precedes low SAME_TOPIC, other kinds and empty links have explicit buckets; author values have their own cross-tabulation. The global target is explicitly sampling-only. These parts are resolved.

The remaining gap is the verification operation. A7 requires each label digest to equal the digest **stored in replay**, but never requires recomputing `eligibility_sha256` from the current replay items. A6 says the digests must “all match,” explicitly rehashing case bytes, but does not unambiguously require rehashing eligibility metadata either. A8's “digest changes when confidence changes” can be satisfied by a hash-function/prepare test; its “eligibility mismatch” can be satisfied by altering a header string. Neither requires rejecting changed metadata with unchanged digest headers.

**Concrete failure:** prepare and adjudicate a SAME_TOPIC item at confidence 0.95. Change only that replay confidence to 0.50, leaving all recorded digests, IDs, hops, labels and case bytes unchanged. Every explicit A7 comparison still passes, but the item moves from `same_topic_hi` to `same_topic_lo`. The final gate therefore still permits labels to accompany a different eligibility cohort.

**Refutation checked:** the snapshot contains the original digest, so a reader that actually recomputes and compares against it could detect the mutation. Merely comparing the three stored copies cannot. B6 runs relation-check directly, and B7 explicitly mentions R1c checking the snapshot hash, not rehashing eligibility. An earlier successful sheet generation does not validate the later replay consumed by B6/R1c. Existing code supplies no implicit new-field validation: current replay decoding is ordinary JSON unmarshalling (`cmd/agent-mem/eval_prepare.go:293–306`).

**Required change:** state that lead readers recompute the A2 canonical digest from the loaded replay items and compare it with replay's stored digest and the relevant snapshot/label digest. Make R1c's handoff require the same validation (directly or through relation-check). This does not require relation-check to read private text.

**Required regression:** mutate only confidence, endpoint or author enum after prepare, preserving every stored digest and label. Both the lead-sheet reader and final relation-check must refuse with a fixed category. Keep the existing digest-generation test too.

**Evidence:** an executed, synthetic, in-memory Python probe using sorted-key compact JSON and SHA-256 returned:

```text
metadata-only mutation: stored label/replay digest equality = True
metadata-only mutation: recomputed eligibility equality = False
```

This demonstrates the specification's missing check, not behavior of an implementation that does not yet exist. Severity remains HIGH because this is the binding used to certify the downstream scoring population; no claim is made that an actual replay has been modified.

### R1-F10 — MEDIUM / Verified contract gap: strict lead replay checks stop at label-sheet

**Location:** plan A6–A7, lines 248–292.

A2's required response shape fixes permissive live decoding; A6 now explicitly refuses missing/failed lead variants, missing items and missing snapshot rows. Legacy `all` and unknown-variant behavior are also defined. Those parts are resolved.

A7 still does not require `resolve-lead.outcome == ok`, successful HTTP status, or complete, shape-valid lead metadata before assembly/checking. Its exhaustive label coverage and seed checks do not substitute for those checks. The round-2 request explicitly covered relation-check as well as sheets.

**Concrete failure:** take an otherwise valid prepared replay and complete labels, retain all items/hops/digests, but set the lead outcome to `decode_error` (or remove it). A2 hashes `{case_id: items}`, not the outcome. Case coverage, item coverage, seed consistency, enum checks on label rows and all three digest comparisons still satisfy A7. A6 would refuse this replay, while the advertised final gate can certify it. An analogous retained-items replay with an HTTP error is not explicitly rejected either.

**Refutation checked:** prepare's all-or-nothing success rule protects newly generated output, not independently invoked file readers. B6 and Acceptance 4 invoke relation-check without rerunning the private sheet reader. The new seed check rejects some missing-data cases but cannot reject an outcome-only change with intact seed items. The current generic replay structs/readers do not implement these proposed guarantees (`cmd/agent-mem/eval_prepare.go:23–39,293–306`).

**Required change:** make a common strict lead-replay validation contract a prerequisite for A6 and both A7 commands: expected cases and variant present; explicit successful outcome/status; required items field and valid item metadata; unique IDs; no legacy fallback for lead. Keep private snapshot-byte/membership validation in readers that have the snapshot. Preserve old-mode compatibility.

**Required regression:** invoke relation-check directly on valid labels paired with replay variants whose outcome is missing, `lead_off`, `http_error` or `decode_error`, plus missing/malformed lead items. Preserve otherwise matching digest headers and seed/items where applicable. These must fail; a shape-valid successful empty array remains allowed subject to the existing seed consistency rule.

The same synthetic probe confirmed an outcome-only mutation leaves the eligibility digest unchanged. This is a verified missing contract guard, not a claim about live server state.

## Updated convergence ledger

| ID | Status | Round-3 disposition |
|---|---|---|
| R1-F1 | fixed, carried | Separate relation rubric and denominators; not reopened. |
| R1-F2 | **fixed** | A3 saves author name and Slack ID; A5 links them through `AddPerson`; A6 registers all authors before masking. Equal-value aliases remain case-scoped. Non-author ID/name ambiguity and duplicate display names are expressly accepted limits, not unsupported identity guarantees. |
| R1-F3 | **open, narrowed** | Digest construction and exhaustive cohort reporting fixed; reader-side metadata rehash is still unspecified. |
| R1-F4 | **fixed** | Snapshot author producer exists; shield-first precedence resolves protected-token overlap; digit-bearing multi-word labelled refs avoid consuming ordinary prose; digit-bearing upstream-cut tails have an explicit rule and real-cap test. Other residuals remain under the stated owner authorization, not a universal privacy guarantee. |
| R1-F5 | fixed, carried | Private lifecycle and retention boundary; not reopened. |
| R1-F6 | fixed, carried | Existing case labels/baseline unchanged; not reopened. |
| R1-F7 | fixed, carried | Separate relation labels preserve case digest; not reopened. |
| R1-F8 | fixed, carried | Separate labels and lead recall treatment; not reopened. |
| R1-F9 | fixed, carried | Worktree generation preserves hub isolation; not reopened. |
| R1-F10 | **open, narrowed** | Strict live decode and sheet reader fixed; final relation commands still lack explicit strict replay preconditions. |
| R1-F11 | fixed, carried | Observable measurements and unexplained residuals; not reopened. |
| R1-F12 | **fixed** | B1/Acceptance use built binary, worktree and explicit URL; A4 separates 20 warm-ups from 200 measured runs; named synthetic dry-run test and immutable `PART_B_BASE` remove the previous execution ambiguities. No online success is asserted. |
| R2-F1 | **fixed** | A7 requires exact case/non-seed coverage, fresh case hash, duplicate-key rejection, schema/input-mode checks, insufficient disputes/missing rows, and seed consistency. The remaining provenance checks are tracked under F3/F10, not duplicated here. |
| R2-F2 | **fixed** | Models receive masked sheets plus ID-only inventory and output rows only. Assembly owns provenance; pi reads independently; optional owner override and missing/disputed handling are explicit. |

No new minor finding is added in previously reviewed sections. Next round should verify only R1-F3, R1-F10 and their resulting delta.

## Evidence and scope receipts

- `graph: not-applicable: plan artifact with no changed code symbols`
- `fan_out: inline: Round-N delta`
- Changed-symbol inventory: none (documentation artifact).
- Source checks: `leadFields` and `capRunes` (`internal/graph/handlers/lead.go:49–74`); actual author producer and item response (`lead_resolve.go:181–190,313–321`); replay production/decoding (`cmd/agent-mem/eval_prepare.go`); live warm-up and measured-run structure (`eval_relevance.go:217–275`).
- Traces: live response → private snapshot and tracked metadata → pseudonyms/sheets → independent rows → assembly → final validator → R1c handoff. Compared sibling reader refusal rules and persisted digest/outcome consumers.
- Lifecycle scope: fresh successful prepare, failed prepare, legacy reader input, metadata-only drift, outcome-only drift, partial model rows and final independent validation. No implementation lifecycle simulation or application test suite was run; this is plan review.
- Privacy: manual scoped pass; no dedicated scanner. No `.env` reads, secrets, database queries, server evaluations, or testdata reads. No customer text or external model call was used.
- No source edits, commits, pushes or task-tracker mutations. The requested report is the only retained review file; the temporary coverage manifest was removed.
- Coverage validator: **`OK: coverage valid`**. Full-body ledger validation rejects finding IDs in prose (a posting rule incompatible with this requested local convergence report). A ledger-block-only retry then hit the installed validator's shell error `line 997: WARNINGS[@]: unbound variable`; therefore full ledger validation is blocked, not claimed successful. Embedded JSON was parsed successfully; temporary validation files were removed.
- No PR exists for this artifact; user-supplied local reports are the explicit convergence baseline. GitHub identity/history/posting stages are not applicable. No plugin manifest resolved; `producer` is omitted rather than invented.

<details><summary>review-ledger:v1 (round 3 — REQUEST_CHANGES) · expand for review state</summary>

```json
{
  "round": 3,
  "reviewed_sha": "58b96244e7bfcd95de507208b1b862b55cd063cc",
  "base_sha": "288d1a4ab4775c4b859bc9aefbc125eb145ce272",
  "verdict": "REQUEST_CHANGES",
  "reviewer_agent": {"harness":"pi","model":"openai-codex/gpt-6-astra","source":"env","harness_version":"1.1.0","effort":"low"},
  "findings": [
    {"id":"R1-F1","sev":"HIGH","loc":"plan: Relation rubric","title":"Retrieval labels do not establish relation truth","status":"fixed","note":"Carried from r2; not reopened."},
    {"id":"R1-F2","sev":"HIGH","loc":"docs/ai/2026-10-12-graph-context-r1b-wrong-item-labels.md:198","title":"Alias identity needs an identity-linking producer and API","status":"fixed","confidence":"verified","evidence":[{"kind":"static","path":"docs/ai/2026-10-12-graph-context-r1b-wrong-item-labels.md:198-259","conclusion":"Author/Slack ID snapshot fields feed AddPerson before masking; unresolved identity cases explicitly limited."}]},
    {"id":"R1-F3","sev":"HIGH","loc":"docs/ai/2026-10-12-graph-context-r1b-wrong-item-labels.md:278-285","title":"Eligibility binding lacks a required reader-side metadata rehash","status":"open","confidence":"verified","evidence":[{"kind":"static","path":"docs/ai/2026-10-12-graph-context-r1b-wrong-item-labels.md:278-285","conclusion":"Checker compares stored digest values, not the loaded metadata's recomputed digest; confidence-only drift passes listed comparisons."}],"note":"Cohort partition and digest construction fixed; require rehash in readers and downstream handoff."},
    {"id":"R1-F4","sev":"HIGH","loc":"docs/ai/2026-10-12-graph-context-r1b-wrong-item-labels.md:213-247","title":"Masking specification cannot meet its safety/evidence tests","status":"fixed","confidence":"verified","evidence":[{"kind":"static","path":"docs/ai/2026-10-12-graph-context-r1b-wrong-item-labels.md:198-247","conclusion":"Author producer, protected-token precedence, digit-bearing labelled references and upstream-cut handling are now specified; residual exposure is expressly bounded."}]},
    {"id":"R1-F5","sev":"MEDIUM","loc":"plan: A3/A6/B7","title":"Confidential output lifecycle incomplete","status":"fixed","note":"Carried from r2; not reopened."},
    {"id":"R1-F6","sev":"HIGH","loc":"plan: Non-goals","title":"Freeze-test baseline points at wrong revision","status":"fixed","note":"Carried from r2; not reopened."},
    {"id":"R1-F7","sev":"MEDIUM","loc":"plan: Non-goals","title":"Relabelling invalidates saved case digest","status":"fixed","note":"Carried from r2; not reopened."},
    {"id":"R1-F8","sev":"MEDIUM","loc":"plan: A4/Relation rubric","title":"Append collisions and lead recall mismatch","status":"fixed","note":"Carried from r2; not reopened."},
    {"id":"R1-F9","sev":"HIGH","loc":"plan: Part B","title":"Hub isolation and unsafe cleanup conflict","status":"fixed","note":"Carried from r2; not reopened."},
    {"id":"R1-F10","sev":"MEDIUM","loc":"docs/ai/2026-10-12-graph-context-r1b-wrong-item-labels.md:266-292","title":"Strict lead replay preconditions missing from relation commands","status":"open","confidence":"verified","evidence":[{"kind":"static","path":"docs/ai/2026-10-12-graph-context-r1b-wrong-item-labels.md:248-292","conclusion":"A6 requires explicit ok outcome/items; A7 omits those preconditions, so an outcome-only mutation preserves all its listed coverage/digest/seed checks."}],"note":"Live decoding, sheet reader and legacy all behavior fixed; share strict replay validation with final gate."},
    {"id":"R1-F11","sev":"MEDIUM","loc":"plan: Part C","title":"Measurements overstate causal explanations","status":"fixed","note":"Carried from r2; not reopened."},
    {"id":"R1-F12","sev":"MEDIUM","loc":"docs/ai/2026-10-12-graph-context-r1b-wrong-item-labels.md:345-355","title":"Final acceptance cannot execute as specified","status":"fixed","confidence":"verified","evidence":[{"kind":"static","path":"docs/ai/2026-10-12-graph-context-r1b-wrong-item-labels.md:345-355","conclusion":"Part B records immutable base and uses built worktree binary with explicit server URL."},{"kind":"static","path":"docs/ai/2026-10-12-graph-context-r1b-wrong-item-labels.md:408-425","conclusion":"Named synthetic test, separate warm-up/measured totals and Part B diff baseline replace previous ambiguous assertions."}]},
    {"id":"R2-F1","sev":"HIGH","loc":"docs/ai/2026-10-12-graph-context-r1b-wrong-item-labels.md:278-292","title":"Validator does not require complete ground truth","status":"fixed","confidence":"verified","evidence":[{"kind":"static","path":"docs/ai/2026-10-12-graph-context-r1b-wrong-item-labels.md:278-292","conclusion":"Exact case/item coverage, fresh case hash, duplicate keys, schema/dispute and seed constraints are required."}]},
    {"id":"R2-F2","sev":"MEDIUM","loc":"docs/ai/2026-10-12-graph-context-r1b-wrong-item-labels.md:266-277","title":"Sheet-only model cannot construct provenance header","status":"fixed","confidence":"verified","evidence":[{"kind":"static","path":"docs/ai/2026-10-12-graph-context-r1b-wrong-item-labels.md:266-277","conclusion":"Models supply rows only; assembler computes provenance and merge."},{"kind":"static","path":"docs/ai/2026-10-12-graph-context-r1b-wrong-item-labels.md:358-364","conclusion":"Both independent reviewers receive the same masked sheets and ID manifest, not raw provenance/text."}]}
  ],
  "coverage": {
    "graph":"not-applicable: plan artifact with no changed code symbols",
    "fan_out":"inline: Round-N delta",
    "value_reach":"ran: traced eligibility digest, lead outcomes, identity registration and adjudication states through A2-A8 and Part B",
    "execution":"ran: in-memory synthetic digest mutation probe only; no application tests or server evaluations",
    "security":"ran: manual scoped privacy pass over author snapshot fields, shielding, masking and model input boundaries; no dedicated scanner",
    "cut":"not-applicable: documentation-only plan review",
    "ticket":"not-applicable: user-supplied plan and seven-entry prior ledger define scope; no PR or ticket",
    "contracts":"ran: static plan-to-repo producer/consumer checks; no runtime or external-state claims",
    "tests_audit":"ran: mapped revised A8 requirements to seven open findings and current source; tests are proposed, not implemented"
  }
}
```

</details>

**Final verdict: CHANGES REQUIRED.**
