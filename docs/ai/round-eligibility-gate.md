# Plan — payments-eligibility gate at the ingest chokepoint, with a dashboard config

Written 2026-08-24. **Self-contained: assume no memory of the conversation that produced it.**

Hub: `ssh enzo@payments`, repo `~/go/src/github.com/agent-mem`. **Do not touch the laptop
instance or its database.**

## The problem, measured

7-day Slack ingest by channel. Roughly 38% of volume comes from channels with no payments
connection, yet every message gets the full treatment (extractor, embedding, topic judging,
sometimes a sonnet thread summary).

| msgs | channel | | msgs | channel |
|---|---|---|---|---|
| 195 | `payments-pull-requests` | | 46 | `product-hajj-umrah` |
| 165 | `payments-dev` | | 44 | `engineering-core` |
| 128 | `web-pull-requests` | | 44 | `payments-releases` |
| 119 | `hajj-umrah-core` | | 36 | `flights-supply-help` |
| 86 | `cai-cs-back-office` | | 30 | `vat_data_ota_eg` |
| 58 | `payments-team` | | 29 | `payments-x-shopcash-devs` |
| 57 | `product-issues` | | 28 | `flights-analysis` |
| 49 | `vat_data_ota_pk` | | 26 | `taxes-core` |

**A blunt channel `ignore` is the wrong tool here, and this is Enzo's explicit direction.**
Channels like `hajj-umrah-core`, `cai-cs-back-office`, `product-issues` and even
`web-pull-requests` *occasionally* carry real payments content — the scope covers Umrah
tax/invoicing, back-office admin tools, and the payment form UI/component work. Ignoring the
channel silently discards those. The gate must therefore decide **per message**, not per
channel.

## Goal

At the ingest chokepoint, score each message from a *gated* channel for payments relevance.
Process the eligible ones exactly as today; mark the rest ineligible and skip the expensive
downstream work. Make every part of it configurable from the dashboard, and make the whole
thing auditable and reversible.

## Where it goes

`internal/graph/handlers/ingest_content.go:186`, immediately after `channelContentSkip` — the
existing pre-LLM chokepoint that already returns early with an `outcome` string
(`skipped_dm`, `alert_fingerprinted`, `skipped_non_incident`). The gate is one more check in
that chain, returning `outcome: "skipped_off_topic"`.

That point has `req.Body` available and has not yet spent any LLM or embedding work.

## Mechanism: cosine first, LLM only for the uncertain middle

The scoring signal is **cosine similarity between the message embedding and an embedding of
the payments scope text** (`graph.topic_subscriptions.scope_definition` — the text already
shown in the dashboard's Hot-Topic Alerts panel).

1. Embed the scope text **once**, cache it, re-embed only when `scope_refreshed_at` changes.
2. Per gated message: one embedding call, then cosine.
   - `score >= high` → **eligible**, continue as today.
   - `score <= low`  → **ineligible**, return `skipped_off_topic`.
   - between → **uncertain band**: if `llm_adjudicate` is on, ask the cheap tier for a
     yes/no; otherwise treat as eligible (fail open).
3. **Fail open on every error.** If the scope embedding is missing, the gateway is down, or
   the embed call fails, the message is processed. A gate that drops data when it breaks is
   worse than no gate.

Why this shape rather than the LLM-per-message that was first suggested:

- An embedding call bills OpenRouter at ~$0.0001 and is **not** subject to the 300/hr generate
  cap. A generate call is. That cap is currently the throughput bottleneck — `link_topics` is
  draining at ~16/hour with a queue — so a design that spends generate calls to save generate
  calls makes the bottleneck worse before it makes it better.
- The gate replaces up to a haiku judge **plus** a share of a sonnet thread summary per
  message, so an embedding is comfortably cheaper than what it prevents.
- Restricting the LLM to the uncertain band gives the "90% confidence" behaviour that was
  asked for, while keeping generate volume to the ambiguous minority.

**Directional bias is deliberate.** A false positive (processing an unrelated message) costs
one message of work — today's status quo. A false negative silently discards real payments
data. Every threshold and every failure path must therefore lean toward processing.

## Configuration — new dashboard panel, mirroring Channel Filters

Settings key `graph.eligibility_gate`, one JSON blob, live-editable, same pattern as
`graph.channel_filters` (`internal/graph/handlers/channel_filters.go`):

```json
{
  "enabled": true,
  "mode": "dry_run",
  "scope_subscription_id": 1,
  "high_threshold": 0.62,
  "low_threshold": 0.45,
  "llm_adjudicate": false,
  "gated_channels": [],
  "exempt_channels": ["C0597404MS6", "CUV9EAYGY", "C05RNSE8TBR", "C06Q3JHUAUV"]
}
```

- `mode`: `dry_run` scores and records but **never skips**; `enforce` acts on the score.
- `gated_channels` empty means "every Slack channel except `exempt_channels`" — Enzo's
  choice of scope (everything except payments channels).
- `exempt_channels` are never gated. Pre-populate with the payments/tax channels above; these
  are the highest-value data and must not be at the mercy of a threshold.

Build the panel to match the existing Channel Filters UI: channel pickers showing
`name (ID)`, numeric fields for the thresholds, toggles for `enabled` / `llm_adjudicate`, a
`mode` selector, and one **Save** button. Reuse that component's patterns rather than
inventing a second style.

## Auditability and reversibility — non-negotiable

Skipping at ingest means **the node is never created**, so a wrong threshold loses data
silently. Two required mitigations:

1. **Record every decision** in a new table `graph.eligibility_decisions`:
   `channel_id, message_ts, score, decision, mode, scope_version, decided_at`. Store the
   Slack coordinates, not the body — enough to re-ingest from Slack later, cheap to keep.
2. **`dry_run` is the default and must stay the default until calibrated.** Ship with
   `mode: "dry_run"`, then use the recorded scores to pick thresholds against real data.

## Calibration — a deliverable, not an afterthought

After 24h of `dry_run`, produce a short report: the score distribution, and for a sample of
~40 messages spanning the range, the channel, an excerpt and the score. That is what justifies
the `high`/`low` values. **Do not invent thresholds** — the 0.62/0.45 above are placeholders
to make the config shape concrete, not recommendations.

## Non-goals

- **Do not add channels to the `ignore` list.** Explicitly rejected: those channels carry
  occasional payments content, which is the entire reason for a per-message gate.
- **Do not use OpenCode Go.** It was evaluated and parked on 2026-08-23
  (`docs/ai/results-ab-opencode-go.md`): 84% agreement, systematic over-linking bias. If the
  uncertain-band adjudicator is ever enabled it uses the existing cheap tier
  (`claude-haiku-4-5`) via the gateway.
- **Do not gate non-Slack sources** (Jira, GitHub, Confluence) in this round.
- Do not change `llm_hourly_call_cap`, tier backends, or gateway config.
- Do not enable `enforce` mode in this round. That is a separate, human-approved decision made
  against calibration data.
- No changes to `link_topics`, `summarize_thread` or `index_artifact` themselves — the gate
  works by not creating the node, so the downstream cascade never starts.

## Acceptance criteria

1. `graph.eligibility_gate` exists with the fields above; absent or malformed config = gate
   off, and ingest behaves exactly as before.
2. Dashboard panel reads and writes it, styled consistently with Channel Filters, showing
   channel names not bare ids.
3. In `dry_run`, a message from a gated channel is scored, a row lands in
   `graph.eligibility_decisions`, and the message is **still fully processed**.
4. In `enforce`, a message scoring below `low` returns `outcome: "skipped_off_topic"` and
   creates no node; one scoring above `high` is processed normally.
5. Channels in `exempt_channels` are never scored — verify no decision rows for them.
6. **Fail-open proven by test**: with the gateway unreachable or the scope embedding missing,
   the message is processed, not dropped.
7. The scope embedding is computed once and reused, not per message.
8. Tests use the `agentmem_test` scratch DB — **never** the live or dev database. Handler tests
   in `internal/graph/handlers` call `truncateGraphHandlerTables`, and that damage syncs onward.
9. `go build ./...` and `go vet ./...` clean. Embedded dashboard rebuilt before committing:
   `cd dashboard && npm run build`, then from the repo root
   `rm -rf internal/worker/dashboard/* && cp -R dashboard/dist/* internal/worker/dashboard/`.
10. Deployed with `mode: "dry_run"`, and the hub still healthy afterwards.

## How to verify

Read the database. Show a handful of real `graph.eligibility_decisions` rows with channel
names and scores, and confirm the same messages still produced nodes (proving dry-run does not
drop). Report any score that looks wrong — a payments message scoring low is the single most
important signal here, and far more useful than a clean summary.
