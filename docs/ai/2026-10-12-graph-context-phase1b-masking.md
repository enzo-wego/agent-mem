# Graph context Phase 1b: mask personal data in graph reads (D2)

Date: 2026-10-12. Owner: Enzo. Status: draft r4 (after pi reviews r1-r3: `docs/reviews/2026-10-12-graph-context-phase1b-masking*.md`), needs pi re-review and Enzo's approval.
REVIEWER: pi (auto: GPT weekly 85% left)
ENVELOPE: accepted (Enzo, 2026-10-12 00:50 SGT)
STATUS: approved (Enzo, 2026-10-12: dispatch on pi APPROVED; pi r4 APPROVED 00:53 SGT)
Parent: `docs/ai/2026-10-11-graph-context-quality.md` (Q2, decided as D2).
Follows Phase 1 (`docs/ai/2026-10-11-graph-context-phase1-acl.md`, PR #93, deployed
2026-10-11 23:55 SGT). Repo: agent-mem only. Branch `feat/graph-context-phase1b-masking`,
worktree `/Users/enzo/go/src/github.com/agent-mem-wt-mask`.

## Goal

D2 (Enzo, 2026-10-11): for non-admin callers, mask emails, phone numbers and card-like
numbers in every graph read response; keep payment refs and booking refs readable,
because they are the lookup keys the bot needs. Admin (`X-Graph-Admin: 1`) responses
stay unmasked.

## Non-goals

- Masking at ingest or in the DB (stored data unchanged; the dashboard is admin and
  sees raw text).
- Names. Display names (`author`, `participants`, ...) stay: they are employees and
  the bot needs them. Customer names inside free text are not detectable by pattern;
  out of scope, recorded as a known limit.
- p-agent changes. p-agent's own Slack masking directive still governs what reaches
  Slack.

## Facts (main `6ea089d`+; worker re-checks)

- No single choke point. The five filtered routes each encode JSON themselves:
  `/search` (search.go:393,413; hybrid display `search_hybrid_display.go:60-188`;
  linked rows `search_linked_threads.go:27-206`), `/resolve` (resolve.go:492; bodies via
  `hydrate.Greedy`, `internal/graph/hydrate/budget.go:37`; `Links[].Why` :736),
  `/node` (node.go:115), `/node/{id}/neighbors` incl. cards (neighbors.go:587; `Edge.Why`,
  `VerdictWhy`, decisions, overview), `/epic/{key}` (epic.go:272; brief, highlights,
  open items as `json.RawMessage`).
- Every one of them calls `requireAsker` first (epic.go:136, resolve.go:194,
  neighbors.go:125, search.go:115, node.go:48); `askerACL.noFilter` is true only for
  admin (acl_guard.go:59,71-73,220). All other graph GETs are admin-only (router.go:53).
- MCP passes server JSON through unchanged (`internal/graphmcp/client.go:177-243`) and
  always sends an asker, never admin, so MCP output gets masked with no MCP change.
- No PII masking exists in the repo today (only credential masking:
  `internal/worker/settings_handlers.go:51`, `internal/database/postgres.go:35`).
- Payment refs: `rePaymentRef` `\b[psd][0-9b-oqrt-z]{9}\b` with at least one digit,
  `reActionRef` `\ba[0-9b-oqrt-z]{14}\b` (`identifiers.go:29-30`), and `F.<[a-z0-9]+>`
  refs (`extractor/extractor.go:458-469`, node id `payment_ref:F.<id>`), whose suffix may
  be all digits. No booking-ref format exists in the code.
- Real node ids can carry an email: `ids.Person(email)` is `person:<email>`
  (`internal/graph/ids/ids.go:167-168`). `/resolve` echoes caller seeds verbatim
  (`resolve.go:351`). Neighbors is a mounted child router (`router.go:154`,
  `neighbors.go:57-61`).
- Digit-heavy values that must survive: Slack ts `\d{10}\.\d{6}`, permalinks `p\d+`,
  Confluence page ids, Datadog monitor ids, UUIDs, eeids, `ts_ms`, URLs, node ids.
- Traces store per-item title / summary / body hashes of the final response
  (`context_trace.go:81,237-276`), plus request inputs (query, asker), ids, resolve link
  `why` and diagnostics (`context_trace.go:81-88,124-190,252-254`). The masker covers what
  is derived from the response; request echoes stored in traces are caller input and
  stay as today (traces are admin-only to read). `agent-mem trace` replay re-renders unmasked
  (`trace_render.go:56`) and would report digest mismatches for filtered rows.
- `identifiers.go:45` has a comment containing real payment refs. Org policy bans
  payment refs in committed files.

## Approach

One response-level masker, applied as middleware to exactly the five filtered routes,
active only when the request is not admin.

1. **Middleware** `maskGraphResponse` (new `internal/graph/handlers/mask.go`), wired
   as `withContextTrace(maskGraphResponse(h), ...)` for search and resolve (traces hash
   what the client received), and directly around node, epic and the neighbors child
   router's GET handler. Admin requests are not buffered. For a non-admin request it
   buffers the whole response, then:
   - JSON body (any status): decode the full document (trailing data is an error), walk
     **every** string value at every depth, mask it, re-encode, keep the status.
   - Anything else (missing or non-JSON Content-Type, invalid JSON, encode failure, a
     handler panic after partial write): replace with `500 {"error":"mask_failed"}`.
     Nothing from the original body is written. Fail closed.
2. **No key or value exemptions.** r1's skip-list and the node-id rule are removed: every
   string is masked, including ids, URLs and echoed seeds. Identifiers survive because
   the patterns cannot match them (rule 3), not because they are skipped. An email inside
   an id (`person:<email>`) or a URL is masked too; follow-up calls by such ids are not
   possible for non-admin callers, which is accepted (people routes are admin-only).
3. **Patterns** (new `internal/redact` package, pure functions, table tests). r3 replaces
   r2's punctuation-boundary and booking-window rules (pi review r2): structure is
   protected by **explicit token shapes**, never by the character before a number, and a
   booking label protects **only its own value**.
   - **Step 1, email first, everywhere** (r4, pi review r3: `partner:`, `status:`,
     `code_file:` ids can also hold an email): `[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`
     -> `[email]` in every string, inside ids, URLs and protected shapes alike. Nothing
     is protected from email masking.
   - **Step 2, protected tokens for numbers** (copied through unchanged by step 3,
     matched as whole tokens after step 1):
     - node ids whose prefix is one of the node types the `internal/graph/ids` package
       produces (worker derives the list from that package, one shared constant, with a
       test that fails when a new type is added without updating it), e.g.
       `slack:C...:<ts>`, `cf:<id>`, `jira:PAY-1`, `datadog:monitor:<id>`.
     - URLs (`https?://\S+`).
     - Slack ts `\d{10}\.\d{6}`, UUIDs, payment refs (`rePaymentRef`, `reActionRef`
       from one shared definition, `F.[a-z0-9]+`).
     - A booking value bound to its label: `(booking|order|pnr|itinerary)` (word
       boundary, case-insensitive), optionally followed by `ref`, `reference`, `no`,
       `no.`, `number`, `id` or `#`, then optional spaces and one of `: # =`, then exactly
       **one** value `[A-Za-z0-9-]{5,20}`. Only that value is protected; scanning resumes
       right after it, so a later card or phone in the same sentence is still checked.
       Bare `ref` / `reference` are not booking labels (payment refs are protected by
       shape already).
   - **Step 3, phone and card** outside protected tokens. A candidate is a digit group
     not directly adjacent to another letter or digit; any punctuation (`:`, `=`, `` ` ``,
     `*`, `_`, quotes, brackets) may sit next to it. So `card:4111...`, `tel:+65...`,
     `phone=8123...`, `` `4111...` `` and `**+6581...**` are all candidates, while
     `cf:4111...` was already protected in step 1.
     - **Phone** -> `[phone]`: (a) `+` then 8-15 digits with optional single spaces,
       hyphens, dots or one pair of parentheses; (b) labelled: 7-15 digits (same
       separators, optional leading `+` or `0`) when the nearest label word before it,
       within 20 characters and with no other number between, is `phone`, `tel`,
       `mobile`, `whatsapp`, `contact`, `call` or `hp`.
     - **Card** -> `[card]`: (a) 13-19 digits, optionally grouped by single spaces or
       hyphens, passing Luhn; (b) labelled: 12-19 digits in that grouping when the
       nearest label word (same rule) is `card`, `pan`, `cc`, `credit`, `debit`, `visa`,
       `mastercard` or `amex`, masked even when Luhn fails.
     - A phone match wins over a card match on the same span; the whole match including
       `+` is replaced.
   - Accepted false negatives, listed in the README: unlabelled local phone numbers
     without `+`, unlabelled non-Luhn digit runs, numbers glued to letters, emails
     written with spaces or "at", customer names. Accepted over-masking: a standalone,
     unlabelled Luhn-valid 13-19 digit run that is really an id but is not in a protected
     shape.
4. **Trace replay**: masked rows record `acl_mode='filtered-masked:v1'` (TEXT column, no
   migration). `agent-mem trace` replay re-renders only rows it can reproduce (admin and
   older `filtered` rows, as today) and prints `masked v1: digest not replayed` for
   masked rows instead of a false mismatch. Per-item hashes on masked rows are hashes of
   masked text.
5. **Repo hygiene**: replace every real payment ref in `identifiers.go` comments (not
   only line 45) with synthetic examples of the same shape; do not copy them into tests.
6. Docs: README / CLAUDE.md asker-contract section gains a masking paragraph with the
   detection envelope and the accepted false negatives.

## Files expected to change

`internal/redact/` (new), `internal/graph/handlers/mask.go` (new), `router.go`
(wire the middleware), `cmd/agent-mem/trace.go` and/or `trace_render.go`,
`identifiers.go` (comment), tests beside each, README, CLAUDE.md, this plan's Findings.

## Acceptance

1. `internal/redact` table tests (synthetic data only):
   - masked: emails (plain, in `person:<email>`, in a URL, in `mailto:`), `+` phones in
     several formats incl. parentheses, labelled local phones, Luhn-valid PANs (plain,
     spaced, hyphenated), labelled non-Luhn card numbers, a `+` phone whose digits also
     pass Luhn (masked whole as `[phone]`, no dangling `+` or tail);
   - unchanged: `F.4111111111111111` and other `F.` refs, `rePaymentRef` / `reActionRef`
     shaped strings, labelled numeric booking refs, Slack ts, permalinks, UUIDs, eeids,
     page ids, `slack:C...:<ts>` node ids, URLs without email, unlabelled non-Luhn 16-digit
     runs, amounts;
   - boundaries: too short / too long runs, adjacent letters or digits, two candidates
     side by side;
   - r2 regressions, each alone and as a sole free-text canary: `card:4111111111111111`,
     `phone:81234567`, `phone=81234567`, `tel:+6581234567`, a PAN in inline code and in
     a fenced code block, `**+6581234567**` (all masked), paired with unchanged
     `cf:4111111111111111`, `datadog:monitor:4111111111111111`, Slack ids and payment
     refs;
   - email precedence (r4): `partner:alice@example.com`, `status:alice@example.com`,
     `code_file:exports/alice@example.com.csv`, `person:alice@example.com`, a URL with an
     email, each alone and in prose, give `[email]`;
   - booking binding: `booking 7 card 4111111111111111` and
     `booking ref: AB12345 phone +6581234567` mask the card / phone and keep the booking
     value; the same across a newline; a Luhn-valid numeric booking ref after a booking
     label stays intact; `F.` ref and a PAN in one sentence (ref kept, PAN masked).
2. Route tests (mounted router): for a named asker, each canary placed **alone** in its
   own field (title, summary, body, edge why, link why, thread decisions, overview,
   participants, epic brief, highlights, open items, echoed seed, search query echo) is
   masked on every route that returns that field (search legacy + hybrid + linked,
   resolve with `include_bodies`, node, neighbors plain + cards, epic); payment refs and
   all structural ids still present; admin gets canaries unchanged.
3. Fail closed: a stub handler returning (a) 200 with no Content-Type, (b) 200
   `text/plain`, (c) invalid JSON, (d) valid JSON plus trailing garbage, (e) an error body
   containing a canary, (f) a panic after a partial write, each gives `500 mask_failed`
   (a-d, f) or a masked body (e) for a named asker, and the outer trace records no raw
   items.
4. Trace: masked rows have `acl_mode='filtered-masked:v1'`; replay prints the masked
   notice; admin and pre-change `filtered` rows still replay with matching digests.
5. `make test-db` green except the known flaky tests; `go vet ./...` clean.
6. Branch pushed; no merge, no deploy, no hub calls.

## Verify after merge (conductor, Enzo's go)

Deploy the worker on the hub per CLAUDE.md, confirm build rev, then: a named-asker search
for a term known to hit a body with an email returns `[email]` and admin returns the raw
text (counts only in the report, no content); rerun `eval relevance` to confirm no
latency regression beyond +20% p95 on resolve.

## Risks

- Over-masking a standalone, unlabelled Luhn-valid 13-19 digit run that is really an id
  (about 1 in 10 such runs). Ids with prefixes, dots or path context are not candidates;
  a hit costs readability only.
- An email inside a node id or URL is masked, so a non-admin caller cannot follow that
  id. Accepted.
- Under-masking: local phone numbers without `+`, emails written with spaces or "at",
  customer names. Known limits; the bot's Slack masking directive is the second layer.
- Buffering adds one decode/encode per filtered response; resolve responses are small,
  search up to ~100 KB.

## Findings

(filled by the worker)

Implemented as planned (redact package, `maskGraphResponse` on the five routes, `ids.AllTypes` + completeness test, `filtered-masked:v1` tagging and replay notice, `identifiers.go` comment cleanup, README/CLAUDE.md).

**Deviation (approved by Enzo, 2026-10-12):** the plan said every non-JSON response becomes `500 mask_failed`. The handlers answer 400/404/500 with plain-text `http.Error`, and about a dozen existing ACL tests expect 404 from node/neighbors. Final rule: JSON bodies at any status are walked and masked; status >= 400 with a non-JSON body gets the text masked by the full masker with the status kept; any 2xx non-JSON or invalid JSON body, invalid JSON with a JSON content type, and any panic stay `500 mask_failed`. Test: `TestMaskGraphResponsePlainTextErrorMaskedStatusKept` (400 and 404 with email, + phone and Luhn PAN).

Notes: the real refs in `case_ref_sql_test.go`, `link_topics.go` and `identifiers_test.go` were left untouched (plan scoped the cleanup to `identifiers.go`). The route test covers the fields in one fixture (title, summary, body, edge why, link why, overview, decisions, questions, participants, epic brief/highlights/open items, echoed seed, query echo); linked-thread rows share the same JSON walk and have no dedicated fixture.

## Fix round 1 (code review F1)

- **F1 (HIGH):** URL protected span was `https?://\S+`, swallowing text after a URL (`<url|+65…>`, `<url|4111…>`, `url,+65…`, space-less JSON). Now `https?://[^\s"'`<>\[\]{}|,;]+` with a trailing `.` `)` `:` trimmed. Five review inputs are must-mask tests. Digits inside a real URL path stay readable; listed in README accepted false negatives.
- **Non-ASCII digits (PDPL):** phone/card matching uses any Unicode Nd digit (`digitAt`, value from block offset) for plus/label rules and Luhn (`digitsOf`). Tests: Arabic-Indic, Extended Arabic-Indic, full-width.
- **Percent-encoded emails:** `user%40example.com` (`%40` has no case) masked as `[email]`.
