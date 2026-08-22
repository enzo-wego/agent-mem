# Plan — add OpenCode Go as a third llm-gateway backend, A/B tested before adoption

Written 2026-08-22. Facts below were read from https://opencode.ai/docs/go/ and
https://opencode.ai/docs/zen/ on that date, not from memory.

## What Go actually buys us

$5 first month, then **$10/month**, giving access to a curated set of open models with
usage ceilings expressed in dollars: **$12 per 5 hours, $30 per week, $60 per month**. So $10
buys up to $60 of usage — a ~6x subsidy.

**It is not the cheapest per call.** OpenRouter on cheap models is still cheaper in raw dollars
(~$3/month at our volume). The reason to want Go is different and it is the reason that matters
right now: **it takes generate load off the Anthropic subscription**, which is the resource that
actually ran out, and it does so at a fixed, predictable price with no balance to drain. OpenRouter
has ~$10 left until the 1st; the Claude subscription burned out twice. Go is capacity insurance.

## Hard constraints found in the docs — these shape everything

1. **Go serves no embedding models.** The entire model list is chat/completion only. Embeddings
   must stay on OpenRouter (`google/gemini-embedding-001`). This matters more than it sounds:
   our pending backfill is *embedding-dominated* (~11,000 embeds vs ~1,000 generate calls), so
   **Go will barely help the backfill**. It helps steady-state generate load.

2. **Data retention differs per model, and one model trains on your data.**
   - 0-day retention: GLM-5.x, Kimi, MiMo-V2.5(-Pro), Qwen3.x, MiniMax, Hy3, DeepSeek V4 (ZDR
     agreement renewed monthly, currently valid through 2026-08-31)
   - 30-day retention: Grok 4.5, GPT 5.6 Luna
   - **Muse Spark 1.2 Contributor — "Yes" to model training, not ZDR. EXCLUDE IT.** It has the
     largest request budget (226,600/month) and it is the one we must not use: this traffic is
     Wego internal Slack content.

3. **The endpoint is OpenAI-compatible.** Open models are served at
   `https://opencode.ai/zen/v1/chat/completions` with `@ai-sdk/openai-compatible`. Our existing
   `app/openrouter.py` already speaks exactly this protocol. **This is a config-shaped change, not
   a new client.**

## Which model can actually carry our volume

Monthly request budgets from the docs, against our measured load. Our generate volume is roughly
**500/hour during working hours** → ~88,000/month (8h × 22d). Measured basis: 944 generate calls in
the 50-minute resume window on 2026-08-22, of which `confirmTopicLink` + `judgeTopic` (the cheap
tier, from `link_topics`) were the dominant graph consumer.

| model | requests/month | 0-day retention | covers 88k/mo? |
|---|---|---|---|
| **MiMo-V2.5** | **150,400** | yes | **yes, comfortably** |
| Muse Spark 1.2 | 226,600 | **NO — trains** | excluded on policy |
| DeepSeek V4 Flash | 37,800 | yes | no |
| Qwen3.7 Plus | 21,600 | yes | no |
| Hy3 | 21,500 | yes | no |
| MiniMax M2.7 | 17,000 | yes | no |
| MiMo-V2.5-Pro | 16,300 | yes | no |
| GPT 5.6 Luna | 10,250 | 30-day | no |
| DeepSeek V4 Pro | 5,200 | yes | no (but fine for summary tier) |

**Only MiMo-V2.5 has the headroom for the cheap tier.** Everything else is a summary-tier
candidate (lower volume, higher quality bar).

Note their budgets assume ~71,500 *cached* tokens per request — a coding-agent pattern. Our calls
have no cache and much smaller prompts, so our real per-request cost should be well below their
assumption and our effective request count higher than the table. Worth verifying in the A/B, not
worth assuming.

## Where Go fits, and where it must not

| tier | today | proposal |
|---|---|---|
| **cheap** (`link_topics` judges, flat observations) | `claude-haiku-4-5` | **candidate for Go / MiMo-V2.5** — this is the volume |
| **summary** (thread + cluster summaries) | `claude-sonnet-5` | **stay on Claude for now.** Enzo has direct experience that summary quality on a cheap model is bad (`gemini-2.5-flash` was rejected for exactly this). Only move after a separate, human-judged A/B. |
| **describe** (attachments) | `claude` | leave alone. Blocked on `files:read` anyway, and vision quality is its own question. |
| **embed** | `google/gemini-embedding-001` on OpenRouter | **unchanged — Go cannot serve it.** |

## The gateway change (small)

`app/main.py:_tier_config()` returns `(backend, claude_model, openrouter_model, effort)` and
`backend` is currently a two-valued thing (`claude` | `openrouter`). `app/openrouter.py` reaches
`config.OPENROUTER_BASE` and one `_headers()` directly.

1. **Generalise the provider, don't fork the client.** Turn the base-URL + key + headers into a
   small provider record, and let `openrouter.py`'s request function take one. Register a third
   provider `zen` pointing at `https://opencode.ai/zen/v1` with `ZEN_API_KEY`. Same code path.
2. **Per-tier backend already exists** (`BACKEND_SUMMARY` / `BACKEND_CHEAP` / `BACKEND_DESCRIBE`) —
   extend the accepted values to include `zen` plus per-tier model names (`ZEN_MODEL_CHEAP`).
3. **Pin embeddings to OpenRouter explicitly.** `embed()` must not follow tier backend selection.
   Add a guard that fails loudly if someone points an embed call at a provider with no embedding
   model, rather than 404-ing at runtime.
4. **Ordered failover.** Enzo asked for a configurable cascade (Go → OpenRouter → Claude). Express
   it as an ordered list per tier rather than the current single `FALLBACK_ON_QUOTA` boolean, and
   fail over on 429/5xx/quota — not on 4xx, which would mask real bugs.
   **Keep `LLM_GATEWAY_FALLBACK_ON_QUOTA` off until the cascade is tested**: with ~$10 of OpenRouter
   credit, a summary-tier runaway failing over costs ~$20/day.

## The A/B test — this is the part that needs Enzo's key

The cheap tier is the right thing to test first: it is the volume, and its output is *structured*,
so the comparison can be objective rather than a vibe check.

**Method**

1. Pick 100 real `link_topics` inputs from the hub — node pairs plus the topic scope text that
   `confirmTopicLink` / `judgeTopic` actually see. Real production prompts, not synthetic.
2. Run each through `claude-haiku-4-5` (current) and each candidate (`mimo-v2.5`,
   `deepseek-v4-flash`) via the same gateway, recording: the decision, latency, token counts,
   and any parse failure.
3. **Agreement rate** is the headline metric: how often does the candidate reach the same verdict
   as haiku on the same input.
4. Where they disagree, extract those cases (expect 5-20) and have **Enzo adjudicate** which
   answer is correct. That converts agreement into an actual accuracy signal without needing
   pre-labelled ground truth.
5. Record parse-failure rate separately. This matters more than it looks: agent-mem has a history
   of malformed-JSON failures (`parse observation: unexpected end of JSON input`, 673 rows on the
   hub), and a model that emits slightly-off JSON will fail *after* we have paid for the call.

**Decision criteria — set before seeing results**

| metric | adopt | reject |
|---|---|---|
| agreement with haiku | ≥ 90% | < 85% |
| Enzo's adjudication of disagreements | candidate right ≥ half the time | candidate clearly worse |
| parse-failure rate | ≤ 1% | > 3% |
| p95 latency | ≤ 2x haiku | > 3x haiku |

If a candidate lands between adopt and reject, run 200 more inputs rather than deciding on noise.

**Cost of running the test:** 100 inputs × 3 models = 300 calls. Trivial against any budget, and
the haiku leg can come from the existing subscription.

## Rollout, if it passes

1. `BACKEND_CHEAP=zen` on the **hub only**, cap left at 300/hr so a surprise is bounded.
2. Watch one working day: the `llm_caller` attribution histogram plus parse-failure counts. We now
   have per-caller attribution, so a quality regression shows up as a spike in retries for a
   specific handler rather than as a vague feeling.
3. Compare `graph.jobs` failure rates before/after — specifically `link_topics` failures.
4. Only then consider the summary tier, as its own round with its own human-judged A/B.

## What NOT to do

- **Do not move embeddings.** Go cannot serve them.
- **Do not use Muse Spark 1.2 Contributor**, whatever its request budget. It trains on the data.
- **Do not move the summary tier on the strength of a cheap-tier result.** Different quality bar,
  and Enzo has already rejected one cheap summary model on experience.
- **Do not expect this to fix the backfill.** The backfill is ~11,000 embeds and ~1,000 generate;
  Go addresses the smaller half.
- **Do not enable the failover cascade before it is tested**, and not while OpenRouter has ~$10.
- Do not remove the cap. It is the only thing that bounded today's incident.

## Open question for Enzo

Go's ceiling is $60/month of usage. If our real cost per call is well under their cached-coding
assumption we have lots of headroom; if not, MiMo-V2.5's 150k/month is the only model that fits and
we would be relying on a single model with no same-tier fallback inside Go. The A/B should therefore
record **actual token counts and cost per call**, so we can compute our own request ceiling instead
of trusting the table.
