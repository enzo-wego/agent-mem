# Phase 1b masking plan review, round 4

**Verdict: APPROVED**

Reviewed draft r4 against checkout `e2d7c40`, the deployed Phase 1 ACL contract, parent quality plan, and the supplied prior reviews. D2 and Enzo's accepted detection envelope are fixed. This approves the implementation plan, not an implementation or deployment. No blocking findings remain; no new defect was established in this round.

## R3-F1: resolved

Email replacement now runs on every string before any numeric protection. Thus the actual email-capable constructors in `internal/graph/ids/ids.go:140–168` cannot bypass email masking through `partner:`, `status:`, `code_file:` or `person:` protection. The same holds for URLs and for other protected shapes. Protection in the second stage can preserve only the already-email-masked representation.

Acceptance 1 explicitly adds the four ID collisions and a URL, both alone and embedded in prose. An in-memory synthetic probe of the specified email regex passed all ten combinations. This verifies the specification's precedence fix, not nonexistent production masker code. The regex can consume `.csv` as part of an email-shaped suffix in the code-file example; that is not an email leak and follow-up by email-bearing IDs is explicitly sacrificed.

## Facts checked against code

- **Five independent JSON response paths: confirmed.** Search encodes legacy and hybrid results at `search.go:393,413`; hybrid fields are defined in `search_hybrid_display.go:14–54`; linked thread rows and their source titles are assembled in `search_linked_threads.go:176–206`. Resolve encodes at `resolve.go:492`, hydrates via `hydrate.Greedy` at `:316`, and extracts link why at `:736`. Node encodes at `node.go:115`; neighbors at `neighbors.go:587`; epic at `epic.go:272`.
- **Asker/admin boundary: confirmed for content reads.** All five handlers invoke `requireAsker`; only exact `X-Graph-Admin: 1` yields `noFilter` from the resolver (`acl_guard.go:59–73,220–235`). The phrase “calls requireAsker first” means before graph content access: resolve necessarily decodes its body first and can reject malformed JSON before the guard (`resolve.go:182–194`). The proposed response wrapper covers that rejection too. Other graph GET/HEAD paths are admin-gated by `router.go:53–65`.
- **MCP: confirmed semantically, not byte-for-byte.** All five graph methods obtain asker headers and do not send admin; `doJSON` decodes response JSON into a map (`graphmcp/client.go:177–243`). Masked string values remain masked. Probe is outside these five tools and intentionally exempt.
- **Existing masking: confirmed for the relevant repository paths.** Targeted Go-source search finds credential masking in worker settings and PostgreSQL URL handling, not an existing graph personal-data response masker.
- **Reference shapes: confirmed.** `identifiers.go:29–30` defines the stated payment/action regexes; its extraction loop at `:75–85` applies the digit check to both and an additional payment-word exclusion. `extractor/extractor.go:458–469` recognizes `F.` references and emits `payment_ref:F.<suffix>`, including numeric suffixes. No booking grammar was found in the identifier/ID/extractor paths. The proposed booking rule is policy, not a recovered domain schema.
- **IDs and seeds: confirmed.** Person IDs embed email; other unconstrained natural keys can too. Resolve echoes original seeds (`resolve.go:351`). Neighbors is a mounted child router (`router.go:154`, `neighbors.go:57–61`).
- **Structural numeric values: confirmed.** Slack timestamps, prefixed node IDs, UUIDs, URL/permalink forms and reference shapes require preservation. `ts_ms` and related fields are JSON numbers (`neighbors.go:81–86`, `search_hybrid_display.go:35–36`), outside the string walker. Bare numeric strings remain subject to the accepted detection envelope, not an unlimited promise that all conceivable numeric IDs survive.
- **Trace fact: corrected.** `context_trace.go:81–88,124–190,237–276` stores request/asker inputs, IDs, links including why, diagnostics and error text as well as representation hashes. The revised Facts item no longer claims traces contain only hashes. Response-derived values cross the masker; original request inputs and independently collected diagnostics are not a trace-at-rest redaction guarantee.
- **Replay/version storage: confirmed.** `trace_render.go:21–65` re-renders unmasked representations, including unfiltered hybrid rendering. `cmd/agent-mem/trace.go:101–130` currently compares those digests. `migrations/20261011000001_context_traces.sql:11` defines a TEXT ACL mode, so the proposed version needs no migration.
- **Comment hygiene: still implementation work.** Reference-related comments remain in this checkout's `identifiers.go`. R4's Approach 5 correctly expands the future cleanup to every real reference in those comments, not only line 45. No actual references were copied into this report. This plan review does not claim the cleanup has already happened.

## Coverage and wrapper order

The specified placement covers every current response string, not merely the primary node title/body:

| Route | Additional fields covered after final JSON decoding |
| --- | --- |
| Search, legacy and hybrid | Query echo, author, purpose, decisions, open questions, participants, PR titles, linked-via title, error strings, IDs and URLs |
| Resolve | Hydrated/truncated bodies, titles, summaries, author, echoed seeds, cache misses, link kind/tag/topic/why |
| Node | Title, summary, body, author, URL, incoming/outgoing edge IDs |
| Neighbors, plain and cards | Overview, decisions, questions, participants, PR titles, edge why/topic/tag and verdict why |
| Epic | Brief, member strings, resolved-from, nested highlights/open-items strings |

Epic's `json.RawMessage` fields (`epic.go:47–48`) are not opaque after encoding and decoding the complete response: their nested arrays and objects enter the same walk. Linked rows and cards also enter the final encoder before middleware sees bytes. No key-name skip-list or whole-string node-ID exemption remains. Known token shapes protect numbers only; they cannot protect email after the r4 precedence change. Structural-token and URL numeric exceptions are part of the accepted envelope and are not reopened here.

`withContextTrace(maskGraphResponse(h), ...)` is the correct order. The trace wrapper receives the masked bytes, inserts only its generated request ID, and then computes response/item hashes (`context_trace.go:63–82,237–276`). Wrapping in the reverse order would record raw representation hashes. With a nil recorder, `withContextTrace` returns its argument, so the independently installed masker remains active. The neighbors wrapper must be on its actual child GET handler, as the plan now explicitly says.

## Fail-closed and replay assessment

The revised contract discards non-JSON, missing-content-type, invalid/trailing JSON and partial-write panic responses, with stable `500 mask_failed`. JSON error responses are masked too. This covers actual plain-text errors in resolve and avoids relying on error content being inherently safe. The newly added panic acceptance case closes the prior omission.

Implementation must keep buffered bytes and headers private until success, and must not forward stale content-length/representation headers. This is an implementation obligation of the stated buffering/fail-closed design, not a newly discovered plan defect.

`filtered-masked:v1` plus declining masked replay avoids both changed-length truncation mismatches and false comparisons against historical `filtered` digests. The CLI must branch before rendering/comparison, including for masked email-bearing IDs that no longer resolve. Mode tagging must occur after the guard's `filtered` assignment; rejected/not-evaluated requests must not be indiscriminately relabelled.

## Acceptance-test assessment

The listed cases target the prior concrete defects: independent canaries defeat whole-value shortcuts, compact labels and Markdown exercise numeric boundaries, booking-plus-PII pairs catch protection extending past one value, and r4's ID/URL email cases catch R3-F1. Mounted route tests cover the child router, linked results, cards, bodies and epic nested JSON. Fail-closed cases now include panic after partial write. Legacy/admin trace behavior is retained on unchanged fixtures.

Carry forward the prior review's implementation-test details when realizing these gates:

- Assert the expected row and field exist before asserting masking; empty results must not pass. Include the ancillary fields enumerated above, especially linked-via title and verdict why.
- Independently hash the decoded title/summary/body actually returned and compare with recorded item hashes. Checking only `acl_mode` does not catch wrapper-order reversal. Exercise recorder-present and recorder-absent mounting.
- For masked replay, require the notice and no digest-comparison labels or renderer dependency; include truncated bodies and masked IDs. Keep admin/legacy matching tests on unchanged fixtures.
- Require whole-token/candidate consumption and no numeric tails; retain structural IDs and refs alongside the sensitive canary, with admin controls.

These are execution details of the existing acceptance promises, not additional envelope requirements or new blockers.

## Verification and conclusion

Read-only source tracing and a synthetic ten-case email-precedence probe were performed. No DB queries, secrets reads, external source fetches, hub calls, deployment, code edits or plan edits were performed. Only this requested review artifact was written. No masker implementation exists in the reviewed checkout; no claim is made that `make test-db`, `go vet`, route tests or performance gates have passed. No dedicated security scanner ran.

Graph tooling: not applicable to changed-symbol analysis, because this is a plan-only re-review; source relationships were checked manually. Fan-out: inline, Round-N delta/no subagent tool.

R3-F1 is resolved in design, the trace fact is corrected, partial-write panic is in acceptance, and reference cleanup is correctly broadened. R2 punctuation/booking fixes and the middleware/replay architecture remain intact. **APPROVED — no further plan-review round is needed before implementation.**
