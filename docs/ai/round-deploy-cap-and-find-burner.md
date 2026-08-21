# Round plan — deploy the cap, prune the queues, resume the hub, name the burner

Written 2026-08-22. **Self-contained: assume the reader has no memory of the conversation
that produced it.** This plan is the execution brief for one round.

Primary issue: `agent-mem-5k0` (P0). Also touches `agent-mem-0x7` (cap, already built) and
`agent-mem-rik` (P0, duplicate jobs). Run `bd show <id>` for each.

## Situation

agent-mem drained Enzo's Anthropic subscription quota twice. Measured from llm-gateway
access logs, the burn ran at **600-850 `/generate` calls per hour on the hub** plus
**500-730/hour on the laptop** — roughly 15,000-20,000 calls/day, for days.

**The responsible code path has never been identified.** Five hypotheses have been tested
against production; four are disproven by measurement and recorded in
`docs/ai/quota-burn-containment.md`. Do not re-litigate them:

1. `link_signature` churn — no; `linkSignature` already guards legacy rows without an LLM call.
2. Permanent signature disagreement — no; stored signatures agree for 1,686 of 1,699 rows.
3. Sync touching `nodes.updated_at` — no; 0 old rows re-touched in a 10-minute window.
4. `summarize_thread` was the consumer — no; 27,411 jobs completed in 6 hours while only
   ~20 summaries were written. Nearly all took a cache-hit early return, no LLM call.

**Hypothesis 5, untested and the reason for this round:** the burn is *human-triggered*.
The hub's hourly `/generate` counts follow Enzo's working day (UTC+7):

| local time | calls/hour |
|---|---|
| 02:00-05:00 | 2 |
| 08:00 | 139 |
| 10:00 | 418 |
| **15:00** | **780** |
| 19:00 | 78 |

A background job would be flat around the clock. This is not. That points at paths that
create **no `graph.jobs` row** — which is exactly why four queue-based hypotheses failed.
Leading candidates: `cluster_summary.go:647` (a GET handler calling the **expensive** tier)
reached via the dashboard or via the `agent-mem mcp` server bridging Enzo's Claude Code
sessions to the hub worker.

**This round does not guess.** It deploys attribution, bounds the damage with a cap, and
then resumes and reads the log.

## Pre-state — verify before starting

| thing | expected |
|---|---|
| hub (`ssh enzo@payments`) | `agent-mem-worker-1`, `agent-mem-postgres-1`, `llm-gateway-llm-gateway-1` all up |
| hub `processing_paused` | `true` |
| hub `/generate` last 10 min | `0` |
| laptop | worker + postgres up; llm-gateway **container stopped**; native uvicorn on :8750 live |
| laptop `processing_paused` | `true` |
| `main` | `c340dbe` |
| branch `feat/llm-meter-and-cap` | `2073644`, pushed to origin, **no PR yet**, **not merged** |

Note `docs/ai/why-we-keep-rebuilding.md` lives on the branch, not on `main`.

## Goal

1. The cap and per-call attribution are running on the hub.
2. A non-zero hourly ceiling is set, so no cause can exceed it.
3. The duplicate job population is pruned.
4. The hub is resumed **alone**, and the attribution log either names the burning path or
   proves it does not reproduce.

## Non-goals — do NOT do these

- **Do NOT change any model, provider or tier.** Decided and settled: summary stays
  `claude-sonnet-5`; cheap stays `claude-haiku-4-5`; embeddings stay
  `google/gemini-embedding-001` on OpenRouter. Enzo has direct experience that
  `gemini-2.5-flash` summary quality is bad — it is ruled out. OpenCode Go is deferred
  (it needs code, because embeddings ride the same `OPENROUTER_BASE` and Go has no
  embedding models).
- **Do NOT enable `LLM_GATEWAY_FALLBACK_ON_QUOTA`.** It is `false` on the hub and must stay
  false until the cap is proven live. OpenRouter has only ~$10 until the 1st, and a
  summary-tier runaway failing over would cost ~$20/day — it would drain the balance in
  ~12 hours. Cap first; failover is a later, separate decision.
- **Do NOT resume the laptop.** One instance at a time, so attribution is unambiguous.
- **Do NOT rebuild any data.** No signature bump, no stale-summary sweep, no re-embedding.
- **Do NOT touch `channels.go`, `summarize_thread.go`, `cluster_summary.go` or
  `detect_hot_topics.go`.** Finding the path is what the attribution is for. Changing
  suspects before evidence is how four wrong diagnoses happened.
- Do not delete `thread_summaries` rows (`agent-mem-fs2` is separate).

## Steps

### 1. Open the PR and merge (GATE: needs Enzo's explicit go-ahead in the round)

Branch `feat/llm-meter-and-cap` is reviewed and complete. All 8 acceptance criteria were
verified by the conductor, not taken from the worker's report:

- attribution logged at 3 sites, derived via `runtime.Callers`, **no call site gained a parameter**
- cap live-changeable: `Update()` returns `llmChanged` when `LLMHourlyCallCap` differs, so
  `reloadLLM()` rebuilds the flat client **and** swaps the graph adapter
- refusal wraps `ErrUnreachable` (so `IsRetryable` treats it as transient), makes no HTTP
  request, and contains the string `hourly cap`
- embeddings counted and attributed but **not** capped
- all 13 pre-existing tests restored after the worker deleted them; verified by name
- `go build ./...`, `go vet`, and tests in `llmgateway` / `config` / `worker` all clean

Default is `llm_hourly_call_cap = 0` = unlimited, so merging changes no behaviour by itself.

```bash
gh pr create --base main --head feat/llm-meter-and-cap \
  --title "feat(llmgateway): attribute every LLM call and cap them per hour" \
  --body "<see the commit message on 2073644>"
# then merge per Enzo's instruction
```

### 2. Deploy to the hub

`make deploy` is **broken** — it targets the retired VPS and builds linux/amd64. Use:

```bash
ssh enzo@payments 'cd ~/go/src/github.com/agent-mem && git pull --ff-only \
  && PATH=/opt/homebrew/bin:$PATH docker compose up -d --build worker'
```

### 3. Verify the running binary carries the change (never trust the build log)

```bash
ssh enzo@payments 'PATH=/opt/homebrew/bin:$PATH docker exec agent-mem-worker-1 \
  grep -c "hourly cap" /usr/local/bin/agent-mem'
```

Must be >= 1. If 0, the deploy did not take — stop and investigate before proceeding.

### 4. Set the cap to 300/hour

Must go through the API: `handleUpdateSettings` calls `s.config.Update(partial)` on the
live in-memory config. **A raw `psql` UPDATE does nothing until a restart.**

300/hour is ~4x the measured steady-state need and bounds the worst case at ~7,200/day
instead of an open tap.

The conductor's direct writes to prod settings are denied by the Claude Code permission
classifier, so stage a script and have Enzo run it. `/tmp/agentmem-pause.sh` on the hub is
the existing precedent for this pattern — mirror it.

Verify after: `GET /api/settings` shows `llm_hourly_call_cap: 300`.

### 5. Prune the duplicate job population

Measured on the hub. These make **zero** LLM calls but 93,517 executions will hammer the
Slack API and the database for no benefit:

| type | queued | note |
|---|---|---|
| `notify_watch_channels` | 62,418 | **61,223 carry the laptop's `machine_id`** — replicated in via sync |
| `derive_person_roles` | 31,099 | split across both machine_ids |
| `detect_hot_topics` | 3,860 | ignores `processing_paused`; uses the **expensive** tier |
| any type, `target_runner='vps'` | 529 | the VPS is retired — unclaimable forever |

Keep exactly **one** queued row per singleton type (earliest `available_at`), and delete
all `target_runner='vps'` rows. Do not touch `summarize_thread`, `fetch_body`,
`index_artifact`, `link_topics`, `describe_attachment`, `resolve_identity`,
`backfill_slack_thread` or `refresh_jira_board`.

Take a count snapshot before and after. This is a destructive prod write — stage it as a
reviewable script for Enzo to run, do not improvise SQL inline.

### 6. Resume the hub ONLY

```bash
ssh enzo@payments '/tmp/agentmem-pause.sh off'
```

The laptop stays paused. Do not run `/tmp/agentmem-pause-local.sh off`.

### 7. Watch for 60 minutes

Expected backlog on resume — measured, and mostly cheap:

| source | queued | calls | tier |
|---|---|---|---|
| flat `pending_messages` | 2,084 | 2,084 | haiku |
| link-topic cascade | 53 summaries | ~555 | haiku |
| attachment descriptions | 255 | 255 | describe |
| threads with no summary | 53 | 53 | sonnet |
| hot-topic detection | 3,860 jobs | ~20-100 | sonnet |
| **total** | | **~3,000** | + ~300 embeddings |

~3,000 calls is a few hours of ordinary running. **Anything far above that is the burner.**

Watch:

```bash
# per-hour call rate
ssh enzo@payments 'PATH=/opt/homebrew/bin:$PATH docker logs llm-gateway-llm-gateway-1 \
  --since 10m 2>&1 | grep -c "POST /generate"'

# THE POINT OF THE ROUND — which code path is calling
ssh enzo@payments 'PATH=/opt/homebrew/bin:$PATH docker logs agent-mem-worker-1 \
  --since 30m 2>&1 | grep -oE "caller=[a-zA-Z.]+" | sort | uniq -c | sort -rn'
```

Resume during Enzo's working hours — the burn is diurnal and will not appear at night.

### 8. Rollback at any point

```bash
ssh enzo@payments '/tmp/agentmem-pause.sh on'
```

Nothing is lost by pausing: Slack messages buffer to disk with fsync in p-agent and replay
from an offset; coding-hook events queue in `pending_messages`, where
`RequeueRetryablePendingMessage` *decrements* the attempt counter so an unavailable LLM
never burns retry budget; graph jobs are durable rows.

## Acceptance criteria

1. The deployed binary contains the cap string; `llm_hourly_call_cap` reads 300 via the API.
2. Exactly one queued row per singleton type; zero rows with `target_runner='vps'`.
3. The hub is running (`processing_paused=false`); the laptop is still paused.
4. After 60 minutes of Enzo's working time, the attribution histogram is reported verbatim
   — a ranked `caller=` count.
5. Either a named code path accounts for the excess, or the report states plainly that the
   burn did not reproduce and gives the observed rate.
6. `/generate` per hour is reported and compared against the ~3,000-call backlog estimate.
7. If the rate exceeds the cap's ceiling behaviour, the cap's refusals appear in the log as
   `hourly cap` warnings — confirming the ceiling actually binds.

## Report back

The attribution histogram, the hourly call rate, the before/after job counts, and a plain
statement of whether `agent-mem-5k0` is now identified. File a follow-up issue for the
named path. Do not fix it in this round — that is the next round, with evidence.
