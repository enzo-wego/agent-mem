# Phase 1b masking plan review

**Verdict: CHANGES REQUIRED**

Reviewed draft r1 against checkout `b8a4acb`, including the deployed Phase 1 ACL plan and the parent quality plan. D2 is fixed: mask personal data for non-admin callers, preserve payment and booking lookup references. No code or plan edits, DB reads, hub calls, or deployment were performed. Examples below are synthetic.

This is a plan review, not an implementation approval. Source tracing and synthetic pattern/truncation probes were executed; the proposed middleware does not exist, so implementation tests and quality gates cannot yet establish its correctness. No dedicated security scanner ran.

## Blocking findings

### F1 — HIGH, verified: the node-ID exemption bypasses masking in actual free-text fields

**Plan:** lines 68–72, global `^[a-z_]+:[^\s]+$` exemption.

All of `email:alice@example.com`, `mailto:alice@example.com`, `tel:+6581234567`, and `card:4111111111111111` match that expression. A title, summary, decision, edge explanation, body, or echoed search query containing only one of these strings would remain untouched. This is not a node-ID validator; it accepts arbitrary lowercase labels and URL schemes. The synthetic probe confirmed all four matches.

The key exceptions also need a narrower contract. `graph_trace.seeds` is the original caller-provided string array (`resolve.go:351`), not a validated array of node IDs. URLs may carry personal data. Genuine node IDs can themselves contain email addresses: `ids.Person(email)` returns `person:<email>` (`internal/graph/ids/ids.go:167–168`). Preserving every ID/URL and promising no personal data anywhere in the response are therefore different contracts.

**Required change:** remove whole-value identifier exemptions from free-text positions. Use documented, path-specific structural exemptions for actual ID/reference fields, including node edges `from`/`to`, thread roots, linked-via IDs and epic citation `sources`. Do not exempt an arbitrary subtree because its key ends in `_id`/`_ids`. Explicitly resolve the treatment of email-bearing structural IDs/URLs and echoed seeds rather than calling them “never free text.” Recognized reference spans inside prose can be protected without exempting the entire string.

**Tests:** put each compact synthetic example independently into title/body/summary/why/text/query, not alongside space-separated canaries. Add arrays and nested objects with misleading keys, and explicit email-bearing ID/URL/seed cases. An ordinary mixed canary sentence cannot detect this bypass.

### F2 — HIGH, verified: the proposed card rule can destroy a payment reference supported by the repo

**Plan:** lines 43–46 and 75–83, payment refs “always contain letters” and are “never touched.”

The payment-family and action regexes do contain a leading letter, but those are not the complete supported domain. `extractor.go:458–469` recognizes `F.([a-z0-9]+)` and creates `payment_ref:F.<suffix>`. An entirely numeric suffix is valid. For synthetic `F.4111111111111111`, the suffix is 16 digits, passes Luhn, and is preceded by a dot, not a letter/digit. The proposed card boundary therefore permits replacing the suffix with `[card]` in prose. The node-ID exemption protects the ID field, not a sentence mentioning the reference.

The synthetic probe confirmed both extractor acceptance and card eligibility. The claim about preserving prefixed/dotted IDs is also too broad: excluding dots *within* a card candidate does not exclude a digit run *after* a dot or colon.

**Required change:** protect the full supported payment-reference tokens before numeric matching, including `F.` refs, and specify matching precedence. Do not infer that an unknown booking-reference format always contains letters or fails Luhn. Establish synthetic booking formats from the caller contract; explicitly address numeric booking references rather than silently breaking D2.

**Tests:** `F.` references with all-numeric Luhn-valid suffixes, in free text and alongside real synthetic PAN canaries; all three payment-family prefixes; action refs; booking examples, including numeric formats if supported. Bare Confluence/Datadog numeric IDs can also pass Luhn: tests must distinguish structured IDs from numeric text, not promise that Luhn proves identity.

### F3 — MEDIUM, verified: replay needs the pre-mask truncation boundary and a representation version

**Plan:** lines 84–85 and acceptance 5, simply apply the same masker for `acl_mode=filtered`.

The current pipeline is:

- `hydrate.Greedy` truncates the raw body to a byte boundary (`hydrate/budget.go:126–144`).
- `resolve.go:444–487` sets `body_truncated` using the raw returned body versus the stored full body.
- The outer trace wrapper will compute `body_bytes` from the **masked** response (`context_trace.go:256–260`).
- `cmd/agent-mem/trace.go:108–113` passes that byte count to `RenderTraceRepresentation`, which truncates the **raw** body first (`trace_render.go:35–37`).

Masking changes length. A raw 24-byte prefix `alice@example.com abcdef` becomes the 14-byte `[email] abcdef`. Replaying by cutting the raw body at 14 bytes yields `alice@example.`, not the recorded representation, even after masking. The synthetic probe reproduced this mismatch with unchanged content.

Furthermore, existing Phase 1 rows already use `acl_mode=filtered` but have unmasked digests. Applying masking to every historical filtered row creates false change reports. ACL mode is not a masking version.

**Required change:** record a masking/representation version and the raw returned-body byte boundary (metadata, not raw text). Replay must reproduce raw truncation first, then apply the recorded masking version. Preserve legacy unmasked replay or explicitly mark legacy rows non-comparable. Moving masking before truncation is not an equivalent fix without changing the live pipeline.

**Tests:** recorder-mounted resolve with truncation and PII before the cutoff, PII crossing the cutoff, UTF-8, and replacement-length changes; untruncated filtered search/resolve; admin and pre-masking filtered rows. A single untruncated filtered replay test misses this defect.

### F4 — MEDIUM, verified plan gap: “fail closed” currently depends on the handler declaring JSON

**Plan:** lines 62–66; acceptance 3–4.

A non-admin 200 response containing personal text is passed through if its Content-Type is absent or non-JSON. Thus a future missing header or alternative success writer bypasses the security boundary instead of failing closed. All current five success paths set `application/json`, so this is a regression-resilience gap, not a claim that today's success handlers already return plain text.

Errors are also deliberately outside the proposed boundary although the goal says every response. There is an existing concrete reflection path: `/epic/{key}` includes the requested value in its 404 text (`epic.go:191,203`); other handlers expose `err.Error()`. Treating every error as inherently PII-free is not supported by the code. This is separate from returning an unmasked successful artifact.

**Required change:** make unexpected successful content types/encoding failures fail closed, with full JSON-document validation and no partial/raw writes. Define the error policy explicitly: preserve status/error codes, but sanitize arbitrary text or return stable safe errors. Keep safe fixed ACL errors unchanged. Include output encoding failures and stale Content-Length handling in the implementation contract.

**Tests:** missing/wrong Content-Type, invalid JSON, trailing garbage, failed encoding, and an error body containing a canary. Verify the outer trace sees `500 mask_failed` with no raw items. Do not merely assert that all arbitrary 4xx/5xx bodies pass through byte-for-byte.

### F5 — MEDIUM, verified scope gap: the pattern ceiling narrows the fixed D2 decision without an acceptance decision

**Plan:** lines 75–80 and Risks.

“Phone numbers” becomes only international-looking strings with a leading `+`; “card-like numbers” becomes only Luhn-valid numbers. A clearly labelled local phone or a labelled 16-digit invalid-checksum card-like value is deliberately left readable. Recording this as a known limit does not establish that it meets fixed D2. No real-data lookup is needed to establish that the implementation contract excludes those classes.

**Required change:** define and obtain acceptance for the supported detection envelope, without changing D2's payment/booking preservation decision. At minimum consider labelled phone/card forms rather than masking every numeric ID. Specify full candidate boundaries, matching order and overlapping phone/card matches: a plus-prefixed long digit run must not have its middle masked as a card leaving a dangling plus or leak an unmatched tail.

**Tests:** labelled local phones, international formats with parentheses, too-short/too-long candidates, adjacent letters/digits, multiple adjacent candidates, invalid-checksum card-like text, and phone/PAN overlap. Tests should document accepted false negatives rather than equate “does not match” with “not personal data.”

## Facts audit

- **Five independent JSON encoders: confirmed.** Search legacy/hybrid (`search.go:393,413`), node (`node.go:115`), neighbors (`neighbors.go:587`), epic (`epic.go:272`), resolve (`resolve.go:492`). Middleware is a reasonable way to introduce a common response boundary.
- **All call `requireAsker`: confirmed, with wording correction.** Search/node/neighbors/epic guard at entry. Resolve first decodes the body (`resolve.go:184–194`), so malformed requests can return 400 before the guard; it still guards before graph reads. `isAdminRequest` requires exact `1`, and production `noFilter=true` comes from the admin branch (`acl_guard.go:59,71–73`).
- **Other graph GETs admin-only: confirmed.** `Mount` installs `graphReadPolicy`; GET/HEAD exceptions are the named filtered paths (`router.go:22–65`). POST resolve must be wrapped explicitly; the GET classifier does not cover it.
- **MCP: substantively confirmed, not byte-for-byte passthrough.** All five tool methods obtain asker headers, unset asker fails before HTTP, no admin header is sent. `doJSON` decodes into a map (`client.go:177–243`); it preserves server string contents rather than forwarding raw JSON bytes. Probe is intentionally asker-free and is not a graph read.
- **Existing masking: confirmed for the relevant graph paths.** Repository Go search found credential masking in settings and DB URL logging, no existing personal-data response masker. This is not proof about external callers.
- **Payment/action regex spelling: confirmed.** `identifiers.go:29–30`; digit requirement is an additional check. The `F.` extractor is a separate format, exposing F2. No booking-reference parser was found under the graph code; absence cannot prove booking refs are safe from the patterns.
- **Digit-heavy identifiers: confirmed as relevant shapes, not universally protected by the proposed regexes.** Slack IDs/timestamps, `p` permalinks, int64 Confluence and Datadog IDs, UUID request IDs, numeric times and EEIDs exist. `ids.CFPage`/`Datadog` accept int64 values, so numeric IDs are not bounded below PAN length. JSON numeric values need preservation too: decode with `UseNumber` or equivalent to avoid float64 rounding.
- **“Traces store only hashes”: false as written.** Title/summary/body payloads are hashed, but traces also store raw request/query/asker inputs, IDs, resolve `links` including `why`, diagnostic metadata, and error text (`context_trace.go:81–88,124–190,252–254`). The outer wrapper hashes final bytes after inserting `request_id`. Masking inside it protects returned link text as well, but does not sanitize stored request inputs. D2 is read-time masking, not trace-at-rest sanitization; document the distinction rather than expanding scope silently.
- **Replay renders unmasked: confirmed.** `RenderTraceRepresentation` uses hydrate/search renderers; hybrid replay deliberately uses `askerACL{noFilter:true}`. Existing data/ACL-dependent renderer differences may also cause legitimate mismatches; a new masking test must isolate unchanged fixtures.
- **Comment hygiene: partially verified.** The file has payment-reference examples in comments, including an earlier comment at lines 15–19 as well as the cited later block. Their real-world provenance and the claimed org-wide ban are not independently established by this checkout's guidance. Honor the supplied hygiene requirement, replacing all identified real examples without copying them into tests or the report.
- **Deployment/date/PR assertions:** treated as supplied historical context, not independently verified against the hub. No production inspection was needed or performed.

## Route coverage and middleware order

The proposed *placement* is correct if wired as `withContextTrace(maskGraphResponse(handler), ...)` for search and resolve. Reversing it records unmasked per-item hashes. Node and epic need direct wrapping. Neighbors is a mounted child router (`router.go:154`, `neighbors.go:57–61`), not a top-level `r.Method` handler: wrap the child router or its actual GET handler; do not assume wrapping the other registrations covers it.

A recursive walk of the decoded successful JSON can cover all current content-bearing value paths:

| Route | Content that must be visited |
|---|---|
| Search legacy | title, summary, author, query, arm error strings |
| Search hybrid/linked | above plus purpose, decisions text/by, open questions, root author, participants, PR titles and linked-via title |
| Node | title, summary, body, author name |
| Resolve | title, summary, hydrated body, author, link tag/topic/why; echoed seeds need an explicit policy |
| Neighbors plain/cards | node title/overview, decisions, open questions, authors/participants, PR titles; edge tag/topic/why/verdict/verdict_why |
| Epic | root/member titles/statuses, brief, nested highlights/open-items text |

`json.RawMessage` is not an obstacle: `epic.go:46–48` emits JSON, and `refresh_epic_brief.go:273–282` uses `briefItem{text,sources}` arrays. On decoding the outer response those are normal nested values. The current typed free-text keys are generally *not* in the proposed skip-list; the demonstrated current bypass is the global node-like value exemption, plus arbitrary seed/URL handling. No evidence was found that an existing typed `scope` field on these responses currently contains prose. The blanket key rule remains unsafe as a future schema contract, especially inside RawMessage objects.

## Acceptance-test changes needed

Keep the mounted-router approach, but require positive coverage, not just the absence of canaries:

- Assert each expected row and field is present before checking masked content. Seed separate visible artifacts, bodies, thread summaries/decisions, edge metadata and epic brief records; one node row cannot populate all those storage paths. Ensure epic brief source visibility permits the brief, and seed a genuine hybrid linked-row case with `linked_via_title`.
- Assert placeholder output and identifier/reference equality at exact decoded JSON paths. An empty response or dropped nested field must fail. Check admin output preserves original content, with and without a trace recorder.
- Include `VerdictWhy`, link topic/tag/why, PR titles, query, purpose, open questions, decision authors, epic citation arrays, ID-valued `from`/`to`, and compact node-like text from F1.
- Exercise `X-Graph-Admin: 1` versus `true`, `0`, and absent; body-only resolve asker; mounted neighbors with encoded IDs. Preserve existing ACL-denial tests.
- Add the pattern, error, numeric-precision and versioned/truncated replay cases described above. Assert traced title/summary/body hashes equal the strings actually sent to the client, not merely that a trace was produced.
- Name the allowed pre-existing flaky tests and compare failures with base; “except known flaky tests” is not permission to waive a new failure.

## Review execution notes

Manual source tracing and synthetic in-memory probes were used. No implementation was changed or executed; no claim of passing `make test-db` or `go vet` is made. No subagent tool was available. Structural graph tooling was not applied because this is a design review with no changed implementation symbols.

Beads tracking was attempted but blocked: the local database reports missing `issue_prefix` configuration. It was not initialized or repaired during this read-only review.

The middleware direction is sound. Approval requires fixing the free-text exemption, supported-reference collision and replay contract, and making the fail-closed/detection envelope and regression gates explicit.
