# Quota burn containment — measured causes and the plan

Written 2026-08-19. All numbers measured on the production hub (`ssh enzo@payments`).
Supersedes the "stop the worker" suggestion made earlier in this session.

## The question this answers

After the `flattenLines`/`v9` fix (PRs #33/#34/#35), Enzo's Anthropic quota drained
twice. Is the fix the cause, and does anything major need to change?

**The fix is not the cause.** The whole backfill was 1,347 calls, once, and finished
2026-08-17 13:03. The hub has been making 600-850 `/generate` calls per hour
continuously since well before that.

## Measured burn

llm-gateway access log, `/generate` per hour:

| hour (UTC) | calls |
|---|---|
| 08-18 19 | 611 |
| 08-18 23 | 657 |
| 08-19 02 | 844 |
| 08-19 03 | 820 |

~12-20k calls/day. `summarize_thread` (`Generate` -> tier=summary -> `claude-sonnet-5`)
is the consumer: 22,392 runs today across **398 distinct threads** (56x per thread).
The handler skips on a two-signature cache hit, so ~16% become real calls — which is
the observed ~820/hour.

## What must NOT be done

### Do not disable the Slack ingest endpoint

`p-agent` (`~/go/src/github.com/p-agent`, launchd `com.enzo.p-agent`) receives Slack
over socket mode and POSTs `/api/graph/ingest/content`. Its forwarder
(`src/graph-ingest/forwarder.js:55-66`) classifies:

- network error / timeout / 5xx -> `RetryableError` -> line stays in the fsync'd
  NDJSON buffer and is replayed every 30s
- **4xx -> `FatalError` -> the line is SKIPPED permanently**

A disabled endpoint returns 4xx, so disabling it **silently loses Slack messages**.
It also saves almost nothing: only 344 new slack nodes arrive per day, while the burn
is churn over threads already ingested.

### Do not set `processing_paused = true`

`internal/config/config.go:103` — it suspends graph dispatchers **and** the flat-memory
pending-message loop. Flat memory (the coding-agent hooks) would stop.

### Do not stop the worker

It takes the HTTP API down with it. Slack survives (p-agent buffers on connection
refused — verified: `~/.enzobot/graph-buffer/pending-2026-08-19.ndjson`, live, with an
offset cursor, and a day's file is only archived once fully drained). But the
flat-memory hooks (`/api/hook/*`) are fire-and-forget with no buffer, so those are lost.

## Flat memory is not at risk from this plan

Separate subsystem: own table (`pending_messages`), own loop (1s ticker,
`internal/worker/processor.go:25-42`), own tier — `GenerateCheap` -> `claude-haiku-4-5`
for both observations and session summaries. Volume ~1,000-2,000/day. It does not touch
`graph.jobs`, so parking graph jobs leaves it running.

## Measured causes

| # | cause | evidence |
|---|---|---|
| 1 | 922 live duplicate `notify_watch_channels` (should be 1) | 922 x 12 ticks/hr = 11,064/hr — matches the measured 11,064 exactly |
| 2 | 529 jobs pinned to the retired `vps` runner | `target_runner='vps'`: 273 notify + 256 detect. Unclaimable, `due_now` since 2026-08-15 |
| 3 | `derive_person_roles` duplicated across two instances | 27,558 queued: `a0411a4a` 14,784 + `fa05616a` 12,774 |
| 4 | `summarize_thread` re-triggered ~308/min from a read endpoint | machine_id empty (locally created), 3 open connections from `100.73.237.58` |
| 5 | no LLM budget cap anywhere | `internal/llmgateway/client.go` — no ceiling, no accounting |

Causes 1-3 waste CPU, DB and Slack rate limit but make **zero** LLM calls
(`notify_watch_channels.threadTopic` is cached-only by design). Cause 4 is the quota.

## Plan

### Phase A — conductor, now, no code, no LLM, no downtime

1. Delete the 529 `target_runner='vps'` rows. They can never be claimed.
2. Collapse duplicate singletons to one row per type, keeping the earliest
   `available_at`: `notify_watch_channels`, `detect_hot_topics`, `derive_person_roles`.
3. Identify and stop the client on `100.73.237.58` driving cause 4 (open dashboard tab,
   or a second agent-mem instance polling). This is the single biggest quota win.
4. Re-measure `/generate` per hour. Expect it to fall to near zero.

### Phase B — code, AFTER 2026-08-22 03:00

Deliberately deferred: an `omp` worker pane bills the same Claude subscription
(observed "$4.30 (sub)"), so writing these fixes now spends the quota being protected.

- `agent-mem-rik` (P0) — stop singleton job populations growing. Root cause still
  unconfirmed; needs a reproduction, not a guess.
- `agent-mem-48e` (P1) — `link_signature` derives from linked node *titles*, so a new
  neighbour invalidates a thread and pays a full sonnet summary. Confirm it is the
  churn driver, then bound it.
- `agent-mem-0x7` (P1) — per-hour LLM ceiling in the gateway client, editable in the
  dashboard GUI, visible count.
- `agent-mem-tcg` (P2) — `detect_hot_topics.judgeTopic` uses the expensive tier for a
  boolean; move to `GenerateCheap`.

## Non-goals

- Do not change the summarizer model or prompt.
- Do not delete `thread_summaries` rows (`agent-mem-fs2` is a separate decision).
- Do not re-run the stale-summary sweep; the corpus is fully on v9 and `remaining: 0`.

## Verification

- `/generate` per hour from the gateway access log, before and after Phase A.
- `graph.jobs` queued counts per type: one row each for the three singletons, zero `vps`.
- Flat memory still live: `observations` count rising, `pending_messages` not backing up.
- Slack still live: newest `graph.nodes` timestamp advancing; p-agent buffer offset
  keeping pace with its pending file.

---

## Outcome — 2026-08-19

### Action taken

`processing_paused=true` via `PUT /api/settings` (HTTP 200, value persisted). Applied
live: `handleUpdateSettings` calls `s.config.Update(partial)` on the in-memory config.
A raw `psql` UPDATE would NOT have taken effect until a restart.

Both prod write attempts by the agent were denied by the Claude Code permission
classifier; Enzo ran the staged script `/tmp/agentmem-pause.sh on`.

### Verified stopped

- `/generate` calls in the 60s after the pause: **0** (was 600-850/hour)
- graph jobs claimed in the same 60s: **0**
- Slack ingest: still accepted, p-agent buffer keeping pace
- flat-memory hooks: still accepted, queueing in `pending_messages`

### Hypotheses DISPROVEN by measurement — do not re-litigate these

Three separate theories were tested against production and killed. Recorded so the
next session does not spend the effort again:

1. **`link_signature` churn.** No. `linkSignature` already guards legacy/empty rows
   and backfills them with no LLM call (`summarize_thread.go:196-215`).
2. **Permanent signature disagreement between writer and staleness checker.** No.
   Stored `signature` agrees with the `channels.go` live computation for **1,686 of
   1,699** rows.
3. **Sync re-touching `nodes.updated_at`.** No. **0** rows older than a day had
   `updated_at` move in a 10-minute window.
4. **`summarize_thread` was the quota consumer.** No — and this was asserted twice
   before being measured. 27,411 jobs completed with no error in 6 hours while
   `thread_summaries.updated_at` advanced only 1-6 times per hour. Essentially all of
   them took the `summarySkip` early return: no LLM call, no write.

### Still unattributed

The 600-850 `claude-sonnet-5` calls/hour have **not** been traced to a code path.
Tracked as `agent-mem-5k0` (P0). The remaining candidate class is GET handlers that
call `deps.Gemini.Generate` directly (`cluster_summary.go:647`,
`refresh_scope.go:225`) — they create no `graph.jobs` row, which is why every
queue-based diagnostic missed them, and nothing meters them.

**Instrument before fixing.** Add per-path attribution at the gateway so the next
occurrence names its own caller. Four inference-based hypotheses have already failed.

Confound: the pause and Enzo closing the dashboard tab happened within minutes of each
other, so the two cannot be separated from the available evidence.

### Unverified second LLM source

Machine `a0411a4a` (Enzo's laptop) runs its own agent-mem and pushes ~100 rows/min to
the hub via `/api/sync/push` — observations kept arriving on the hub after the pause,
from sync rather than local processing. If that instance has its own `llm-gateway` on
the same `CLAUDE_CODE_OAUTH_TOKEN`, pausing the hub did not stop that half. The measured
burn was on the hub gateway and is stopped; the laptop was never instrumented.

### Decision

Hold all code work until **2026-08-22 02:00 UTC (09:00 UTC+7)**. Accept a ~3-day gap in
hub-side flat-memory processing: hooks are still accepted and queue in
`pending_messages`, and the backlog drains on unpause. Nothing is lost.

Resume: `ssh enzo@payments '/tmp/agentmem-pause.sh off'`

Order of work on resume: `agent-mem-5k0` (attribution first), then `agent-mem-rik`
(duplicate singletons), `agent-mem-48e` (disagreeing staleness checks), `agent-mem-0x7`
(budget cap), `agent-mem-tcg` (wrong tier).

Deliberately NOT done: the queue cleanup (529 dead-`vps` rows, 922+334 duplicate
singletons). It is cosmetic while nothing claims jobs, and not worth another prod write
during the pause.

---

## Correction — there were TWO gateways, not one (2026-08-19)

The "burn confirmed stopped" claim above was measured against the **hub** gateway only.
It was wrong as a statement about total spend.

Enzo's **laptop** runs a complete second agent-mem stack: its own `agent-mem-worker-1`
(container created 2026-08-05), its own postgres, and its own llm-gateway. It had
`processing_paused=false` the entire time and was making **500-730 `/generate` calls per
hour, uninterrupted, for at least 12 hours** — right through the hub pause.

Real total was ~1,370 calls/hour. Pausing the hub removed about 60% of it.

### Two gateways on the laptop, and the container is the decoy

```
llm_gateway_url = http://host.docker.internal:8750
```

`host.docker.internal` resolves to the **host**, so the laptop worker talks to a
**native uvicorn**, not the docker container of the same name:

| listener | what it is | used by agent-mem? |
|---|---|---|
| `llm-gateway-llm-gateway-1` (docker, 127.0.0.1:8750) | container | **no** |
| `uvicorn app.main:app --port 8750` (PID, native, up 14d) | real gateway | **yes** |

Stopping the container alone changes nothing. Both must go down.

## Full shutdown state as of 2026-08-19 05:15 UTC

| target | action | verified |
|---|---|---|
| hub processing | `processing_paused=true` | 0 calls, 0 jobs / 45s |
| laptop processing | `processing_paused=true` | partially honored — `detect_hot_topics` still ran |
| laptop gateway (container) | `docker stop llm-gateway-llm-gateway-1` | `Exited (0)` |
| laptop gateway (native) | `kill <uvicorn pid>` | nothing on :8750, HTTP 000 |

Note: the laptop's pause flag is set but `detect_hot_topics` continued past it. Root
cause not investigated (Enzo scoped it out as small). Killing the gateway is what
actually stopped the laptop.

## Is anything lost? No — verified in code

- **Flat memory: safe, guaranteed.** `RequeueRetryablePendingMessage`
  (`internal/database/pending.go:96`) sets `attempts = GREATEST(attempts - 1, 0)` — an
  LLM-unavailable retry *decrements* the counter, so it can never exhaust the retry
  budget. Messages wait indefinitely and drain on resume.
- **Graph jobs: durable.** They are rows in `graph.jobs`. On the hub nothing is claimed
  at all while paused. Where a job does run against a dead gateway, the handlers return
  `nil` on transient LLM failure (`summarize_thread.go:168`) leaving the cache signature
  unadvanced, so the work is re-triggered later rather than lost.
  Exception: `link_topics` judgments are skipped and only re-run when the pair reappears
  in an embedding shortlist.
- **Slack: safe.** p-agent's fsync'd NDJSON buffer replays from its offset.

## Resume procedure for 2026-08-22 02:00 UTC (09:00 UTC+7)

Order matters. Do NOT start with `pause off` — a straight resume on the hub spiked it to
**3,480 calls/hour** as the paused backlog discharged at once.

1. Fix first (`agent-mem-5k0` attribution, then `rik`, `48e`, `0x7`).
2. Restart the laptop gateway only when needed:
   ```
   cd /Users/neocapitelo/go/src/github.com/llm-gateway
   .venv/bin/uvicorn app.main:app --host 0.0.0.0 --port 8750
   ```
   (the docker container is not the one in use; leave it stopped)
3. Resume ONE instance at a time, and cap the drain — do not unpause both at once.
4. Decide whether the laptop should run a worker at all (`agent-mem-l3o`). Two workers
   on one OAuth token, each with its own gateway, is the condition that made this
   incident invisible.

## Secret exposure

A `SELECT key||' = '||value FROM settings WHERE key LIKE '%gateway%'` printed
`llm_gateway_api_key` into the session transcript. Rotate if that transcript is shared.
The existing guidance — select specific keys, never a LIKE pattern over `settings` — was
already recorded and was not followed.
