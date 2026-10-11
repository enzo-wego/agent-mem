# Phase 1b masking plan review, round 2

**Verdict: CHANGES REQUIRED**

Reviewed draft r2 against checkout `e2d7c40`, the round-1 review, the Phase 1 ACL plan and the parent quality plan. This is a design review, not an implementation review. D2 remains fixed. No code or plan edits, DB queries, secrets inspection, hub calls, deployment, or implementation-test claims. Only this requested report was written. All examples below are synthetic.

The response-level architecture is sound, and the replay and fail-closed revisions resolve their principal round-1 problems. Two detection rules still permit plainly labelled personal data to escape. These need correcting before presenting the detection envelope to Enzo for acceptance.

## Blocking findings

### R2-F1 — HIGH, verified specification defect: token boundaries reject ordinary labelled and formatted personal data

**Plan:** `docs/ai/2026-10-12-graph-context-phase1b-masking.md:85–101`.

The start boundary explicitly excludes colon and equals, even when the preceding text is a recognized personal-data label. As specified, these complete field values remain readable:

- `card:4111111111111111`
- `phone:81234567`
- `tel:+6581234567`

The first is a Luhn-valid synthetic PAN; the second satisfies the labelled local-phone length; the third satisfies the international-phone rule. None can start a candidate after its colon. The labelled rules do not override the shared boundary rule. This recreates the numeric portion of round-1 F1 without a node-ID regex. The claim that such punctuation means “part of an identifier” is not true in free text.

Ordinary markup has the same problem: `` `4111111111111111` `` and `**+6581234567**` have neither allowed opening nor closing boundaries. These are standalone personal-data tokens, not numbers glued to letters or IDs. Markdown/code-formatted source text can pass directly through hydrated bodies (`internal/graph/hydrate/budget.go:37–132`); no response renderer strips that markup before the proposed masker. This is not a request to detect every obfuscated phone: it concerns the plan's supported numbers in routine punctuation.

**Evidence:** an executed synthetic probe of the exact specified boundary predicate rejected all five examples and accepted `card: 4111111111111111`. Thus a whitespace-only formatting change controls whether clearly labelled data is masked. There is no masker implementation yet; this proves the specification contradiction, not a runtime implementation result.

**Required change:** define label-aware candidates that can consume ordinary label separators (`:`, `=` and agreed punctuation) without granting that exception to arbitrary node-ID prefixes. Recognize common Markdown delimiters around supported numbers. Preserve complete known structural/reference tokens rather than inferring that every colon-prefixed number is an identifier. Define precedence explicitly; do not leave the implementer to choose between “labelled numbers mask” and “colon numbers never mask.”

**Regression tests:** each example independently as a unit case and as a sole free-text canary; include spaced/hyphenated PANs, `phone=81234567`, and inline-code/fenced-code bodies. Pair them with unchanged `cf:4111111111111111`, `datadog:monitor:4111111111111111`, Slack IDs and supported payment refs. These pairs demonstrate that the fix masks labelled text without breaking lookup IDs.

**Round-1 relationship:** F1's global value/key exemptions are removed, but the compact numeric bypass persists through the new boundary rule; F5's labelled coverage is consequently not yet fulfilled.

### R2-F2 — HIGH, verified specification defect: the booking-label proximity exemption protects nearby PANs and phones

**Plan:** `docs/ai/2026-10-12-graph-context-phase1b-masking.md:81–84`, protection before all detection.

“Any digit run” with a booking/ref label anywhere in the preceding 20 characters is substantially broader than a booking reference. For example:

- `booking 7 card 4111111111111111`
- `reference: phone +6581234567`

Both sensitive digit runs have a booking-label word within that window. Both also have an explicit, nearer personal-data label. Protection runs first and is “never masked”, so the labelled phone/card rules cannot correct the classification. In the first example, `7` is already the booking value, but its label also exempts the subsequent card. This does not depend on substring matches for `ref`: it happens even with proper word boundaries.

**Evidence:** an executed synthetic window probe confirmed the booking/ref word lies within the specified 20-character prefix for both sensitive runs. The first example alone establishes the issue independent of how a phone's leading plus interacts with protected-span overlap. These examples are normal payment-support prose shapes; no claim is made about their frequency in stored data.

**Required change:** bind a booking label to its own value using a specified grammar, rather than protecting every number in a vicinity. Consume one reference value and stop at intervening labels/clause boundaries. A nearer explicit phone/card label must not inherit a previous booking exemption. Define label word boundaries, separators and protection-overlap behavior. Continue preserving actual labelled numeric booking values and the complete supported payment refs.

**Regression tests:** the examples above must mask the personal number while retaining the booking value; test adjacent booking and phone/card fields on one line and across newlines, and a true Luhn-valid numeric booking reference that must remain intact. Keep the positive `F.`-reference/PAN-in-the-same-sentence test. A test containing only `booking 4111111111111111` cannot expose this defect.

**Round-1 relationship:** F2's supported `F.` collision is fixed, but its replacement booking exemption introduces this new under-masking path. F5 requires the corrected envelope, not this broader accidental exemption, to go to Enzo.

## Round-1 disposition

| Finding | R2 disposition |
|---|---|
| F1: key/node-ID exemptions | Key and whole-value exemptions removed; emails in IDs/URLs/seeds now explicitly masked. Numeric compact-label leakage remains through R2-F1. |
| F2: payment/booking ref collisions | `F.` protection fixes the concrete payment-reference collision. Booking intent is now explicit, but protection is too broad: R2-F2. |
| F3: masked replay/truncation/version | Resolved in design by versioning and declining replay for masked rows. No need to reconstruct raw truncation boundaries when comparison is not attempted. |
| F4: fail-closed coverage | Resolved in design: all statuses, unexpected content types, broken/full-document JSON and partial-write panics are covered. Add the missing panic regression test. |
| F5: detection envelope | Labelled rules and phone-before-card precedence are improvements. Envelope acceptance remains an explicit Enzo gate, as requested; R2-F1/F2 must be corrected first. This review does not reopen D2. |

## Facts audit against current code

1. **Five JSON-producing routes: confirmed.** `search.go:393,413`, `node.go:115`, `neighbors.go:587`, `epic.go:272`, `resolve.go:492`. Linked rows feed search display; cards feed neighbors. Resolve bodies come through `hydrate.Greedy`; resolve links extract metadata `why` at `resolve.go:736`. Epic brief arrays are `json.RawMessage` at `epic.go:46–48`.
2. **“Every one calls requireAsker first”: needs the same wording correction as r1.** All five guard graph reads. Resolve decodes JSON first (`resolve.go:184–194`) and can return plain-text 400 before the guard. Admin detection is exact header value `1` (`acl_guard.go:59`); the production resolver grants `noFilter` only on that branch. Replay deliberately constructs a separate unfiltered ACL, which is not a request authorization bypass.
3. **Other graph GETs admin-only: confirmed.** `router.go:22–65` installs default-deny GET/HEAD policy. POST resolve is explicitly registered and must explicitly receive masking. Neighbors is a mounted child (`router.go:154`, `neighbors.go:57–61`).
4. **MCP: confirmed for string contents, not byte-for-byte JSON.** All five methods require asker headers; none sends admin. `client.go:177–243` decodes the response into a map, preserving masked string contents. Probe is intentionally exempt from asker requirements and is not one of these graph reads.
5. **No existing graph personal-data masker: confirmed by code search of the relevant repository Go paths.** Existing masking functions found are settings credential masking and DB-URL credential redaction. No claim about masking in other repos or external callers.
6. **Payment shapes: confirmed with a small qualification.** `identifiers.go:29–30` matches the regexes quoted. Extraction at `:75–85` requires a digit for both payment and action matches and rejects payment-like words; a shared definition should preserve existing extraction behavior. `extractor.go:458–469` supports `F.` with a lowercase-alphanumeric suffix, including all-digit suffixes. No booking grammar was found in the graph identifier/extractor code. Do not interpret that absence as evidence that all booking references are safe from numeric detection.
7. **Email-bearing person IDs, seeds and child router: confirmed.** `ids.Person` emits `person:<email>` (`ids.go:167–168`); resolve echoes request seeds. R2 explicitly accepts losing follow-up by email-bearing ID/URL. Keep that acceptance visible rather than simultaneously promising all structural IDs are unchanged.
8. **Digit-heavy identifiers: shapes are real; universal survival is not established.** `ids.CFPage` and `ids.Datadog` use int64 natural keys, while Slack IDs embed timestamps and PR IDs embed numbers. Prefixed IDs survive the new numeric boundary rule. Bare numeric page/monitor IDs that are Luhn-valid PAN length do not. This is already acknowledged in Risks and should qualify Acceptance 1's blanket “page ids unchanged”. Numeric JSON values must remain exact on decode/re-encode; use `UseNumber` or an equivalent lossless representation. Current EEID/timestamp numeric fields are below the float64 exact-integer ceiling, but the generic middleware must not silently round larger values.
9. **“Traces store only hashes”: still false.** `context_trace.go:81–88,124–190,252–254` stores request/query/asker inputs, node IDs, resolve links including `why`, diagnostic metadata and error text as well as title/summary/body digests. The inner masker protects returned link text before trace extraction, but not original request inputs or all diagnostic metadata. D2 is response masking, not trace-at-rest sanitization; correct the statement without expanding scope.
10. **Replay unmasked / TEXT mode column: confirmed.** `trace_render.go:21–65` rehydrates raw text; hybrid replay uses an unfiltered ACL. `cmd/agent-mem/trace.go:101–130` currently compares per-item digests. `migrations/20261011000001_context_traces.sql:11` defines `acl_mode TEXT`, so the proposed new mode needs no migration. Old filtered replay can retain its existing behavior, but “matching digests” is a fixture expectation, not a guarantee for changed content/ACL-dependent rendering.
11. **Payment-reference comment hygiene:** retain r1's qualification. Comments include examples in more than the cited block; their real-world provenance and the org-wide ban are supplied context, not independently verified policy evidence. Replace all identified real examples without reproducing them in this report or new tests.
12. **Historical deployment and size/performance assertions:** accepted as supplied context or estimates, not verified by a hub call or production measurement. Source inspection alone does not establish a 100 KB response cap or the p95 impact.

## Route coverage and middleware order

`withContextTrace(maskGraphResponse(h), ...)` is the correct order for search and resolve. The trace wrapper buffers the masker's final output, inserts a generated `request_id`, then hashes it. That generated identifier is not free text and does not need masking. Reversing the wrappers would hash unmasked per-item content. `withContextTrace` returns its input unchanged when no recorder exists, so masking must remain active in that configuration too.

A recursive walk over every decoded string value covers all current typed content paths without key-based exemptions:

- **Search legacy:** title, summary, author, query and error strings.
- **Search hybrid/linked:** the above, plus purpose, decisions and their authors, open questions, root author, participants, PR titles and `linked_via_title`.
- **Node:** title, summary, body, author name and strings in edge references.
- **Resolve:** title, summary, hydrated body, author, link tag/topic/why, echoed seeds, URLs and cache-miss IDs.
- **Neighbors plain/cards:** title/overview, decisions, open questions, authors/participants, PR titles, edge tag/topic/why/verdict/verdict_why.
- **Epic:** root/member titles and statuses, brief, nested highlights/open-items text and citation-source strings.

`json.RawMessage` does not bypass a decoder operating on the completed response. Generated brief items are objects with `text` and `sources` (`refresh_epic_brief.go:265–276`), which become ordinary maps and arrays. There is no demonstrated current PII-bearing dynamic object-key path among these typed responses; the contract says string **values**, not arbitrary schema keys.

Wrap the actual child-router neighbors GET handler or its mounted router. Editing only top-level `r.Method` registrations misses it. No additional route-placement blocker was found.

## Fail-closed and trace implementation/test requirements

The new fail-closed rule is coherent. It intentionally converts existing plain-text errors, including resolve's malformed JSON and missing-seed errors, to stable 500 `mask_failed` for non-admin callers. That is a real compatibility effect, not just a hypothetical future content-type regression; document and test it. JSON ACL rejections can retain their status and safe error code.

Keep original bytes and headers private until masking succeeds. Remove/recompute stale `Content-Length` and invalid representation headers; do not leak a buffered partial body on panic. The acceptance cases cover malformed/trailing JSON and wrong content types, but omit the explicitly promised partial-write panic. Add that case and assert exact safe output and zero trace items.

For mode tagging, update a successful `filtered` decision after masking, without overwriting `rejected` or `not-evaluated` states indiscriminately. `requireAsker` writes the mode during handler execution (`acl_guard.go:220–231`), so setting a mode only before calling the handler would be overwritten. A mounted recorder test must establish the final mode.

For masked replay, branch before rendering or digest comparison, including masked/truncated rows and masked email-bearing node IDs that no longer resolve. A notice printed alongside continued comparison would not fix F3. Tests should assert the notice is present, comparison labels are absent, and the renderer is not required for masked rows. Preserve legacy/admin behavior on unchanged fixtures.

Per-item hash tests should independently hash the exact decoded title/summary/body returned to the client and compare them to recorded values. A mode-only test does not verify ordering. Raw stored request/asker fields remain outside the response-masking guarantee.

## Acceptance-test sufficiency

The new mounted-route and single-canary approach is materially better than r1, but the listed happy-path canaries would not catch R2-F1 or R2-F2. Add the exact punctuation and cross-label cases above before approval.

Also retain these non-blocking test clarifications from r1:

- Assert expected fixture rows and exact field paths exist, then assert placeholders. An empty result, dropped field or ACL-hidden epic brief must not pass merely because the canary is absent.
- Exercise `linked_via_title`, PR titles, `verdict_why`, decision authors, link tag/topic and epic sources, not just generic title/body fields. A generic recursive-walker test should include nested arrays and misleading keys such as `scope`, `id` and `url` to prevent reintroducing exemptions.
- Exercise admin `1` versus `true`, `0` and absent, body-only resolve asker, encoded neighbors IDs, and recorder-present/absent variants. Keep ACL-denial gates intact.
- Assert all non-string JSON data remains equal, including a synthetic integer above 2^53. Specify expected survival for realistic prefixed IDs separately from accepted bare-numeric false positives.
- Test full candidate consumption: too-long digit sequences must not yield a masked prefix or suffix, and grouped phones/cards, adjacent candidates and protected-reference neighbors must not leave numeric tails. Phone-before-card precedence is now specified correctly.
- Carry forward the named Phase 1 flaky tests and base comparison, rather than permitting unspecified new failures under “known flaky”.

## Execution receipt and handoff

Source tracing used file reads and targeted repository searches. Synthetic in-memory probes checked the specified boundaries and booking-label windows; no real customer content or payment references were queried. No implementation exists here to run masking tests against, and no claim of passing `make test-db` or `go vet` is made. No dedicated security scanner ran.

Graph: not applicable to changed-symbol analysis; this is a plan-only revision with no implementation diff. Fan-out: inline, no subagent tool. The round-1 report supplied by the user served as the convergence record. `bd prime` was read; task/database mutations, git commits and pushes were not performed under this read-only review request. Pre-existing untracked files were left untouched.

**Approval gate:** fix R2-F1 and R2-F2 and add their regression cases, correct the repeated trace-storage fact, then present the precise detection envelope and accepted remaining limits to Enzo. The middleware architecture, all-string recursion, supported `F.` protection, fail-closed direction and masked-replay notice do not need redesign.
