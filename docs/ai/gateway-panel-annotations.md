# Plan: Gateway panel — OpenRouter remaining + self-explanatory labels

Repo: `~/go/src/github.com/agent-mem`, branch off `main` @ `b140395`.
**Dashboard only. No Go changes, no llm-gateway changes, no new endpoints.**

Source: four annotations on the Settings page. Three were questions about what
the controls mean; the answers belong in the UI so nobody has to ask again. One
was a real feature request.

---

## 1. Show OpenRouter remaining USD in Gateway Status

The panel answers "is the Claude seat up?" but not "how much OpenRouter money is
left?" — and OpenRouter is what funds every embedding, so it is the number that
decides whether the graph can still grow.

The data already exists. `getOpenRouterUsage()` (`src/api.ts:277`) hits
`/api/openrouter/usage`, which the worker serves from llm-gateway's `/usage`
with a 60s cache. `App.tsx` and `LiveGlobe.tsx` already render it.

- In `src/pages/Settings.tsx`, fetch it in the same `load()` that fetches health
  and config, and render remaining next to the seat line:
  `Claude seat available · OpenRouter $12.34 left of $50`.
- Use `limit_remaining`; fall back to `limit - usage` only if `limit_remaining`
  is absent. If `available` is false, render nothing rather than `$NaN` or a
  scary error — the seat line is the primary signal and OpenRouter being
  unreadable must not make the panel look broken.
- Do **not** add an endpoint, a Go handler, or a second cache. If you find
  yourself editing a `.go` file for this item, stop — it is already served.

## 2. Make the form explain itself

Add hint text only. No new config keys, no behaviour change.

**Describe backend** — the confusing one. There is a backend selector but no
describe model, because `/describe` borrows one: on Claude it uses
`MODEL_CHEAP` + `EFFORT_CHEAP`, on OpenRouter it uses `OR_MODEL_SUMMARY`
(`llm-gateway/app/main.py:129-160`). Say exactly that under the control. Do not
"fix" it by adding `MODEL_DESCRIBE` — that is a gateway change and is out of
scope for this task.

**The three tiers** — add a short line under the backend row saying what each
one actually serves in agent-mem:

- `summary` — thread and cluster summaries, hot-topic detection, scope refresh,
  feature-entity derivation, and flat-memory observation extraction
- `cheap` — the topic-link confirm gate only (`link_topics.go:717`), high volume,
  one yes/no per candidate
- `describe` — image and PDF attachments, multimodal (a different request shape,
  not a different quality level)

Note explicitly that flat memory is **not** on the cheap tier — it runs on
`summary` via `processor.go:146,219`. That was an explicit question and the
answer is counterintuitive.

**Effort** — reasoning effort passed to the Claude Agent SDK
(`claude.py:_options`). Claude-only: the OpenRouter path never receives it, so
changing it does nothing when a tier's backend is `openrouter`. Say so, or the
control looks broken to whoever flips a backend and sees no change.

**Max Claude budget** — a per-call ceiling handed to the SDK as
`max_budget_usd`; it aborts a single runaway call. It is not a daily or monthly
cap and does not limit total spend.

**Claude timeout** — wall clock for one call. Also reused as the HTTP timeout
for OpenRouter calls (`openrouter.py:64,86`). The existing "must be below 200"
label should say *why*: it has to stay under agent-mem's 200s client timeout,
which in turn stays under the 240s job lease, or a lease expires mid-call and
the janitor reclaims the job into a duplicate LLM call.

Keep hints to one line each, in the existing muted `text-xs` style. This is a
dense form already; do not turn it into a document.

---

## Verify

```bash
cd ~/go/src/github.com/agent-mem/dashboard
npx tsc --noEmit && npm run build
cd .. && rm -rf internal/worker/dashboard && cp -R dashboard/dist internal/worker/dashboard
go build ./...
```

The embedded bundle has no Makefile step — it must be rebuilt by hand or the
deployed dashboard silently serves the old JS.

## Rules

- **Do not deploy.** Commit and push a branch; the lead reviews, merges, deploys.
- **Do not touch the VPS** — not `.env`, not the database, not systemd.
- **Do not unpause.** `processing_paused` stays `true`.
- Do not change any current gateway *value* (models, backends, efforts). This
  task changes labels and adds one read-only figure.
