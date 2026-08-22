# Results — A/B test of OpenCode Go on the cheap tier

Run 2026-08-22 on the hub (`enzo@payments`). Method and pre-registered thresholds:
`docs/ai/plan-ab-test-opencode-go.md`. Part 2 (summary tier) **not yet run**.

Raw artifacts, uncommitted, on the hub only — they contain Wego internal Slack content:
`/tmp/ab-opencode-go-part1-{inputs,raw}.jsonl`, `-summary.json`, `-disagreements.md`.

## Verdict: REJECT `mimo-v2.5` for the cheap tier

Three of four thresholds pass, comfortably. The one that fails is the one that decides.

| metric | threshold | measured | result |
|---|---|---|---|
| agreement with haiku | adopt ≥90%, reject <85% | **84.0%** (84/100) | **REJECT** |
| parse-failure rate | adopt ≤1%, reject >3% | **0%** (100/100 parsed, all `finish_reason: stop`) | pass |
| p95 latency | adopt ≤2x, reject >3x | 13,557ms vs 7,059ms = **1.92x** | pass (barely) |
| adjudication of disagreements | adopt if candidate right in ≥half | candidate right in ~3 of 16 | **REJECT** |

84% is one point inside the reject band. Stating that plainly rather than rounding it into
the "run 200 more" middle zone: the pre-registered rule says reject, and the adjudication
below independently agrees.

## Cost: far better than estimated, and it does not rescue the verdict

| | plan estimate | measured |
|---|---|---|
| cost per cheap call | $0.000399 | **$0.000114** |
| projected 117,000 calls/month | $46.70 | **$13.39** |

The plan's estimate came from Go's own table, which assumes ~71,500 cached tokens per
request. Our real traffic: mean 1,669 prompt tokens (1,067 cached), 97 completion tokens.
So the $60/month pool has roughly **4x more headroom than assumed** — the earlier "fits with
4% margin" worry was wrong. Cost was never the blocker; accuracy is.

`reasoning_effort: "none"` is what made this affordable — 2 completion tokens vs 28 control
on the same probe, against 364 unsuppressed. Note the knob is not an ordinal scale:
`"low"` was *worse* than the default (107 tokens) and `"minimal"` returns HTTP 400. Only
`"none"` is verified.

## Why it fails: a systematic over-linking bias, not random noise

Of the 16 disagreements, **`mimo-v2.5` said "same" where haiku said "different" in 14**.
Overall it judged "same" 27 times against haiku's 15 — it links ~80% more aggressively.

This is a directional error, which matters more than the headline rate for a topic graph:
over-linking merges distinct work items into undifferentiated clusters, and the damage
compounds as the graph grows. Reading the reasoning, the pattern is consistent — `mimo-v2.5`
treats shared domain, shared participants and nearby timestamps as sufficient, where haiku
requires a shared work item and explicitly discounts "same partner and same author do not
make them the same topic".

Representative cases where haiku is right and the candidate is not:

- **#8** — two PR reviews by the same pair, but FMETA-2927 (airport picker) vs FMETA-2980
  (date picker): different tickets, different code paths. Candidate: "same".
- **#91** — a specific Storybook refactor PR vs a *generic review-request acknowledgement
  with no named subject*. Candidate: "same", on shared people and dates alone.
- **#12** — PAY-2324 (refund counts) vs PAY-2274 (list/detail views): distinct tickets,
  distinct user-facing outcomes. Candidate: "same".

Cases where the candidate is arguably right, and the honest count against it:

- **#25** and **#72** — the only two where the candidate said "different" and haiku "same".
- **#53** (Saudi Rail scoping vs go-live prep) and **#97** (PRA sandbox access vs PRA test
  submission) — both are one feature at two phases, 34 and 21 days apart. Calling these one
  topic is defensible; the candidate did and haiku did not.

That is ~3 of 16 for the candidate — below the "right in ≥half" bar.

**This adjudication is provisional — it is the reviewer's read, not Enzo's.** The plan makes
his adjudication mandatory precisely because a single-candidate test cannot attribute a
disagreement on its own. Full reasoning for all 16 pairs is in
`/tmp/ab-opencode-go-part1-disagreements.md` on the hub.

## Claude is NOT available on the Go subscription — checked, disproven

The pay-as-you-go `/zen/v1/models` catalogue lists `claude-haiku-4-5` and `claude-fable-5`,
which suggested a third option the plans never considered: keep identical model quality and
move only the billing off the Anthropic subscription. **It does not work.** On the Go
endpoint both return:

```
http=401 {"type":"error","error":{"type":"ModelError","message":"Model claude-haiku-4-5 is not supported"}}
```

The Go subscription serves open models only. Since the entire reason to want Go was capacity
insurance for the Anthropic subscription rather than cheapness, this closes the one path that
would have delivered it at zero quality risk.

## Measured API facts worth keeping

- **Two client shapes, not one.** OpenAI-style (`/zen/go/v1/chat/completions`,
  `Authorization: Bearer`) for `mimo-v2.5`, `mimo-v2.5-pro`, `deepseek-v4-flash`;
  Anthropic-style (`/zen/go/v1/messages`, `x-api-key` + `anthropic-version`) for
  `minimax-m3`, `qwen3.7-plus`. Different request and response schemas, separate caches.
- **`/zen/v1` vs `/zen/go/v1` are different products.** The former bills a credit balance
  (ours is $0 → `401 CreditsError` on every paid model); the latter is the subscription.
- **Error taxonomy:** `401 CreditsError` (balance exhausted — *not* the 429 the cascade
  design assumed), `401 AuthError` (bad key — must NOT trigger failover), `403 RegionError`
  (geo-blocked). Dollar-window exhaustion shape still unobserved.
- **No rate or spend headers** on any response. Spend can only be tracked by summing `usage`.
- `deepseek-v4-flash` is `403 RegionError` from the hub; reachable only via China-hosted
  opt-in, which Enzo declined for Wego Slack content. Part 1 therefore ran single-candidate.

## Recommendation

1. **Cheap tier stays on `claude-haiku-4-5`.** `mimo-v2.5` fails on accuracy, and it is the
   only Go model with the request headroom for 117,000 calls/month.
2. **A prompt-tuned retest is a legitimate follow-up, not a re-litigation** — but only with
   the thresholds unchanged. The failure is one systematic bias in a single direction, which
   is the kind of thing a stricter "shared work item required" instruction can move; the
   harness is built and rerunning costs ~$0.01 and ~20 minutes. If a retest is run, it needs
   fresh inputs, since tuning against these 100 would overfit to them.
3. **Part 2 (summary tier) is lower value than the plan assumed.** With Claude unavailable on
   Go and the cheap tier staying put, the remaining prize is ~3,000 summary calls/month —
   a small offload against a quality bar Enzo has already rejected a cheap model on.
4. **Do not build the cascade yet.** Nothing has passed that would use it.
