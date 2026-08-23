# Plan — revive the high-confidence failures on the hub (safe batch)

Written 2026-08-23. **Self-contained: assume no memory of the conversation that produced it.**

Target: the hub only — `ssh enzo@payments`, repo `~/go/src/github.com/agent-mem`, containers
`agent-mem-worker-1` / `agent-mem-postgres-1`, docker at `/opt/homebrew/bin`.
**Do not touch the laptop instance or its database.**

## Context: this is coverage recovery, not repair

The hub is already healthy, measured 2026-08-23 04:22 UTC (**the DB clock is UTC; local is
UTC+7 — do not misread scheduled jobs as overdue**):

- flat memory drained: 0 pending, 101,757 completed, 12 rows stuck in `processing`
- 134 graph jobs queued, **0** in `processing`, dispatcher completing ~19 jobs/30min
- no invalid indexes; zero null embeddings in `graph.artifact_index`
- `detect_hot_topics` down to 2 queued, from a 4,678-row treadmill during the burn incident

So nothing here fixes a broken hub. It recovers artifacts lost while the Slack token was
missing.

## Goal

Requeue only the failures that will actually succeed now, and unstick the flat rows. Measure
the real cascade cost, so the decision on the larger 7,700-row batch rests on numbers instead
of estimates.

## Scope: exactly these, and nothing else

Total 2,318 graph jobs + 12 flat rows.

| type | error signature | count | why it succeeds now |
|---|---|---|---|
| `fetch_body` | `slack fetcher: API error: not_authed` | 1,199 | token was absent, now restored and verified live |
| `fetch_body` | `jira fetcher status N` | 784 | Atlassian token now present |
| `link_topics` | `confirm: invalid JSON` | 335 | transient parse failure, retryable |
| flat | `pending_messages.status='processing'` | 12 | orphaned by worker restarts; oldest 2026-06-22, newest 2026-08-15 |

`status` allows `pending|processing|completed|failed` (checked), so resetting to `pending` is
valid.

## Explicitly OUT of scope — do not touch

- **The 7,700 `fetch_body` → `ratelimited` failures.** They failed *because* of Slack pacing;
  a burst re-run reproduces the failure exactly. They need throttling that is not built. This
  round exists partly to size that decision.
- **~22,600 permanently dead failures.** Retrying re-fails; leave them:
  `resolve_identity` bot-no-user-info 9,810 · `describe_attachment` download/API 6,692
  (**blocked on the `files:read` Slack scope**) · `resolve_identity` duplicate-key 2,642 ·
  `fetch_body` no-fetcher-for-`feature:*` ~1,719 · `index_artifact` node-not-found 1,371
  (the node is gone — nothing to index) · `fetch_body` `not_in_channel` 340.
- **The 100 deferred `backfill_slack_thread` rows.** Deliberately pushed to ~2026-08-29;
  unbounded cascade, and out of scope here.
- Do not delete any rows. Do not change `llm_hourly_call_cap`. Do not alter gateway config —
  it was just corrected and verified (`OR_MODEL_CHEAP=anthropic/claude-haiku-4.5`,
  `OR_MODEL_SUMMARY=anthropic/claude-sonnet-4.5`, all backends `claude`,
  `FALLBACK_ON_QUOTA=false`).

## Approach

### 1. Baseline first — record before writing anything

Counts of `graph.jobs` by status+type; `pending_messages` by status; `graph.nodes` total
(34,354) and `graph.artifact_index` total (29,629); `/generate` calls in the last 10 minutes.
Without this the cost measurement afterwards is meaningless.

### 2. Requeue, in three separate statements

One statement per signature so each row count can be checked against the table above. Match
on `status='failed'` AND `type` AND the `last_error` signature — never on `type` alone, which
would sweep in the dead rows.

Each statement must also **reset `attempts = 0`** and set `available_at = now()`. Requeuing
without resetting `attempts` means the row is already at `max_attempts` and dies on first
claim — the requeue would look successful and do nothing.

Expect exactly: 1,199 / 784 / 335. **If a count differs materially, stop and report** rather
than proceeding — it means the signature match is wrong.

### 3. Unstick the 12 flat rows

`UPDATE public.pending_messages SET status='pending' WHERE status='processing'` — expect 12.

### 4. Watch the cascade for ~45 minutes

Each `fetch_body` success cascades: `index_artifact` (1 embedding) and possibly
`summarize_thread` (sonnet) plus `link_topics` (~15 haiku judge calls). **The cascade, not the
retry, is the cost.**

Sample every 10 minutes: queued counts by type, `failed` delta, `/generate` per minute, cap
refusals, and `graph.artifact_index` growth.

### Abort criteria — any one, pause immediately with `/tmp/agentmem-pause.sh on`

- `failed` climbing more than ~200 in a 10-minute sample (means the signature match was wrong
  and dead rows are churning)
- jobs reaching `attempts >= max_attempts` in bulk
- `/generate` sustained above ~900/hour
- any Anthropic quota warning

## Acceptance criteria

1. Exactly 1,199 + 784 + 335 rows requeued; 12 flat rows returned to `pending`.
2. No row outside those signatures modified — verify the dead-category counts are unchanged
   (`resolve_identity` still 12,461 failed, `describe_attachment` still 7,644,
   `index_artifact` still 1,491, `fetch_body` `ratelimited` still 7,700).
3. After the window: report `graph.artifact_index` growth, new `failed` count by type, total
   `/generate` calls consumed, and embeddings issued.
4. **A measured per-artifact cascade cost** — generate calls and embeddings per recovered
   artifact. This is the deliverable that sizes the 7,700-row decision.
5. Gateway config unchanged; cap unchanged; all tiers still `claude`.
6. Hub still healthy: 0 jobs stuck in `processing`, flat queue draining.

## How to verify

Read the DB directly; do not infer from logs alone. Attribution for generate calls is the
`llm_caller` log field (**not** `caller` — that name is reserved by zerolog and renders as a
message prefix, so `grep caller=` always returns nothing):

```bash
docker logs agent-mem-worker-1 --since 10m 2>&1 | sed -e 's/\x1b\[[0-9;]*m//g' \
  | grep -oE 'llm_caller=[a-zA-Z0-9._()*-]+' | sort | uniq -c | sort -rn
```

Report the numbers verbatim, including any that undercut the case for the larger batch.
