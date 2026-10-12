# Code review r4: graph context phase 1b masking

Branch `feat/graph-context-phase1b-masking`, HEAD `e049d53`. This round covers the delta since `47886ac`.

## Verdict

**REQUEST_CHANGES (narrow).** F2 is mostly closed. Compact `%2B` and `%2b` phones are masked, and so are phones that use space or hyphen separators. One variant still leaks: a form-encoded phone whose spaces are encoded as `+` or `%20`. The node-id allowlist and the date skip both work as intended. Nothing regressed.

## F2 HIGH (Verified): narrowed, still open for encoded separators

`internal/redact/redact.go`, in `matchAt`'s separator loop.

| Input | Output |
|---|---|
| `customer_phone=%2B966501234567&x=1`, `msisdn=%2b…`, `?to=%2B…`, `"%2B…"` in JSON | masked |
| `to=%2B65-9123-4567`, `to=%2B65 9123 4567`, `phone=%2B1-202-555-0143`, `%2b65 9123 4567` | masked |
| `phone=%2B65+9123+4567&x=1` (form encoding of `+65 9123 4567`) | **unchanged** |
| `phone=%2B966+50+123+4567`, `phone=%2B1+202+555+0143`, `https://x/?phone=%2B65+9123+4567` | **unchanged** |
| `curl -d 'phone=%2B65%209123%204567'`, `to=%2B65%2D9123%2D4567` | **unchanged** |
| `x=%252B6591234567` (double-encoded) | unchanged; residual, as agreed |

With `application/x-www-form-urlencoded`, a phone the customer typed with spaces always comes out as `%2B65+9123+4567`. Gateway request bodies and curl commands pasted into Jira are where F2 lives, and this is the form those bodies take. The ledger asked for "%2B with separators" before F2 could close.

Why it was missed: r3 tested only compact `%2B` values.

**Fix:** once the plus came from `%2B` (`plusStart` width 3), also accept `+` and `%20` as a space separator and `%2D`/`%2d` as a hyphen in `matchAt`, but only when no ordinary separator rune was read. I checked this in a scratch copy. Every row above masks except the double-encoded one. The shipped tests pass, and the output of all 43 non-`%2B` probe rows is byte-identical to HEAD. Add the rows above to `TestMaskFixRound3`.

## Checked and clean

- **Node-id allowlist:** `jira:PAY-1` followed by NBSP, `)` or `،` and then `+65…` is masked, and so is `person:+65…`. `partner:4111…` and `cf:4111…` are kept, which is approved precedence. `gh_pr:wego/payments#2269` and `slack:C123:<ts>` are kept. Node ids built by `ids.go` use only characters in the allowlist, apart from rare `code_file` paths.
- **Date skip:** `+6591234567`, `+65-9123-4567`, `+1-202-555-0143`, `+65 9123 4567`, `+44 20 7946 0958`, `+971-50-123-4567`, `+966 50 123 4567`, `+2010-1234-5678` and `call +1 (202) 555-0143` are all masked. The Confluence title `Incident+2026-10-12+Payments` is kept.
- **Must-keep values:** payment, action and F. refs, `booking ref:`, Slack ts, permalinks with `thread_ts`, UUIDs, `wa.me`, `%40` queries, `ip:port`, dotted versions and `%2B` with no digits after it.
- `go test ./internal/redact ./internal/graph/...` passes. `go vet` is clean.

## Residual (not blocking)

- `isDateAfter` does not check the month or day ranges, so `+2010-12-34-5678` leaks. That layout is not a real phone format. Adding a month 01-12 and day 01-31 check would close it.
- `jira:PAY-1%2B65…`: the allowlist includes `%`, so the span swallows the encoded plus. This is unlikely in human text.
- `a%2B65…` and `x+(65)…`, where the plus is glued to a letter, fall under the accepted "glued to letters" false negative.

## Receipts

- graph: degraded: code-review-graph was not queried. The delta only touches private helpers in `internal/redact`, and rg shows the `Mask` callers in `handlers/mask.go` are unchanged.
- fan_out: inline: Round-N delta
- Probes: about 60 inputs run against a scratch copy of the module, plus the fix candidate, outside the repo.
- coverage-only validator: `OK: coverage valid`
