# Code review round 3: graph context Phase 1b masking

Branch `feat/graph-context-phase1b-masking`, HEAD `47886ac`, delta `457cca4..47886ac`. deep-review Round 3, read-only.

## Verdict

**REQUEST_CHANGES (narrow).** F1 is closed. Every r2 row and every new URL-adjacency shape I tried is masked, and nothing regressed. One new blocking shape remains: a phone with its `+` percent-encoded as `%2B` (F2). The fix is a few lines, and I checked it in a scratch copy.

## F1 HIGH: closed

`internal/redact/redact.go:38` (allowlist), `:113-140` (the `+digit` cut, the tail cut and the trim).

Each of these is masked when it sits directly after a URL, followed by a phone or a PAN:

- The r2 rows: `[t](url)+65…`, `(url)4111…`, `).` `):` `.` `:` `!` `(` `+`, NBSP, U+3000, U+2009, U+200B, `،` and `，`.
- Other Unicode spaces: U+2002, U+2003, U+2007, U+200A, U+2060, U+FEFF, U+200C and U+200F.
- Other punctuation: `؛`, `、`, em and en dashes, `…`, `”`, `;` and `,`.
- Markdown and link syntax: `**url**+65…`, `_url_4111…`, nested Markdown links, `[url](url)4111…`, Jira `[url|url]4111…` and Slack `<url|+65…>`.
- A labelled local phone after a URL: `url phone: 9123…`, `url. Phone: 9123 4567`, `url,mobile 8123…`, and NBSP followed by `mobile:`.
- `url?phone=+65…` and `url. +65…`.

These must-keep values are unchanged: payment refs, action refs, `F.` refs, `booking ref:`, `order=`, Slack ts, permalinks with `thread_ts`, UUIDs, node ids, `wa.me`, `%40` queries, `ip:port` URLs and dotted versions.

`go test` passes for `./internal/redact`, `./internal/graph/ids` and `./internal/graph/handlers`. `go vet` is clean.

## F2 HIGH (Verified), new: a percent-encoded `+` hides a phone

`internal/redact/redact.go:94`, `:122-123` and `:301` (`candLeftOK`).

`%2B` is not treated as `+`. The digits that follow it then have the letter `B` on their left, so `candLeftOK` rejects them:

| Input | Output |
|---|---|
| `customer_phone=966501234567&x=1` | `customer_phone=[phone]&x=1` |
| `customer_phone=%2B966501234567&x=1` | unchanged |
| `msisdn=%2b966501234567` | unchanged |
| `https://x.example/?to=%2B6591234567` | unchanged |

Jira tickets in this team often contain pasted form bodies, request logs and curl commands from payment gateways, and those carry `%2B…` phone numbers. The delta's stated rule is that a `+` phone is masked even inside a URL. Without the `+`, the same value is masked. What leaks reaches LLM context, and that cannot be undone.

Why it was missed: rounds 1 and 2 never tested a percent-encoded `+`.

**Fix:** treat `%2B` or `%2b` followed by a digit as a forced `+`. Add a helper `isPct2B`, then:

- use it in the `Mask` candidate check, with `force=true`;
- in `matchAt`, set `plus=true` and `p+=3`;
- use it in the `protectedSpans` cut next to `+digit`.

In a scratch copy, all four rows mask, the shipped tests pass and every must-keep row is unchanged. Add the rows above as tests.

## Residual risk (not blocking)

- **Node-id span (root-cause sibling).** `redact.go:57` still uses the old exclusion class, so `jira:PAY-1` followed by NBSP, `)` or `،` and then `+65…` leaks. Ordinary lowercase words are also node types, so `person:+65…` and `partner:4111…` leak as well. These are rare in human text. A one-line fix closes it: `[A-Za-z0-9\-._~:/#@!$&*=%]+`, with no `+` and no `|`. I checked it in the same scratch copy.
- **PII inside URL syntax.** `url?4111…`, `#`, `/`, `=`, `&`, `-`, `~`, `*`, `url?tel=9123…` and glued `url006591…` stay readable. This matches the accepted false negative for "digits inside a URL".
- **Characters inside digit runs.** U+200B, U+200D, a soft hyphen, U+2011 or U+2007 inside a phone or card leaves it unmasked. This was already the case before this delta and is uncommon in pasted text.
- **Over-masking.** Confluence titles such as `Incident+2026-10-12+Payments` become `Incident[phone]+Payments`, which breaks the link. The plan says this over-masking is limited to the query, but here it also hits the path. Consider skipping date-shaped `+` runs.

## Receipts

- graph: degraded: code-review-graph was not queried. rg shows that every changed helper is private to `internal/redact`, and the callers of `Mask` (`handlers/mask.go:48,110`) are unchanged.
- fan_out: inline: Round-N delta
- Probes: about 100 inputs run against a scratch module copy of `redact` and `ids`, outside the repo.
- coverage-only validator: `OK: coverage valid`
