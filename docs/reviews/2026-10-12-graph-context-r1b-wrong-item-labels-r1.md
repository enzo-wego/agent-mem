# R1b wrong-item labels plan review, R1

**Verdict: CHANGES REQUIRED**

Reviewed `docs/ai/2026-10-12-graph-context-r1b-wrong-item-labels.md` (draft r1) as the artifact, not a PR or implementation diff.

- Plan SHA-256: `4cec902d332e33dd5811a5ad4cab70c89c396748d2da397c4d7b58adc0d2d752`.
- agent-mem inspected HEAD: `58b96244e7bfcd95de507208b1b862b55cd063cc`; origin/main: `288d1a4ab4775c4b859bc9aefbc125eb145ce272`.
- p-agent inspected HEAD: `41c7a35dadfe04a31320541ec40830ec8e812a3c`.
- Requirements: supplied p-agent umbrella and detector review checklist, particularly R3/R6/R7/R8.

No source edits, commits, pushes, server evals, database queries, model calls, or credential reads were performed. No testdata replay, node metadata, fixture content, or case text was read. Dataset counts in the plan were therefore not independently certified. Source and synthetic-test code were read; no application test suite was run. This report is the only retained file created by this review.

## Summary

The proposed labels are not yet sufficient ground truth for R1c. They mix retrieval usefulness with same-case identity, erase the identifiers needed to distinguish incidents, require guesses on insufficient evidence, and are generated from text different from the live lead payload. Root counts alone cannot establish detector coverage.

There are also concrete execution blockers: Part B points the freeze test at the pre-label commit, copies a replay whose case digest becomes stale, and leaves the hub dirty before pulling the merged changes. Several acceptance checks either cannot pass literally or cannot prove the property they claim.

All findings below concern the proposed contract or a reachable source interaction, not an assertion that prohibited live inspection revealed customer data.

## Findings

### R1-F1 — HIGH / Verified: retrieval labels still do not establish the detector's ground truth

**Plan:** A2, B3–B6, target (`:86–89`, `:144–171`). **Checklist:** R7, also R6.

`must` and `may` receive no new operational definition. The existing contract explicitly says `may` is neither right nor wrong (`docs/ai/2026-10-11-graph-context-quality.md:130–131`); relevance excludes it from the precision denominator (`cmd/agent-mem/eval_relevance.go:339–350`). B3 introduces `same_component_context` alongside `same_case`, but does not say which labels these imply, or whether a useful different-incident precedent should be retained or rejected by a same/different judge. Its `must_not` rule can reject precisely such a useful precedent merely because the occurrence differs.

A target of 60 combined `must`/`may` roots can be satisfied entirely by `may` additions. It establishes neither a trustworthy good-item denominator nor a same-case positive cohort. Calling results provisional does not repair the semantic mismatch.

**Required change:** define the prediction target and an explicit rubric with contrasting synthetic examples. Prefer separate dimensions for retrieval usefulness and relationship truth, such as `same_case`, `valid_related_context`, `different_case`, and `insufficient_evidence`. Specify how each maps to detector 1, detector 2, and each reported denominator. Preserve existing relevance semantics; do not promote historical `may` entries to proven good items. Define the p-agent handoff schema, including how R1c distinguishes newly adjudicated roots from old labels.

**Check:** synthetic useful-other-incident, same-incident, unrelated, and ambiguous examples produce predetermined labels and scoring dispositions. Count confirmed positives, negatives, optional context, and excluded/unknown examples separately.

### R1-F2 — HIGH / Verified: masking destroys the evidence needed to label different occurrences

**Plan:** A3, B3–B4 (`:94–99`, `:144–158`). **Checklist:** R7.

Every matching reference becomes the identical string `[ref]`. Thus a query about synthetic ref `ABC123DEF456` and a candidate about `XYZ789UVW012` becomes indistinguishable from a candidate repeating the first ref. That is exactly the distinction B3 asks reviewers to judge. Seeds are excluded from sheets, and the only candidate evidence is a short excerpt; blank, missing, truncated, or genuinely ambiguous items are nevertheless forced into `must|may|must_not`.

An Opus draft and pi review of the same information-losing view do not restore the missing evidence. The automatic “pi wins if it supplies a code” rule does not establish truth, and optional owner inspection of the same masked rows cannot resolve erased identifiers.

**Required change:** preserve equality and inequality without exposing original values, for example with per-case, namespace-aware aliases shared across the query, seed context, and all candidate rows (`booking-1`, `booking-2`). Keep the mapping private and ephemeral. Add an explicit insufficient-evidence outcome and exclude it from R1c's truth denominators. Specify how an authorized adjudicator resolves a dispute and how unresolved cases affect readiness; a reason code alone is not adjudication evidence.

**Check:** same-reference and different-reference inputs remain distinguishable after masking; two namespaces do not alias accidentally; absent/truncated evidence never gets forced into a truth label. No raw alias mapping is written.

### R1-F3 — HIGH / Verified: lead sheets and saved metadata cannot reproduce the R1c live cohort

**Plan:** Goal 1, A4–A5, B2–B3 (`:14–15`, `:102–106`, `:142–152`). **Checklist:** R3/R6/R8.

Changing candidate IDs to roots does not change the sheet's evidence source. `readSheetNode` (`eval_prepare.go:388–405`) reads the raw DB title and the first 300 characters of `artifact_index.summary`, falling back to body text. Live lead mode uses a Slack thread-summary title fallback (`internal/graph/handlers/lead_resolve.go:181–190`) and `lead_summary` from the artifact summary, masked then capped at 600 runes, with no body fallback (`lead.go:68–74,288–326`). Consequently the reviewers can see a body the bot never receives, miss a title the bot does receive, and miss the latter half of its summary.

The proposed replay stores only hop and unique kinds, not confidence, endpoints, bot provenance, activity, or an eligibility classification. The umbrella A5/A11 cohort depends on those fields: SAME_TOPIC confidence, structural links to seeds, bot filtering, and judging only eligible Possibly-related survivors of detector 1. Twenty negatives among all returned roots could yield no negatives in the judge's actual cohort. Kinds and hop help R6 but are not sufficient for R8.

**Required change:** specify a snapshot-bound evidence contract using the actual returned title and `lead_summary` and exact nested request `requests['resolve-asker'].body.query`. If additional authorized evidence is needed for truth adjudication, separate it clearly from detector-visible evidence. Keep sensitive snapshots outside the repo. Save enough allowlisted numeric/enum/ID metadata to reconstruct eligibility, and bind the handoff to case digest, replay digest, server build, input mode, and adjudication version. Define how later graph/text drift invalidates or limits a label; rerunning resolve is not the original snapshot.

Report unique non-seed `(case_id,node_id)` coverage by label and eligible kind/cohort, including mixed kinds, no-direct-link items, absent labels, unknowns, and excluded bots. Shortfall is allowed to be honestly inconclusive, not allowed to authorize live drops. Detector-union contingency logic remains R1c work, but R1b must supply its usable population and truth contract.

**Check:** fixtures distinguish body-only content, empty summaries, title fallback, content after character 300, low-confidence SAME_TOPIC, mixed structural/SAME_TOPIC, bot/unknown provenance, and no-direct-link roots. Root-only discovery must not be treated as proof of live eligibility.

### R1-F4 — HIGH / Verified: the masking specification and tests miss supported sensitive inputs

**Plan:** A3/A8, B3, risk paragraph (`:90–101`, `:117–121`, `:144–145`, `:217–220`).

There are several concrete gaps:

- `redact.Mask` deliberately protects labelled booking refs of 5–20 characters (`internal/redact/redact.go:46–48` and its protected-span handling). A synthetic `booking: AB1234` survives it and is too short for A3's 10-character mixed-token mask. The supported F-reference pattern is also not covered by a guaranteed grammar. “Booking/payment refs” is therefore materially broader than the specified mask.
- `readSheetNode` truncates in SQL before the proposed masks run (`eval_prepare.go:392`). A token crossing character 300 can become a short fragment that none of the masks recognizes. Existing live `leadText` deliberately masks before truncation to avoid this (`lead.go:68–74`).
- Slack storage is normalized to display names under AGENTS.md. Replacing raw `<@U…>` tokens does not remove ordinary normalized `@Display Name` mentions. The residual-name caveat acknowledges some exposure, but the header “people” and sentinel test do not establish person masking. Author-field aliasing alone does not remove the same name from prose.
- Sheets already include use case, source thread, and query (`eval_prepare.go:347–348`). “Every text field” should include those explicitly and tests must exercise them. B3 additionally instructs the draft agent to read the raw case query, bypassing that masked view.
- “Token” boundaries, hyphenated/URL-embedded refs, namespace exceptions, and ordering between reference masking and mention masking are not specified. The implementer cannot derive one unambiguous function contract from the examples.

The umbrella's acceptance of retaining existing card/phone exports is not evidence of a complete policy permitting additional residual refs/names to an external reviewer. No actual exposure was probed; the verified defect is the incomplete proposed boundary.

**Required change:** enumerate accepted sensitive grammars and fields, mask complete strings before excerpting, and use one masked query/context view for both reviewers. Use an accurate header listing actual coverage and residuals. Either redact normalized/free-text names to the approved standard or require explicit owner authorization for that exact residual-data/provider boundary before any model run; do not infer it from “masked sheets.”

**Check:** synthetic sentinels in every header/text field; short labelled booking refs; hyphenated, URL-contained and boundary-crossing tokens; U/W mentions; normalized names; permitted Jira/PR/date examples. Confirm both non-disclosure and preserved comparison utility from R1-F2.

### R1-F5 — MEDIUM / Verified: confidential output handling is not a complete lifecycle

**Plan:** A8, B2–B4/B6, Acceptance 4–5 (`:119–121`, `:142–167`, `:201–205`).

Current sheet writes use `MkdirAll(...,0700)` and `os.WriteFile(...,0600)` (`eval_prepare.go:359–364`). They do not tighten an existing permissive directory/file or refuse an existing file symlink. Validating only the output directory does not validate a symlinked `<case>.md`. A fresh-temp happy-path mode assertion therefore cannot establish the promised boundary on reruns.

The plan also gives no permissions/retention rules for labels JSON, pi notes, the approval page, or model/pane transcripts. “Never commit” is not “no persistence.” `git diff --stat` cannot detect confidential content inside an allowed file, untracked sheets, ignored files, or copies outside the scanned checkout. Actual source queries also appear in ordinary dry-run output (`eval_relevance.go:208`), so the required real-data dry-run should not be sent unfiltered to an external review pane.

**Required change:** require a newly created private scratch directory, exclusive/non-symlink file creation or explicit safe overwrite handling, and fixed error categories on the customer-data workflow's stdout/stderr. Define who may read each artifact, where the approval page is hosted, retention/deletion ownership, and the actual model-session persistence policy. Run request-rendering checks on synthetic cases or inspect sensitive output locally and report only counts. Replace the diff-stat “content proof” with schema/allowlist checks of generated artifacts plus tracked/untracked path checks, without printing rejected content.

**Check:** existing 0644 file, existing permissive directory, symlink target, failed write, rerun, malformed input/error sentinels, and cleanup after success/failure. Do not describe local file cleanup as proof of vendor-side deletion.

### R1-F6 — HIGH / Verified: the freeze-test baseline points at the wrong version

**Plan:** A7 and B5 (`:109–111`, `:163–165`).

`TestEvalCaseLabelsUnchangedFromBase` compares all non-request bytes against the selected commit (`eval_acl_test.go:193–246`). The parent of the commit that introduces the new labels contains the old labels. Pointing `evalLabelsBase` there makes every actual relabel fail the test; it cannot “guard the new labels.” The comparison also covers label reasons and canonical seeds, not just labels.

**Required change:** prescribe an achievable baseline sequence. For example, make a reviewed label-data commit first, then a separate test-baseline commit pointing to that data commit, with final combined-branch gates; explicitly handle squash/rebase policies so the referenced object remains available in CI. Alternatively use a reviewed committed snapshot/digest with an explicit scope. Do not use a moving ref or compare the working tree to itself.

**Check:** the final branch passes against the new frozen data, then a synthetic label/reason/seed mutation fails while an allowed request-only edit passes. Verify baseline availability in the actual CI checkout mode.

### R1-F7 — MEDIUM / Verified: copying B1 replay after relabelling invalidates its digest

**Plan:** B1/B5 and post-merge Acceptance 4–5 (`:133–141`, `:159–165`, `:201–205`).

`casesSHA` hashes the full case bytes (`eval_common.go:70–77`), and prepare stores that digest before B5's edits (`eval_prepare.go:180–196`). B5 changes labels and reasons but copies the B1 replay unchanged. Afterwards `label-sheet` rejects it at `eval_prepare.go:305–306`; the existing harness test explicitly verifies refusal after any case edit (`eval_harness_test.go:583–587`). This blocks the post-Part-B masked-sheet acceptance run.

Simply rerunning prepare obtains a new graph sample that might not be the sample just labelled.

**Required change:** explicitly finalize the digest for the reviewed final case bytes without silently resampling. Preserve the original preparation/adjudication provenance separately, and verify that only the permitted label/seed changes occurred. Alternatively define a versioned digest contract separating immutable request inputs from label revision, with reader migration tests.

**Check:** a synthetic prepare → relabel → finalize → sheet cycle succeeds; an unrelated request edit still fails. Saved returned IDs must remain those actually adjudicated.

### R1-F8 — MEDIUM / Verified: append-only label application and shared resolve metrics need reconciliation rules

**Plan:** A2/B5 (`:86–89`, `:159–165`).

B5 says lists “gain” roots and “nothing removed.” If a root already has `may` but its new adjudication is `must_not`, appending puts it in both lists. The validator rejects precisely this (`eval_common.go:440–450`). The plan supplies no collision rule, including for changed canonical seeds. This is a supported-input specification gap; no claim is made that a particular real root collides today.

Keeping reply `must` labels in the same set creates another known mismatch: lead mode cannot return those replies, but existing recall divides by every `must` ID (`eval_relevance.go:362–376`). Adding root equivalents leaves unachievable recall targets or counts two representations of one logical item. Preserving old-mode metrics is a reasonable goal, but it requires a separate lead denominator/cohort, not an unqualified shared denominator.

**Required change:** define idempotent upsert/reclassification for existing roots, preservation rules for legacy replies, deduplication, canonical-seed exclusion, and a validated enum-only reason map for new labels. Specify lead recall separately: root-normalized relevant truth with conflict handling, or a distinct lead label set. Keep legacy route semantics unchanged.

**Check:** old-may/new-negative, old-positive/new-negative, duplicate roots, root/reply pairs, a newly canonical seed, and applying the same finalized labels twice. No contradictory membership or impossible lead recall denominator remains.

### R1-F9 — HIGH / Verified specification conflict: hub isolation is contradicted and pull/cleanup ordering is unsafe

**Plan:** Part A worktree instructions and B1 (`:74–77`, `:133–141`).

Part A says never touch the hub, while B1 rebases it, rewrites tracked testdata, and later discards the entire `testdata/` working tree. This exception must be explicit and owned by the conductor, not left for an implementer to infer.

More importantly, B1 leaves replay, metadata, and case files modified in the hub until *after* the Part B merge is pulled. That pull/rebase must operate over dirty tracked paths also changed upstream; there is no clean-transfer step or explicit autostash policy. The final `checkout -- testdata/` restores the index, not “main,” and can discard unrelated local testdata changes. Copying output to a worktree is not a verified backup or ownership check. Actual inspection confirmed the reusable worktree is detached at `288d1a4` and the hub is on `main` at `58b9624`; it did not verify future cleanliness or safety.

**Required change:** keep generated data in an isolated checkout/scratch dataset, with an explicit data-dir/env-path interface or protected conductor-only credential setup. If hub writes remain necessary, specify a preflight on the exact generated paths and index, a private backup/checksum manifest, exclusive ownership, verified transfer, restoration of only those owned modifications *before* pull, and final verification of both merged data and preserved local commits. Never blanket-discard `testdata/`. Define interruption/retry handling and the exact fresh branch/base for Part B.

**Check:** rehearse with a disposable synthetic Git repository: unrelated dirty file, local commit, generated dirty files, interruption after generation/copy, pull of overlapping changes, and rerun. All unrelated work survives and no application checkout is left in an ambiguous state.

### R1-F10 — MEDIUM / Verified: two proposed CLI contracts contradict the current representation/behavior

**Plan:** A1/A4/A5 (`:79–85`, `:102–106`).

1. Replay `status` is currently an integer HTTP status (`eval_prepare.go:23–27`). A1 requires the string `lead_off`, while A5 says to change nothing else in the schema. A string cannot be decoded by existing `replayVariant.Status int` readers. There is no defined migration or failure/continuation policy for prepare, nor a rule refusing sheets from an off/missing/failed lead variant.
2. Today's label-sheet candidates are the deduplicated union across **all** eval variants (`eval_prepare.go:337–344`), not `resolve-asker`. Making that the default does not “keep today's behaviour”; it silently hides admin/search-only candidates. Existing tests use the same candidate lists for the variants and would not detect this regression.

**Required change:** retain numeric HTTP status and add an explicit outcome enum, or deliberately version/migrate the schema and every reader. Define lead-off preparation, output, and sheet refusal behavior. Preserve union as the default (for example `all`) or explicitly approve/document a breaking default change. Validate unknown and absent variants.

**Check:** legacy replay decoding, lead-off HTTP 200, missing/failed lead variant, unknown variant, and three variants returning disjoint IDs. Assert that deriving the lead request does not mutate the map later used by plain resolve.

### R1-F11 — MEDIUM / Verified: Part C cannot establish the promised explanations from its specified evidence

**Plan:** C1/C2 (`:175–183`).

C1 says “from replay,” but replay does not map plain reply IDs to roots; that requires `node_meta.json`, whose `--refresh` deliberately preserves old entries (`eval_prepare.go:154–176`). Even with fresh mappings, plain and lead select from differently constrained populations: plain uses body-budget hydration (`resolve.go:377–382`), while lead groups scored candidates before `max_items` selection (`lead_resolve.go:65–143`). A delta of final counts cannot attribute missing groups to deletion, scope, ACL, epic filters, folding, or the two selection budgets. Treating folding plus root filtering as the measured explanation overstates the available evidence.

C2 asks how many B1 lead roots were `unknown`, but A5 does not persist `authored_by_bot`. A later aggregate DB query cannot prove the exact response-time value. Its provenance is `p.id IS NOT NULL` plus bot membership (`lead.go:199–207,288–299`), not merely whether a person has a Slack ID. Bot-list population changes bot-vs-human classification for known authors; by itself it does not repair missing author links causing unknown.

**Required change:** either narrow C1 to observable overlap/folding counts and report an unexplained residual, or define read-only evidence for each causal bucket, including selection caps. State freshness and denominators. Save B1 response-time provenance enums/counts for C2, distinguish missing-author linkage from known-person bot classification, and specify the sample population and issue-priority thresholds. Do not guess source/channel-type columns or infer null equivalence without defining the query/schema checks.

**Check:** synthetic cases where only body budget, only max-items, only folding, and only root eligibility change counts; unknown/human/bot examples with and without Slack IDs. Report causal attribution as unavailable when the evidence cannot distinguish it.

### R1-F12 — MEDIUM / Verified: the test gates and acceptance checks do not prove the advertised properties

**Plan:** A8, Gates, Acceptance, Verify (`:112–129`, `:193–213`).

- `make test-db` executes `go test` only for `./internal/graph/... ./cmd/agent-mem/...` (`Makefile:63–64`). It does not execute new `internal/redact` unit tests. `go vet ./...` is not a replacement. Add `go test ./internal/redact/...` or broaden a gate explicitly.
- Acceptance 4's blanket prohibition on 8+ digit runs conflicts with A6's raw node IDs: ordinary Slack node IDs contain long timestamp digit runs. Masking those IDs would break the ID handoff. Scan designated human-text fields, not identifiers/structural URLs, and explicitly define permitted ID fields and residual risk. Checking `<@U` also misses W mentions and normalized names.
- Acceptance 3's `failed=0` is not proof that all lead requests succeeded. Current relevance marks a case failed only when all ten measured runs fail (`eval_relevance.go:284–291`). One success plus nine failures can satisfy that aggregate. Require the intended successful-case/run counts and no `lead_off`/failed runs, or document an explicit tolerated failure policy.
- URL-keyword grep cannot prove absence of titles/customer text. A title need not contain `http`, `slack.com`, or `atlassian`; `http_403` is a valid safe category that contains `http`. Use synthetic sentinel tests across stdout/stderr and an allowlisted output schema rather than treating keyword grep as a confidentiality proof.
- Acceptance 5's diff-stat check cannot establish content absence (R1-F5), and “target met or real number reported” is a reporting condition, not an R1c readiness threshold. Results must mark unsupported cohorts inconclusive.
- “Existing tests unchanged” cannot be literal alongside A7's required test edit and A6's row-format change: the sheet test currently asserts `## <node_id>` (`eval_harness_test.go:555–558`). Identify expected assertion changes while preserving their underlying coverage.
- The R1a `after.txt` baseline has no reproducible path, digest, invocation, or revision in the handoff. A no-context implementer cannot verify the final comparison. Supply an accessible counts-only baseline with provenance, or explicitly mark that check blocked/inconclusive. Graph drift means unchanged metric values cannot be guaranteed merely by unchanged code.

**Required change:** turn each acceptance item into a command/assertion with an exact scope and result, separating synthetic safety tests, live owner evidence, and R1c readiness. Include the lifecycle tests in R1-F6–F10, not just the successful fixture path.

## R1c checklist handoff assessment

| Checklist item | R1b status | Required handoff |
|---|---|---|
| R3: inventory, query location, input revision | Partially addressed by naming 20 files and using existing sorted discovery. No finalized digest/provenance protocol; request query path is implicit. | Sorted case-ID inventory, exact nested query path, final input/label/replay hashes, loaded/adjudicated counts. |
| R6: combined gate and population | Hop/kinds are useful, but total roots do not establish eligible labelled denominators. Union scoring itself belongs to R1c. | Unique non-seed units, cohort membership, separate positive/negative/optional/unknown counts, mixed-kind/absent handling, inconclusive rules. |
| R7: trustworthy good/wrong labels | Not closed. `may` remains neutral and useful context differs from same-case truth. Masking erases distinguishing evidence. | Explicit target/rubric, identity-preserving pseudonyms, adjudicated truth dimensions, unresolved exclusions. |
| R8: live-equivalent inputs and cohort | Not closed. Roots match shape, but sheet body fallback/300-char text and missing eligibility metadata do not match live lead use. | Actual title/lead-summary input mode, eligibility metadata and snapshot binding; rerun policy after drift. |

R1b need not implement R1c's detectors, union arithmetic, cancellation, or SDK isolation. It must not advertise labels as satisfying those gates without the ground-truth and population contract above.

## Acceptance audit

1. **A8/tests/vet:** feasible after fixes, but current commands omit redaction unit-test execution. No passing test claim is made here.
2. **80 dry-run requests:** feasible on synthetic data; check identity and body overrides without printing real queries to reviewers. Include no-mutation parity for old variants.
3. **Hub quiet run:** owner-only online verification, not performed. Add complete-run assertions; distinguish safe fixed error codes from content leaks.
4. **Masked hub sheets:** currently cannot pass the literal digit grep with raw Slack IDs and cannot prove adequate masking. Also blocked after B5 by the stale replay digest.
5. **Labels/freeze/no repo text:** currently blocked by the baseline/digest rules, possible label conflicts, and an invalid diff-stat privacy oracle. Counts alone do not certify R1c readiness.
6. **Part C/results/issue:** aggregate reporting is possible, but attribution and response-time unknown counts require the revised evidence contract. No bd mutation was performed during this review.

## Review coverage and limits

Deep-review was applied as a standalone Round-1 plan review. PR identity, PR history, posting, and implementation-diff discovery are not applicable. The supplied earlier detector review was treated as an explicit downstream requirements document, not as a prior verdict on this artifact.

- `graph: not-applicable: plan artifact; no changed code symbols or implementation diff`
- `fan_out: inline: no subagent tool`
- Value/lifecycle traces: proposed label semantics, replay status/digest, sheet generation, freeze baseline, branch transfer, rerun/interruption behavior, and consumers in the supplied p-agent design.
- Privacy pass: manual masking/input/output/file-lifecycle review; no dedicated security scanner available or run.
- Tests audit: proposed A8 behavior mapped to existing source tests and actual Makefile execution scope. New implementation tests do not yet exist.
- External contracts: local CLI/replay readers and supplied p-agent requirements examined; deployment state, actual dataset coverage, and actual returned content remain unprobed.
- Execution: no application tests or runtime/server claims. State/lifecycle defects above are static contract findings; synthetic fixtures named in the findings are required future checks, not tests claimed to have run.
- Coverage validator executed on a temporary manifest, then removed: **`OK: coverage valid`**.

**Final verdict: CHANGES REQUIRED.**
