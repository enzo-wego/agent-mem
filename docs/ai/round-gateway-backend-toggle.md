# Plan — expose the per-tier LLM backend switch in the dashboard, and fix the OpenRouter models

Written 2026-08-23. **Self-contained: assume the reader has no memory of the conversation
that produced it.**

## Goal

Give Enzo two working modes for LLM generation, switchable from the dashboard without ssh:

- **default:** Claude subscription (`claude-haiku-4-5` cheap, `claude-sonnet-5` summary)
- **on demand:** the *same* models via OpenRouter (`anthropic/claude-haiku-4.5`,
  `anthropic/claude-sonnet-4.5`), billed per token

## Why this is small: the switch already exists

The llm-gateway (separate repo, `~/go/src/github.com/llm-gateway`, container
`llm-gateway-llm-gateway-1`, bound to `127.0.0.1:8750`) already implements everything:

- `app/main.py:_tier_config()` selects a backend per tier from `BACKEND_SUMMARY` /
  `BACKEND_CHEAP` / `BACKEND_DESCRIBE`, each `claude` | `openrouter`.
- `GET /config` and `PUT /config` (both `Depends(require_key)`) read and atomically persist
  the editable runtime knobs. `config.EDITABLE_ENV_KEYS` already contains every key we need:
  `BACKEND_*`, `MODEL_*`, `OR_MODEL_*`, `FALLBACK_ON_QUOTA`, **`MAX_BUDGET_USD`**,
  `EFFORT_*`, `OR_MAX_TOKENS*`.
- Validation is already there: `_BACKENDS = {"claude", "openrouter"}`, `_EFFORTS`, and a
  `_CONFIG_LOCK` around the `.env` rewrite.

**So the gateway needs no code change.** The work is (a) correcting two model values and
(b) surfacing the existing `PUT /config` in agent-mem's dashboard.

## Current live state on the hub, measured 2026-08-23

```
LLM_GATEWAY_BACKEND_CHEAP=claude          LLM_GATEWAY_MODEL_CHEAP=claude-haiku-4-5
LLM_GATEWAY_BACKEND_SUMMARY=claude        LLM_GATEWAY_MODEL_SUMMARY=claude-sonnet-5
LLM_GATEWAY_BACKEND_DESCRIBE=claude       LLM_GATEWAY_FALLBACK_ON_QUOTA=false
LLM_GATEWAY_OR_MODEL_CHEAP=google/gemini-2.5-flash     <-- WRONG, see below
LLM_GATEWAY_OR_MODEL_SUMMARY=google/gemini-3.6-flash   <-- WRONG, see below
```

**The two `OR_MODEL_*` values are the bug this round fixes.** Enzo rejected
`gemini-2.5-flash` on summary quality by direct experience. As configured, flipping a tier to
`openrouter` silently downgrades to a model he has already refused — the switch looks like
"same quality, different biller" and is not.

`FALLBACK_ON_QUOTA=false` is correct and **must stay false**: the switch is deliberate, never
automatic. Note the code *defaults* it to `true`, so it must remain explicitly set — an unset
var means silent fallback.

## Approach

### 1. Correct the OpenRouter models (config only, no code)

Via the gateway's own API, so the change is validated and persisted the same way the UI will
do it later:

```
PUT /config {"OR_MODEL_CHEAP": "anthropic/claude-haiku-4.5",
             "OR_MODEL_SUMMARY": "anthropic/claude-sonnet-4.5"}
```

**Verify the exact OpenRouter model slugs against `https://openrouter.ai/api/v1/models`
before setting them.** Slugs drift, and a wrong slug fails at call time, not at set time —
which means it fails only when Enzo actually needs the escape hatch. Do not guess.

### 2. Proxy the gateway config through the worker

agent-mem already knows the gateway address: `config.LLMGatewayURL`
(`internal/config/config.go:93`, e.g. `http://172.18.0.1:8750`).

Add two worker routes next to the existing settings handlers in
`internal/worker/settings_handlers.go`:

- `GET /api/gateway/config` → proxy `GET {LLMGatewayURL}/config`
- `PATCH /api/gateway/config` → proxy `PUT {LLMGatewayURL}/config`

Constraints:

- Reuse the dashboard's existing auth. Do **not** invent a second auth scheme.
- The gateway needs its own API key on the proxied call. Read it from existing config; do not
  log it, do not return it, do not add it to any response body.
- Whitelist the keys the UI may write: `BACKEND_CHEAP`, `BACKEND_SUMMARY`,
  `BACKEND_DESCRIBE`, `OR_MODEL_CHEAP`, `OR_MODEL_SUMMARY`, `MAX_BUDGET_USD`. Pass nothing
  else through, even though the gateway would accept more — a dashboard that can rewrite
  `MODEL_CHEAP` or `FALLBACK_ON_QUOTA` by accident is a bigger blast radius than this feature
  needs.
- Surface the gateway being unreachable as a clear error, not an empty panel. The gateway is
  a separate container and *will* be down sometimes.

### 3. Dashboard panel

In `dashboard/src`, alongside the existing settings UI:

- Three tier rows (cheap, summary, describe), each a two-state control: **Claude subscription**
  / **OpenRouter**. Show the model that each choice resolves to, so the consequence is visible
  before clicking — this is the whole point of the round.
- Show `MAX_BUDGET_USD` as an editable field in the same panel. It already exists in
  `EDITABLE_ENV_KEYS` and is the only thing standing between a forgotten switch and a large
  bill; hiding it would be strange when it is free to surface.
- Display the current per-tier cost implication in plain text, from measured numbers:
  cheap ≈ **$0.00215/call** (1,669 prompt / 97 completion tokens measured 2026-08-22),
  ≈ **$250/month** at ~117,000 calls. Claude-subscription mode is $0 marginal.
- **Rebuild the embedded dashboard before committing** — this repo serves an embedded copy:
  `cd dashboard && npm run build`, then from the repo root
  `rm -rf internal/worker/dashboard/* && cp -R dashboard/dist/* internal/worker/dashboard/`.
  Skipping this ships a UI change that does not appear in production.

## Non-goals

- **Do not enable `FALLBACK_ON_QUOTA`.** Automatic failover is explicitly not wanted, and with
  metered billing an automatic switch is how a quota incident becomes a spend incident.
- **Do not change embeddings.** They stay on OpenRouter (`google/gemini-embedding-001`).
- **Do not touch `llm_hourly_call_cap`.** It bounds the OpenRouter bill as well
  (300/hr × $0.00215 ≈ $0.65/hr, ~$16/day worst case) and is the reason this hatch is safe
  to expose at all.
- **Do not change the default backend.** Claude stays default on all three tiers; this round
  ships the *ability* to switch, not a switch.
- **Do not touch the laptop instance or its database.**
- No new gateway code. If it seems necessary, stop and say why — it probably is not.

## Acceptance criteria

1. `GET /api/gateway/config` returns the live gateway config through the dashboard auth, with
   no API key present in the response.
2. The dashboard shows three tier toggles reading their real current state (all `claude`), and
   `MAX_BUDGET_USD`.
3. Flipping the cheap tier to OpenRouter in the UI and reloading shows it persisted, and
   `docker exec llm-gateway-llm-gateway-1 env | grep BACKEND_CHEAP` agrees.
4. **Flipping it back to `claude` restores the original state**, verified the same way. A
   one-way switch is not an escape hatch.
5. Writing a non-whitelisted key through the proxy is rejected.
6. `OR_MODEL_CHEAP` / `OR_MODEL_SUMMARY` are the verified Anthropic slugs, and a single live
   `/generate` call with the cheap tier flipped to OpenRouter returns a sane completion —
   proving the hatch actually works rather than merely persisting a string.
7. `FALLBACK_ON_QUOTA` is still `false` afterwards.
8. `go build ./...` and `go vet ./...` clean; embedded dashboard rebuilt and committed.

## How to verify

Do not trust the build log or the UI alone. For the live checks:

```bash
ssh enzo@payments 'PATH=/opt/homebrew/bin:$PATH docker exec llm-gateway-llm-gateway-1 \
  sh -c "env | grep -E \"BACKEND_|OR_MODEL|FALLBACK\" | sort"'
```

Leave the system in Claude-subscription mode on every tier when finished.
