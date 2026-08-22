# Resume plan — bring the hub back and name the quota burner

Written 2026-08-22 ~12:40 local (UTC+7), Saturday. Monitored resume, Enzo present.

Goal: answer `agent-mem-5k0` — which code path spent 600-850 LLM calls/hour for days — by
reading the `llm_caller` attribution histogram from a live hub. Everything else is secondary.

## What is queued right now (measured, not estimated)

### Graph jobs — 358 queued

| type | queued | LLM cost each | tier |
|---|---|---|---|
| `fetch_body` | 166 | none directly, but cascades on success | — |
| **`backfill_slack_thread`** | **100** (all `vps`-pinned) | **unbounded — see below** | cascade |
| `summarize_thread` | 45 | 1 | summary (sonnet) |
| `refresh_jira_board` | 18 | none | — |
| `link_topics` | 16 | up to ~10 judge calls | cheap (haiku) |
| `resolve_identity` | 5 | none | — |
| `describe_attachment` | 2 | 1 | describe |
| `refresh_slack_bots` / `_channels` / `monitor_hourly_report` / `detect_hot_topics` / `notify_watch_channels` | 1 each | `detect_hot_topics` ~20-100 | summary |
| `derive_person_roles` | 1 | none | — (available 2026-08-23, will not run today) |

### Flat memory — 2,308 pending messages

Coding-agent hook events. Each is 1 cheap generate + 1 embed. So ~2,308 haiku calls, ~2,308
embeddings. This is the bulk of the backlog and it is the cheap tier.

### Failed rows — 37,137, and they will NOT be retried

| type | failed |
|---|---|
| `fetch_body` | 15,138 |
| `resolve_identity` | 12,453 |
| `describe_attachment` | 7,614 |
| `index_artifact` | 1,491 |
| `link_topics` | 396 |

**Answering "will we process old data?" directly: no, not these.** `failed` is terminal — the
dispatcher only claims `queued`. Nothing re-enqueues them automatically. Most of the `fetch_body`
and `describe_attachment` failures are collateral from the missing Slack token, and now that Slack
works they *would* succeed — but reviving them is a separate, deliberate decision with its own cost,
not part of this resume. Do not bundle it in.

## The one thing to hold back: `backfill_slack_thread`

Those 100 rows have been stranded since 2026-08-16 because the hub's runner was `any` and they are
pinned to `vps`. Setting `AGENT_MEM_GRAPH_RUNNER=vps` (done, in the hub `.env`) makes them claimable
for the first time.

Each one fetches an entire Slack thread and ingests every message as a node. At a plausible 15
messages per thread that is ~1,500 new nodes, each cascading into `fetch_body` → `index_artifact`
(embed) → `summarize_thread` (sonnet) → `link_topics` (haiku judges). **This is the single item
whose cost cannot be bounded in advance**, and it contributes nothing to answering `5k0`.

**Decision: defer them past this window** by pushing `available_at` into the future. Reversible with
one UPDATE. Everything else runs.

## Cost ceiling, and why this is a safe experiment

`llm_hourly_call_cap = 300` per client, and there are two clients (flat 768-dim, graph 3072-dim), so
**generate calls are bounded at ~600/hour total** regardless of what misbehaves. That is the whole
point of the cap round: the worst case is now a known number rather than an open tap.

Embeddings are metered but deliberately *not* capped. They bill OpenRouter
(`google/gemini-embedding-001`) where only ~$10 remains until the 1st. At ~500 tokens per embed,
2,300 embeds is roughly 1.2M tokens — cents, not dollars. Acceptable.

Draining the full backlog at 600/hr takes ~8 hours. **We do not need that.** One hour gives ~600
attributed calls, which is plenty to see whether one caller dominates.

## Steps

### 0. Baseline (before touching anything)

Record: queued counts per type, `pending_messages` by status, failed total (37,137), and
`/generate` count in the last 10 minutes (must be 0).

### 1. Defer `backfill_slack_thread` (prod write — staged as a script for Enzo)

```sql
UPDATE graph.jobs SET available_at = NOW() + INTERVAL '7 days'
WHERE status = 'queued' AND type = 'backfill_slack_thread';
```

Expect `UPDATE 100`. Reversible: set `available_at = NOW()`.

### 2. Resume

```bash
ssh enzo@payments '/tmp/agentmem-pause.sh off'
```

### 3. Watch, in 10-minute samples

The question this round exists to answer:

```bash
docker logs agent-mem-worker-1 --since 10m 2>&1 | sed -e 's/\x1b\[[0-9;]*m//g' \
  | grep -oE 'llm_caller=[a-zA-Z0-9._()*-]+' | sort | uniq -c | sort -rn
```

Note the field is `llm_caller`, **not** `caller` — renamed in PR #37 because `caller` is a reserved
zerolog name that renders as a message prefix and makes every `grep caller=` return nothing.

Also each sample: `/generate` per minute, `hourly cap` refusal count, queued counts, failed delta.

### 4. What the histogram should look like if nothing is wrong

| caller | expected share |
|---|---|
| `worker.(*Server).processObservation` | the large majority — flat backlog, 2,308 messages |
| `handlers.summarizeThreadHandler` (or similar, now that attribution walks past the adapter) | tens |
| `handlers.linkTopics…` | ~160 over the window |
| `handlers.detectHotTopics…` | 20-100, once |

**A caller that is not in that list, or one wildly out of proportion, is the burner.** In particular
watch for a `cluster_summary` / dashboard read path — `agent-mem-5k0`'s standing hypothesis is that a
GET handler calls the expensive tier outside the job queue.

### 5. Abort criteria — any one of these, pause immediately

- `/generate` sustained above ~900/hour once the flat queue is drained (means something is calling
  outside both meters, or a third client exists)
- `failed` count climbing by more than ~50 in a 10-minute sample
- any job type reaching `attempts >= max_attempts` in bulk (the refund path should prevent this —
  if it does not, PR #37's Fix A is wrong and we stop)
- `hourly cap` refusals appearing for a tier we did not expect to saturate
- Anthropic quota warnings of any kind

Rollback is always: `ssh enzo@payments '/tmp/agentmem-pause.sh on'` — nothing is lost, Slack
buffers to disk in p-agent, flat events requeue with a refunded attempt, graph jobs are durable rows.

### 6. Stop condition

After ~60 minutes of live traffic during Enzo's waking hours, or sooner if an abort criterion trips.
Report the histogram verbatim and state plainly whether `5k0` is identified.

## Known-broken things that will appear in the log and are NOT the burner

- `describe_attachment` (2 queued, 7,614 failed) — the `enzobot` Slack app has no `files:read`, so
  file downloads fail with `missing_scope`. Expected; tracked separately.
- GitHub fetches — `AGENT_MEM_GH_TOKEN` is deliberately unset, so they fail fast with `ErrFatal`.
- `TestIngestURL_AlreadyFresh` / `TestImportBambooHR` — pre-existing test failures, unrelated.
