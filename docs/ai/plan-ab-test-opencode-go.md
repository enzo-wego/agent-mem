# Plan — A/B test OpenCode Go on both tiers, then adopt Go-first with Claude fallback

Written 2026-08-22. **Self-contained: assume the reader has no memory of the conversation that
produced it.** This is the execution brief.

Supersedes the "A/B test" and "Rollout" sections of `docs/ai/plan-opencode-go-provider.md`. That
file still holds the background: pricing, model list, retention policy, and the gateway's shape.
Read it first.

## Enzo's decision, which this plan implements

> If quality from OpenCode Go is good enough, use Go **first** for both generate tiers, and fall
> back to the Claude subscription when Go hits a limit or runs out of quota.

So the target cascade is **Go → Claude**, per tier, with embeddings staying on OpenRouter
permanently (Go serves no embedding models). OpenRouter is deliberately **not** in the generate
cascade: it holds ~$10 until the 1st, and a summary-tier runaway failing over to it would cost
~$20/day.

## What we already have

- API key on the hub at `/Users/enzo/opencode-go.key` — 67 chars, `sk-B…`, mode 600.
  **Never print its contents.** Read it into an env var; do not echo it, do not commit it.
- Endpoint: `https://opencode.ai/zen/v1/chat/completions`, OpenAI-compatible
  (`@ai-sdk/openai-compatible`). `app/openrouter.py` in the llm-gateway repo already speaks this
  protocol — the A/B needs no new client, just a different base URL and key.
- llm-gateway repo: `~/go/src/github.com/llm-gateway`, deployed on the hub as
  `llm-gateway-llm-gateway-1`.

## Measured volumes this must satisfy (hub, 7 days, excluding the 08-17 backfill spike)

| tier | source | monthly calls |
|---|---|---|
| cheap | flat `processObservation` (~2,400/day) | ~72,000 |
| cheap | `link_topics` judges (~100 jobs/day × ~15 calls) | ~45,000 |
| **cheap total** | | **~117,000** |
| summary | thread summaries (~50/day) + cluster/flat summaries | **~3,000** |

Budget check against Go's **$60/month** ceiling (all models share one pool — the per-model tables
in the docs are not additive): MiMo-V2.5 at $0.000399/req × 117,000 ≈ **$46.70**; MiMo-V2.5-Pro at
$0.00368/req × 3,000 ≈ **$11.00**. Total ≈ **$57.70 of $60** — it fits with ~4% margin.

Those unit prices are derived from Go's own table, which assumes ~71,500 **cached** tokens per
request. Our calls have no cache and much smaller prompts, so our real cost should be materially
lower. **Measuring that is a first-class goal of this test**, not a footnote — it decides whether we
have 4% headroom or 400%.

## Candidate models (0-day retention only)

**Muse Spark 1.2 Contributor is excluded on policy** — it trains on submitted data, and this
traffic is Wego internal Slack content. Do not test it, do not include it in a cascade.

| tier | candidates | monthly budget if used alone |
|---|---|---|
| cheap | `mimo-v2.5` (primary), `deepseek-v4-flash` (comparison) | 150,400 / 37,800 |
| summary | `mimo-v2.5-pro` (primary), `qwen3.7-plus`, `minimax-m3` | 16,300 / 21,600 / 16,000 |

Baseline for both tiers is what runs today: cheap = `claude-haiku-4-5`, summary = `claude-sonnet-5`.

---

## Part 1 — cheap tier: objective, scored by agreement

The cheap tier is where the volume is and its output is *structured*, so this can be measured
rather than eyeballed.

1. **Extract 100 real inputs** from the hub — the actual node pairs plus topic-scope text that
   `confirmTopicLink` and `judgeTopic` see (`internal/graph/handlers/link_topics.go`). Real
   production prompts. Do not synthesise them; a synthetic prompt tests nothing about our data.
   Sample across topics, not 100 rows from one thread.
2. **Run each input through three models** via the gateway: `claude-haiku-4-5` (baseline),
   `mimo-v2.5`, `deepseek-v4-flash`.
3. **Record per call**: the decision, full raw response, latency, prompt tokens, completion tokens,
   whether the response parsed, and any error.
4. **Headline metric: agreement rate** with the haiku baseline.
5. **Adjudication:** collect every disagreement (expect 5–20) into a small side-by-side table.
   Enzo decides which answer is right for each. This turns agreement into accuracy without needing
   pre-labelled ground truth. Do not skip this step and do not guess on his behalf.

### Thresholds — fixed before results are seen

| metric | adopt | reject |
|---|---|---|
| agreement with haiku | ≥ 90% | < 85% |
| Enzo's adjudication of disagreements | candidate right in ≥ half | candidate clearly worse |
| parse-failure rate | ≤ 1% | > 3% |
| p95 latency | ≤ 2× haiku | > 3× haiku |

Between adopt and reject: run 200 more inputs rather than deciding on noise.

**Parse-failure rate is not a formality.** agent-mem has 673 flat-memory rows failed on malformed
JSON (`parse observation: unexpected end of JSON input` and friends). A model that emits
almost-valid JSON fails *after* we have paid for the call, and those failures cost retry budget.

---

## Part 2 — summary tier: side-by-side, judged by Enzo

There is no objective metric for summary quality, and Enzo has already rejected one cheap model
here on direct experience (`gemini-2.5-flash` — "quality very bad"). So this half is deliberately
human-judged and must not be decided by a score.

1. **Pick 15 real threads** from the hub with existing `claude-sonnet-5` summaries in
   `graph.thread_summaries`. Choose deliberately for variety: a long multi-participant incident
   thread, a short two-message exchange, a thread mixing languages, one heavy with code or
   stack traces, one that is mostly links/attachments.
2. **Regenerate each** with `mimo-v2.5-pro`, `qwen3.7-plus`, and `minimax-m3`, using the exact
   production prompt from `summarize_thread` — same system prompt, same body assembly. A different
   prompt invalidates the comparison.
3. **Present blind**: one page per thread, the four summaries in randomised order, labels hidden.
   Enzo ranks them. Blind ordering matters — knowing which is Claude will bias the read.
4. Also record tokens, latency and cost per summary, same as Part 1.

**Decision rule:** adopt a Go model for summary only if Enzo ranks it first or tied-first on the
majority of threads. Anything less and summary stays on `claude-sonnet-5` — its volume is only
~3,000/month, so keeping it on Claude costs almost nothing in quota terms. **The cheap tier is
where the win is; do not trade summary quality to chase it.**

---

## Part 3 — deliverable: the numbers we cannot currently answer

Produce a short results file, `docs/ai/results-ab-opencode-go.md`, containing:

1. Agreement rate per cheap candidate, with the disagreement table for adjudication.
2. Enzo's blind ranking per thread for the summary candidates.
3. **Measured cost per call per model**, from real token counts — and from that, our own request
   ceiling for the $60/month pool. State plainly whether the ~$57.70 estimate above holds, and what
   the real headroom is.
4. p95 latency per model.
5. Parse-failure counts.
6. A recommendation per tier, referencing the thresholds above rather than inventing new ones.

## Non-goals for this round

- **Do not change any production tier setting.** No `BACKEND_CHEAP=zen`, no gateway redeploy. This
  round produces evidence, not a rollout. Adoption is a separate, human-approved round.
- **Do not touch embeddings.** They stay on OpenRouter (`google/gemini-embedding-001`) permanently.
- **Do not implement the failover cascade yet.** It is designed below for the adoption round, and
  building it before we know whether Go passes is wasted work.
- **Do not test or use Muse Spark 1.2 Contributor** (trains on data).
- **Do not resume the laptop instance**, and do not touch its database — Enzo has explicitly parked
  it. All work is hub-side.
- **Do not print the API key** anywhere: not in logs, not in a results file, not in a commit.
- Do not run the pending backfills as part of this. They are a separate decision and the backfill
  is embedding-dominated, so Go is largely irrelevant to it.

## Design for the adoption round (not built now — recorded so it is not re-derived)

Enzo's cascade, per tier: **Go → Claude**.

- Replace the current single `FALLBACK_ON_QUOTA` boolean with an **ordered provider list per tier**,
  e.g. `CASCADE_CHEAP=zen,claude`.
- Fail over on Go's limit and transport failures — **429, 5xx, and whatever Go returns when a
  dollar window is exhausted**. That last error shape is unknown; the A/B should capture it if it
  occurs, otherwise the adoption round must discover it deliberately rather than assume 429.
- **Do not fail over on 4xx** other than 429. A 400 means our request is malformed; silently
  retrying it on Claude would mask a real bug and double the cost.
- Keep `llm_hourly_call_cap` in place. It is the only thing that bounded the 2026-08-22 incident,
  and a cascade makes a runaway *more* expensive, not less, because it can no longer be stopped by
  one provider running dry.
- Log which provider served each call, alongside the existing `llm_caller` attribution, so a
  quality or cost regression can be attributed to a provider and a caller together.

---

## Execution mechanics — read this before writing any code

### Where this runs

**On the hub**, `ssh enzo@payments`. That is where the API key lives and where the
real data is. Keep the harness in `/tmp` on the hub; it is throwaway.

Repo checkout there: `~/go/src/github.com/agent-mem`. Gateway compose lives in
`~/go/src/github.com/llm-gateway`, container `llm-gateway-llm-gateway-1`.

### Do NOT add a `zen` provider to the deployed gateway for this round

The gateway has no OpenCode provider yet, and adding one means editing and
redeploying the container that currently serves all production traffic. **That is the
adoption round, not this one.** For the A/B, write a **standalone script** that talks
to the two sides directly:

- **Candidates (OpenCode Go):** POST straight to
  `https://opencode.ai/zen/v1/chat/completions`, OpenAI-compatible, `Authorization:
  Bearer $ZEN_KEY`. No gateway involved.
- **Baseline (haiku):** the hub's existing gateway `/generate` with `tier=cheap`,
  which is already `claude-haiku-4-5`. Do not add a new Anthropic path.

Reading the key:

```bash
ZEN_KEY="$(cat /Users/enzo/opencode-go.key)"   # never echo, never log, never commit
```

### Verify the model IDs before spending anything

The model identifiers in this plan (`mimo-v2.5`, `deepseek-v4-flash`,
`mimo-v2.5-pro`, `qwen3.7-plus`, `minimax-m3`) were transcribed from the docs page,
not from the API. **Fetch the live model list first** (`GET /zen/v1/models`, or
re-read https://opencode.ai/docs/zen/) and reconcile. A 404 on a guessed model name
is a wasted afternoon, not a data point.

Then **smoke-test one call per model** and print the full response envelope —
including the `usage` block and any rate/spend headers. That single call is how we
learn the token accounting and, if we are lucky, the shape of the
limit-exhausted error the adoption-round cascade must detect.

### Do not trip the hourly cap

`llm_hourly_call_cap = 300` per client, and the hub is serving live traffic. The
100-call haiku baseline leg must therefore:

- throttle (a short sleep between calls is enough), and
- treat a cap refusal as **retry later, not a failed data point** — a capped call
  that gets recorded as a parse failure corrupts the very metric we are measuring.

The candidate legs do not touch the cap at all; they bypass the gateway.

### Where the outputs go

- **Raw JSONL** (every prompt, every full response, tokens, latency): stays in `/tmp`
  on the hub. **Do not commit it.** These prompts contain Wego internal Slack
  content, and the results file is the deliverable, not the corpus.
- **`docs/ai/results-ab-opencode-go.md`** in the repo: aggregates, the disagreement
  table, and the recommendation. In the disagreement table, quote at most ~200
  characters per input — enough for Enzo to adjudicate, not a bulk export.
- Total spend for the whole test is ~360 calls (~245 of them on Go). Trivial. If the
  harness looks like it will exceed ~500 calls, stop and say why rather than
  proceeding.

### Order of work — stop points are deliberate

1. Confirm model IDs + smoke-test each. **Report back before the bulk run.**
2. Part 1 (cheap, 100 inputs × 3 models). Report agreement + the disagreement table.
3. Part 2 (summary, 15 threads × 3 candidates + the stored sonnet baseline).
   Produce the blind comparison page.
4. Write `docs/ai/results-ab-opencode-go.md`.

Steps 2 and 3 are independent — do 2 first, since it carries the decision.
