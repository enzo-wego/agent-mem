# Phase 1b masking plan review, round 3

**Verdict: CHANGES REQUIRED**

Reviewed draft r3 against checkout `e2d7c40`, the supplied r1/r2 reviews, the Phase 1 ACL plan and the parent quality plan. This is a specification review, not an implementation approval. D2 is fixed. The documented detection envelope is not reopened here.

The two concrete r2 defects are resolved in the revised specification. However, the new known-node-type protection reintroduces an email exemption that contradicts the plan's explicit promise to mask emails inside IDs. One blocking finding remains.

## Blocking finding

### R3-F1 — HIGH, verified specification defect: excluding only `person:` does not make protected node IDs email-free

**Plan:** `docs/ai/2026-10-12-graph-context-phase1b-masking.md`, Approach 2 and Approach 3, Step 1's node-ID protection followed by Step 2's email matching.

Step 1 copies recognized node tokens unchanged before email detection; only `person:` is excluded. The code does not constrain all other node types to email-free natural keys:

- `ids.Partner` lowercases its input and replaces spaces, without removing `@` or dots (`internal/graph/ids/ids.go:139–143`).
- `ids.Status` lowercases arbitrary input (`:154–156`).
- `ids.CodeFile` preserves an arbitrary repository path (`:164–165`).
- `ids.ParseType` validates the prefix, not the natural-key grammar (`:176–193`).

These synthetic complete tokens therefore satisfy the proposed protection while containing an email matched by the specified email regex:

```
partner:alice@example.com
status:alice@example.com
code_file:exports/alice@example.com.csv
```

They need not be actual graph IDs to leak. A free-text body, title, explanation or echoed query containing one is enough: the middleware has no schema/path restriction, and the proposal explicitly applies protected-span detection inside all text. Genuine constructor-produced IDs can have the same problem. Restricting the prefix to the 19 current node types does not solve it; validating against these permissive constructors does not solve it either.

**Evidence:** source inspection of the constructors and an executed synthetic probe using the node-type constants extracted from `ids.go` showed all three tokens protected and all three containing a supported email. The control `person:alice@example.com` was correctly not protected. This establishes the specification contradiction, not a claim about deployed customer data or an implementation that does not yet exist.

**Impact:** a supported ordinary email escapes a non-admin response. This is not an accepted spaced/obfuscated-email false negative, not a customer-name limitation, and not a request to change the numeric-ID preservation policy. Approach 2 explicitly says an email inside an ID is masked, accepting loss of follow-up by that ID.

**Required change:** make email detection take precedence over node-ID protection, or disqualify every email-bearing node token from protection, regardless of prefix. Do not use only `person:` as the proxy for “can contain an email.” Retain the numeric protection for genuinely email-free known IDs and the existing payment-reference behavior.

**Regression gate:** test each example independently as the entire string and embedded in prose, including a sole title/body/query canary on a mounted route. Expect the email placeholder, not merely absence of the original field. Pair with unchanged `cf:4111111111111111`, `datadog:monitor:4111111111111111`, Slack IDs and payment refs. Keep the existing `person:`, `mailto:` and URL-with-email tests. The current acceptance list tests only the special-cased `person:` ID, so it can pass while this defect ships.

**Convergence:** this is introduced by r3's new protected-node-token rule. R2 had removed node-value exemptions; no new finding in unchanged implementation code is being added.

## Prior-finding disposition

- **R2-F1: resolved in design.** Numeric candidates now allow colon, equals, backticks, Markdown asterisks and other punctuation. `card:4111111111111111`, `phone:81234567`, `phone=81234567`, `tel:+6581234567`, inline/fenced PANs and bold phones are explicitly required regression cases. Their labels are not known node types. Known `cf:` and Datadog IDs have separate protection instead of relying on punctuation to infer identity.
- **R2-F2: resolved in design.** Booking protection binds one value to a booking/order/PNR/itinerary label; bare `reference` no longer qualifies. `booking 7 card 4111111111111111` protects neither number under that grammar, so its PAN remains a candidate. `booking ref: AB12345 phone +6581234567` protects only `AB12345`, including when the phone follows on the next line. A synthetic grammar probe confirmed these spans and the positive numeric-booking case. Include the prior exact `reference: phone +6581234567` regression too.
- **R1-F1: key exemptions remain removed, but node-token protection partially regresses the email guarantee: R3-F1.** No key-name skip-list remains.
- **R1-F2: supported `F.` payment-reference collision remains fixed.** Full `F.[a-z0-9]+` protection precedes numeric detection, including all-digit suffixes. Preserve complete-token boundaries when implementing it.
- **R1-F3: masked replay remains resolved in design.** Version the mode and do not attempt raw re-render/digest comparison for masked rows. This avoids the pre-mask versus post-mask truncation-length mismatch.
- **R1-F4: fail-closed design remains resolved.** All statuses, unexpected content types, malformed/trailing JSON and partial-write panics are covered by the proposed behavior. The panic test is still missing from Acceptance 3; retain the earlier non-blocking request to add it.
- **R1-F5: the detection envelope remains an Enzo acceptance gate.** No objection here to the enumerated local-phone, non-Luhn, glued-letter, obfuscated-email or customer-name limitations, or the specified protected structural/reference shapes. This review does not require detecting every number inside an intentionally protected URL.

## Facts audit against current source

1. **Five JSON encoders: confirmed.** Search legacy/hybrid encode at `search.go:393,413`; resolve at `resolve.go:492`; node at `node.go:115`; neighbors at `neighbors.go:587`; epic at `epic.go:272`. Search linked rows feed the hybrid representation (`search_linked_threads.go:27–206`, `search_hybrid_display.go:60–188`). Resolve hydrates bodies through `hydrate.Greedy` (`resolve.go:316`, `hydrate/budget.go:37–132`), and extracts link `why` at `resolve.go:736`. Epic highlights/open items are `json.RawMessage` (`epic.go:46–48`).
2. **All five guard graph reads: confirmed; “requireAsker first” is not literally true.** Resolve decodes the body before the guard (`resolve.go:184–194`) and can return a plain-text 400 first. The other four guards are at the cited entry points. Request-time `noFilter` is granted only by the exact admin-header branch (`acl_guard.go:59,71–73`). Replay deliberately uses a separate unfiltered ACL.
3. **Other graph GETs admin-only: confirmed.** `router.go:22–65` installs default-deny GET/HEAD policy. POST resolve is separately registered at `:95`. Neighbors is a child router mounted at `:154`, with its actual GET at `neighbors.go:60`.
4. **MCP propagation: confirmed for string contents, not byte-for-byte JSON.** All five methods obtain asker headers; none sends admin. `client.go:177–243` decodes JSON into a map. Masked strings therefore survive without an MCP change. Probe is intentionally separate.
5. **No existing graph personal-data masker: confirmed by targeted Go-source search.** Existing credential masking is in `settings_handlers.go:51` and `database/postgres.go:35`. This is not a claim about external systems.
6. **Payment formats: confirmed with the existing qualification.** The regexes at `identifiers.go:29–30` match the plan. Extraction requires a digit for both payment and action refs, and applies an additional payment-word rejection (`:75–85`). A shared definition must preserve extraction semantics. `extractor.go:458–469` supports `F.` lowercase-alphanumeric suffixes, including all digits. No booking parser/format was found in the relevant graph identifier/extractor code.
7. **Person IDs/seeds: confirmed, but person is not the only email-capable type.** `ids.Person` is `person:<email>` at `ids.go:167–169`; resolve echoes caller seeds at `resolve.go:351`. See R3-F1 for the omitted types.
8. **Digit-heavy shapes: confirmed, not a universal survival guarantee for bare numeric strings.** Slack timestamps/permalinks, Confluence/Datadog int64 keys, UUIDs, EEIDs and numeric millisecond timestamps occur in these paths. Prefix-protected IDs survive r3; a bare PAN-length Luhn-valid page/monitor ID remains the already accepted false positive. Acceptance's broad “page ids unchanged” and “all structural ids” wording should be qualified by the protected-shape and email-ID exceptions. Preserve non-string JSON numbers exactly when decoding/re-encoding.
9. **“Traces store only hashes” is still false.** `context_trace.go:81–88,124–190,252–254` records request/query/asker inputs, IDs, resolve links including `why`, diagnostic metadata and error text as well as title/summary/body hashes. The inner masker protects response-derived link strings, not the original request or all diagnostic metadata. Correct this repeated factual claim; do not silently expand D2 into trace-at-rest redaction.
10. **Unmasked replay and mode storage: confirmed.** `trace_render.go:21–65` rehydrates raw text and uses unfiltered hybrid rendering; `cmd/agent-mem/trace.go:101–130` compares item digests. `context_traces.acl_mode` is TEXT (`migrations/20261011000001_context_traces.sql:11`), so the proposed version requires no migration. Legacy/admin matching is an unchanged-fixture expectation, not a guarantee after data or rendering changes.
11. **Comment hygiene:** carry forward the prior audit: examples occur outside the singled-out comment too. Their real-world provenance and the org-wide ban are supplied context, not independently proven by this review. Replace all identified real examples without reproducing them in tests or this report.
12. **Deployment dates, response-size estimate and performance:** supplied context/estimates, not verified by production calls. Source inspection does not prove a 100 KB response bound or p95 impact.

## Middleware placement and complete field coverage

The proposed `withContextTrace(maskGraphResponse(h), ...)` order is correct for search and resolve. `context_trace.go:63–82` receives the masker's final body, adds a generated `request_id`, then hashes the final response and extracts per-item hashes. The generated request ID is not arbitrary free text. Reversing the wrappers would hash raw item text. Masking must also remain active when the recorder is nil: `withContextTrace` then returns its argument unchanged.

Directly wrap node and epic. Wrap the actual neighbors child GET handler, or the mounted child router; changing only the top-level `r.Method` registrations misses neighbors/cards.

A recursion over every decoded string value reaches all current free-text response paths:

- **Search legacy:** title, summary, author, query, arm/error strings.
- **Search hybrid/linked:** those fields plus purpose, decisions and their authors, open questions, root author, participants, PR titles and `linked_via_title`.
- **Node:** title, summary, hydrated body, author name, URL and edge-reference strings.
- **Resolve:** title, summary, hydrated body, author, link tag/topic/why, URL, echoed seeds and cache-miss IDs.
- **Neighbors plain/cards:** title, overview, decisions, open questions, authors/participants, PR titles, edge tag/topic/why/verdict/verdict_why and structural strings.
- **Epic:** root/member titles and statuses, brief, highlights/open-items text and citation sources.

`json.RawMessage` becomes ordinary nested arrays/maps when the complete response is decoded. Its generated schema is `briefItem{text,sources}` (`refresh_epic_brief.go:265–276`); it is not an escape from response-level traversal. No demonstrated current PII-bearing dynamic JSON-key path was found. The stated contract is string values, not arbitrary key rewriting.

Thus field coverage and wrapper order are sound. The remaining problem is the string masker's exemption, not an omitted response path.

## Fail-closed, trace and acceptance checks to retain

These are implementation/test clarifications carried forward, not additional blocking findings:

- Buffer headers and bytes privately until success. Drop/recompute stale Content-Length and representation headers. On partial-write panic, discard everything and emit only stable `500 {"error":"mask_failed"}`; add that explicit regression. Plain-text errors already exist, including malformed resolve JSON and missing seeds, so the specified 500 conversion is a real compatibility change.
- Validate the entire JSON document, not just the first successful decoder call. Use `UseNumber` or equivalent lossless number handling; test an integer above 2^53 and equality of all non-string values.
- Update trace mode after `requireAsker` has set `filtered`; a pre-handler assignment is overwritten. Preserve `rejected` and `not-evaluated` distinctions rather than indiscriminately relabeling all failures.
- For masked replay, branch before calling the renderer or comparing digests. Test masked/truncated rows and masked email-bearing IDs that cannot be resolved. Require the notice and absence of comparison labels; preserve admin/legacy comparisons on unchanged fixtures.
- Independently hash the exact decoded title/summary/body delivered to the client and compare with trace item hashes. A mode-only test cannot detect reversed wrapper order. Assert safe error traces have no items; do not incorrectly require original trace request inputs to have been masked.
- Mounted route tests must assert fixture rows and exact field paths exist before checking placeholders. Empty results, hidden epic briefs or missing fields must fail the test. Include `linked_via_title`, `verdict_why`, PR titles, decision authors, epic sources and link tag/topic, not only generic bodies.
- Include nested arrays and misleading keys (`id`, `scope`, `url`) in walker tests; admin `1` versus `true`/`0`/absent; body-only resolve asker; encoded neighbors IDs; and recorder-present/absent variants. Retain existing ACL-denial tests.
- Require complete token/candidate consumption: no matching a valid-length prefix/suffix of an overlong digit run, no protecting part of a booking value, and no tails left by phone/card overlap. R3 now specifies phone priority and the critical punctuation/booking cases. Add R3-F1's protected-ID/email collisions.
- Name the allowed Phase 1 flaky tests (`TestPeriodicTickerReclaimInterleavings`, `TestReindexHeuristicCLI_RateAndProgress`) and compare failures with base rather than waiving unspecified failures.

## Execution receipt and handoff

Manual source reads/searches and synthetic in-memory specification probes only. No DB queries, secrets inspection, hub calls, deployment, or code/plan edits. No implementation masking tests exist in this checkout; no claim of passing `make test-db` or `go vet` is made. No dedicated security scanner ran.

Graph: not applicable to changed-symbol analysis; plan-only revision. Fan-out: inline, no subagent tool. The supplied prior reviews served as the round history. `bd prime` was read; no tracking/database mutations, commits, pulls or pushes were performed under the read-only review scope. Only this requested report was written; pre-existing files were left untouched.

**Approval gate:** resolve R3-F1 with email precedence across all node types and its regression tests. Correct the repeated trace-storage fact. R2-F1/R2-F2 and the middleware/replay architecture do not require another redesign. The documented remaining detection limits still need Enzo's acceptance, not new review findings.
