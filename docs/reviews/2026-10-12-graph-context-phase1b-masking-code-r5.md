# Code review r5: graph context phase 1b masking

Branch `feat/graph-context-phase1b-masking`, HEAD `7b381dd`. This round covers only the delta since `e049d53` (form-encoded `%2B` phones). Final round before shipping.

## Verdict

**APPROVE — no further review needed.** r4 F2 is closed. Every r4 input is now masked, the new branch runs only after a `%2B` plus, and no must-keep value changed.

## F2 (r4, HIGH): resolved at 7b381dd

| Input | e049d53 | 7b381dd |
|---|---|---|
| `phone=%2B65+9123+4567&x=1` | unchanged | `phone=[phone]&x=1` |
| `phone=%2B966+50+123+4567`, `phone=%2B1+202+555+0143` | unchanged | `phone=[phone]` |
| `https://x/?phone=%2B65+9123+4567` | unchanged | `https://x/?phone=[phone]` |
| `curl -d 'phone=%2B65%209123%204567'` | unchanged | `curl -d 'phone=[phone]'` |
| `to=%2B65%2D9123%2D4567`, `to=%2b65%2d9123%2d4567`, `to=%2B65+9123%204567` | unchanged | `to=[phone]` |
| `x=%252B6591234567` | unchanged | unchanged (accepted residual) |

Trace: `pct` is set only when `plusStart` returns width 3 (`%2B`/`%2b`), and the encoded-separator switch runs only when no ordinary separator rune was read (`nr == 0`). Slicing is bounded by `min(q+3, len(s))`, and `r >= limit` still ends the loop.

## No new over-masking

I diffed 40 probes at base and head. The only changed outputs start with `%2B`. These are unchanged: `a+b`, `x%20y`, `https://x/?q=a+b+c&n=12345678`, `https://x/?q=1+2+3+4+5+6+7+8`, `search?q=2026+10+12+report`, `q=%20123%20456%207890`, `q=+65+9123+4567`, `note 12+34+56+78`, `v1+2+3`, `Incident+2026-10-12+Payments` (bare and in a URL), `%2Bfoo`, `%2B+123`, `sum=%2B12+3`.

Must-keep values are byte-identical: payment refs, Slack ts, permalinks with `thread_ts`, UUID, `wa.me/…`, `ip:port`, dotted versions, `%40`, and a bare `%2B`. `+65 9123 4567`, `+6591234567` and Luhn cards still mask.

## Residual (not blocking)

- `%2B2026%2D10%2D12` (an encoded `+2026-10-12`) is now masked as a phone, because `isDateAfter` only checks raw `-`. This over-masks, does not leak, and is unlikely in real text.
- `phone=%2B65++9123+4567` (doubled encoded space) and `to=%2B(65)+9123+4567` still leak. Neither is the standard form encoding of a typed phone.

## Tests

- `go test ./internal/redact` passes. `go vet ./internal/redact` is clean.
- `go test ./...`: all graph, graphmcp and redact packages pass. `internal/hooks` and `internal/skills` fail, but the branch does not touch them, and they fail the same way on `e049d53` and `main`. That points to the local environment, not this change.

## Receipts

- graph: degraded: code-review-graph not queried; delta touches only private `matchAt` in `internal/redact`, `Mask` callers unchanged (rg)
- fan_out: inline: Round-N delta
- Probes: 40 inputs, base vs head, run in scratch copies outside the repo.
- coverage-only validator: `OK: coverage valid`
