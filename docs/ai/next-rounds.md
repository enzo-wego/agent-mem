# Execution queue — written 2026-08-24, before a /compact

**Read this first after compaction.** Two rounds are planned and approved but **not yet
dispatched**. Nothing is in flight. No worker is open.

## Order

| # | round | plan | bd | repo |
|---|---|---|---|---|
| 1 | Fix the gateway 500s | `docs/ai/round-gateway-timeout-500s.md` | `agent-mem-mdd5` | **`~/go/src/github.com/llm-gateway`** |
| 2 | Payments eligibility gate | `docs/ai/round-eligibility-gate.md` | `agent-mem-jstu` | `~/go/src/github.com/agent-mem` |

**Do round 1 first.** It is smaller, and the 500s are actively burning retry budget — 12
`link_topics` jobs have already failed permanently. Round 2 adds an embedding call per gated
message, and it is better to add load to a gateway that is not throwing 5% errors.

The two rounds touch **different repos**, so they may run as two concurrent workers without a
checkout race. One worker per repo, never two in the same directory.

## State of the system as of this note

Verified by direct query, not inferred:

- **Hub** = payments Mac mini (`ssh enzo@payments`), repo `~/go/src/github.com/agent-mem`,
  docker at `/opt/homebrew/bin`. Hub commit `c7a64bf`. VPS is retired; `make deploy` targets it
  and must not be used.
- **DB clock is UTC. Hub local is UTC+8. Enzo is UTC+7.** Three different clocks — do not read
  a future-scheduled job as overdue.
- `processing_paused = false`, `llm_hourly_call_cap = 300`.
- All three gateway backends `claude`; `OR_MODEL_CHEAP=anthropic/claude-haiku-4.5`,
  `OR_MODEL_SUMMARY=anthropic/claude-sonnet-4.5`, `FALLBACK_ON_QUOTA=false`. **Do not change
  these** — they were corrected and verified on 2026-08-23.
- Search index: deduplicated 2026-08-23. 0 duplicate embedding groups, self-recall 10/10 (was
  4/10). **Do not REINDEX** — the index was never the problem.
- Backfill cascade draining: ~119 `link_topics` queued at ~16/hr, cap-throttled. Some rows are
  at `attempts=4` of 5 and will die if they hit another 500 — which round 1 addresses.
- 100 `backfill_slack_thread` deferred to ~2026-08-29 on purpose (unbounded cascade). 30
  `refresh_jira_board` are future-scheduled and normal. Neither is unfinished work.

## Decisions already made — do not re-litigate

- **OpenCode Go is parked.** Evaluated 2026-08-23: 84% agreement, systematic over-linking bias,
  and Claude is not served on its endpoint. Full record and a revisit checklist:
  `docs/ai/results-ab-opencode-go.md`. Do not reintroduce it, including as the gate's
  adjudicator — that uses the existing cheap tier.
- **Do not add channels to the `graph.channel_filters` ignore list.** Enzo was explicit: the 8
  otherwise-unrelated channels carry occasional real payments content, and that is exactly why
  the gate decides per message. Existing channel filters stay as they are.
- **The 7,700 `fetch_body` ratelimited failures are NOT to be burst-requeued.** Requeuing just
  1,199 rows created ~343 new rate-limited failures (7,700 → 8,043), a 28.6% re-failure rate.
  That batch needs pacing built first.
- **`index_artifact` node-not-found failures (1,371) are dead** — the node is gone, retrying
  re-fails. Same for `resolve_identity` bot-no-user-info (9,810).

## Needs Enzo, not a worker

`describe_attachment` failures are growing (7,644 → 7,797) because the `enzobot` Slack app
lacks the **`files:read`** scope. Every backfill that recovers attachments adds to this pile.
No code fixes it.

## Hard constraints for any worker in these rounds

- **Never run `internal/graph/handlers` tests against the live or dev DB.** They call
  `truncateGraphHandlerTables` and the damage syncs to production. Use `agentmem_test`.
- **Do not touch the laptop instance or its database.** Parked by Enzo.
- Dashboard changes require the embed rebuild before committing: `cd dashboard && npm run build`,
  then from the repo root `rm -rf internal/worker/dashboard/* && cp -R dashboard/dist/* internal/worker/dashboard/`.
- `count(distinct embedding)` on `graph.artifact_index` crashes the Postgres backend (3072-dim
  halfvec). Group by `summary` instead.
- Deploying and merging to `main` each need explicit approval **in the round that does them**.
