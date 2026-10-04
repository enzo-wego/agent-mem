# Plan: Payments as root entity, four Hindsight ports

Written 2026-09-27. Companion to `docs/ai/hindsight-vs-agent-mem-payments-entity.md`
(the why). This is the what and the how, in dispatchable rounds. Baseline `main`
at `a4d1e39`. Hub: `ssh enzo@payments`, repo `~/go/src/github.com/agent-mem`.

Not in this plan: the LLM epic classifier (phase 4 of the analysis) and the
Hindsight trial bank (phase 5). Both wait for the eval numbers from round 3.

## Ground rules for every round

- One worker per round, `/team 1:omp` or codex, in a fresh branch off `main`.
  Never two workers in the same checkout.
- Handler tests only against `agentmem_test` (`unset DATABASE_URL` first);
  `internal/graph/handlers` tests truncate whatever DB they see.
- One `CREATE INDEX CONCURRENTLY` per migration file, `-- +goose NO TRANSACTION`,
  created with `make migration_create name=<name>` (never a hand-written
  timestamp). Prove separability: apply all, roll back the last, show earlier ones
  still applied, re-apply.
- Any new setting is editable in the dashboard Settings page, not only in
  `public.settings`.
- Any new LLM path is capped, canaried on 5 items, and its per-item call count
  measured before the full run.
- Deploy and merge to `main` each need explicit approval in the round that does
  them. Deploy command is in `CLAUDE.md` (the Mac builds arm64; `make deploy`
  targets the dead VPS).
- Dashboard changes: `cd dashboard && npm run build`, then
  `rm -rf internal/worker/dashboard/* && cp -R dashboard/dist/* internal/worker/dashboard/`
  before committing.

## Round 0: data gaps (no LLM, no ranking change)

Goal: the PAY subtree is fully indexed and Jira nodes carry the fields the later
rounds filter on.

### 0.1 Jira fields on the node

- `internal/graph/normalizer/jira.go`: keep `status.name`, `issuetype.name`,
  `created`, `updated`, `resolutiondate`, `labels`, `assignee.accountId`,
  `parent.key` (epic link) in the normalized result's metadata map.
- `internal/graph/fetchers/jira.go`: add `issuetype,resolutiondate,parent` to the
  `fields=` list.
- `internal/graph/handlers/fetch_body.go`: merge that map into
  `graph.nodes.metadata` (jsonb `||`) and set `created_at` from `created` when
  null.
- Backfill: one-off admin endpoint or reuse `backfill_api.go` to re-enqueue
  `fetch_body` for all `type='jira'` nodes (1,622), paced at the existing
  fetcher rate (7,700 ratelimited failures in the queue are the reason to pace).

### 0.2 Event time

- `backfill_created_at.go` already exists; extend it to the 316 Slack nulls
  (`created_at` = `to_timestamp(split_part(id, ':', 3)::numeric)`), and to
  `gh_pr` from the PR's `created_at` when the fetcher returns it.

### 0.3 Index the PAY subtree first

- Enqueue `index_artifact` (priority 3) for: every `jira:PAY-*` node, every node
  with a REFERENCES edge to one, every `gh_pr` referencing one, and every thread
  root with a substantive thread summary but no embedding. Roughly 3k threads +
  538 issues + 200 PRs. Paced by the existing `llm_hourly_call_cap`.
- Not a general "embed everything" run; the other 14k Slack rows without
  embeddings are mostly one-line replies and stay as they are.

### Verify

```sql
select count(*) filter (where metadata ? 'status') from graph.nodes where type='jira';  -- ≈ 1,600
select count(*) from graph.nodes where type='slack' and created_at is null;             -- 0
-- subtree coverage ≥ 95%
```

Estimate: 1 round, 1 worker, 1 to 2 days including the paced backfill.

## Round 1: hierarchy and membership (no LLM)

Goal: search, resolve and MCP can scope to an epic or to Payments.

### 1.1 Schema (three migrations, one object each)

1. `graph.epic_membership(node_id text references graph.nodes on delete cascade,
   epic_key text, via text, confidence float8, first_at timestamptz,
   last_at timestamptz, refreshed_at timestamptz, primary key (node_id, epic_key))`.
2. Index `(epic_key, last_at desc)` on it, concurrently.
3. Seed row `graph.nodes` id `business:payments`, `type='business'`,
   `natural_key='payments'`, `scope='jira'`; plus setting
   `graph.business_root_project = PAY` (dashboard-editable).

### 1.2 Edges

- `refresh_jira_board.go`: after upserting `jira_epic_map`, upsert `PART_OF`
  edges: `jira:<issue>` → `jira:<epic>` for every mapped row,
  `jira:<epic>` → `business:payments` for every key in the board's epic rank
  map. Delete `PART_OF` edges whose row disappeared. `metadata.method =
  'jira-epic-link'`.
- `neighbors.go` `expandableThrough`: return false for `type='business'`; keep
  epics under the existing degree cap so a 190-ref epic is not a corridor.

### 1.3 Membership table

Rebuilt by `refresh_jira_board` in the same run (6h), in this order, first
writer wins per `(node_id, epic_key)`:

| via | rule | confidence |
|---|---|---|
| `epic_self` | node is the epic, or an issue whose epic is E | 1.0 |
| `key` | thread root / PR / doc with REFERENCES to an issue in E, or to E | 1.0 |
| `topic_link` | node with SAME_TOPIC edge (confidence ≥ 0.8) to a `key` or `epic_self` member; one hop, never chained | edge confidence |

Slack replies inherit the root's rows (`metadata->>'thread_ts'`), so scoping
works for messages too. `first_at`/`last_at` = min/max of members'
`COALESCE(created_at, first_seen_at)` per epic, stored on the epic's own row.

Also `business:payments` membership: every epic member, plus every message with
an `eligible` row in `graph.eligibility_decisions` (`via='eligible'`,
confidence = gate score). That is the "no epic" tier.

### 1.4 Read side

- `/api/graph/search`, `/api/graph/resolve`, `/api/graph/neighbors`: accept
  `epic` (repeatable) and `business=payments`; add
  `AND n.id IN (SELECT node_id FROM graph.epic_membership WHERE epic_key = ANY($x))`
  to the existing WHERE next to the ACL predicate.
- `graph_resolve` seeds: `epic:PAY-2307` expands to the epic node plus its
  `epic_self` members.
- MCP `internal/graphmcp`: `epic` and `business` fields on `SearchInput`,
  `ResolveInput`; new `graph_epic(key)` returning members grouped by type with
  `via`, and the activity window. (Brief field added in round 3.)
- `/live` PINS board: no change needed; optionally read membership instead of
  recomputing epic groups in `pins.go` `board()` (leave for later if risky).

### Verify

```sql
select epic_key, via, count(*) from graph.epic_membership group by 1,2 order by 1,2;
-- PAY-2307 key count ≈ 190 direct refs + issue members; topic_link adds a measured delta
select count(*) from graph.edges where kind='PART_OF';  -- ≈ 474 issues + 25 epics
```

`curl '/api/graph/search?q=GST invoice&epic=PAY-2307'` returns only subtree
nodes. Unit tests: membership rebuild on a fixture graph (one epic, two issues,
one thread with a key, one thread linked by SAME_TOPIC at 0.85, one at 0.6 that
must not join).

Estimate: 1 round, 1 worker, 2 days.

## Round 2: retrieval (four arms, RRF, boosts)

Goal: `/api/graph/search` runs the arms the query needs, fuses by rank, and
reports per-arm ranks. Same endpoint, same response shape plus new fields.

### 2.1 Keyword arm

- Migration A: `ALTER TABLE graph.artifact_index ADD COLUMN tsv tsvector
  GENERATED ALWAYS AS (to_tsvector('simple', coalesce(summary,'') || ' ' ||
  array_to_string(identifiers,' '))) STORED`. `simple` config: identifiers and
  ticket keys must not be stemmed; titles join via the search query, not the
  column, to avoid a cross-table generated column.
- Migration B: GIN index on `tsv`, concurrently, own file.
- Query: `websearch_to_tsquery('simple', $q)` ranked by `ts_rank_cd`, joined to
  `graph.nodes` for title ILIKE as a second cheap signal, `LIMIT budget`.

### 2.2 Temporal arm

- `internal/graph/temporal/window.go`: `Parse(q string, now time.Time)
  (start, end time.Time, rest string, ok bool)`. Rules, in order: ISO date or
  range; `<month> <year>`; bare `<month>` (whole month, most recent occurrence
  not after `now`); `last week|month|quarter`; `this week|month`; `Qn YYYY`;
  `since <month|date>`; `yesterday|today`. Returns the query with the time phrase
  removed so the other arms embed the topic, not the date. Table-driven tests
  with `now` fixed to 2026-09-27; the "in August" → whole-month case is a
  named test.
- Arm: candidates where `COALESCE(n.created_at, n.first_seen_at)` overlaps
  the window, OR the node's epic row (`epic_membership.first_at/last_at`)
  overlaps; ordered by cosine to the rest-of-query embedding, `LIMIT 60`;
  then 8-bucket round-robin (`temporal/coverage.go`), then one-hop spread over
  REFERENCES/PART_OF with score × 0.7, window re-applied.
- Only runs when `Parse` returns ok, or when `since`/`until` params are set.

### 2.3 Graph arm

- Seeds = top 10 semantic hits. `bfs.Expander.Expand` one hop over
  REFERENCES, SAME_TOPIC, PART_OF; score `scoring.Edge(hop)` × edge confidence
  (1.0 for deterministic kinds). Skip nodes `expandableThrough` rejects.

### 2.4 Fusion and boosts

- `internal/graph/scoring/rrf.go`: `Fuse(lists map[string][]string, k=60)
  map[nodeID]struct{Score; Ranks map[arm]int}`.
- Replace `Combine` in the search path with: `final = rrf × (1+0.2(rec−0.5))
  × (1+0.2(team−0.5)) × (1+0.2(temporal−0.5)) × (1+0.1(auth−0.5))`, where
  `temporal` = proximity to window centre when a window exists, else 0.5;
  `rec` = the existing `scoring.Recency` (30-day half-life) mapped to [0,1].
  `resolve.go` keeps the additive `Combine` for now; it has a different job.
- Keep `graph.weights.*` settings readable but mark them legacy in Settings;
  add `graph.boost.alpha.{rec,team,temporal,auth}` as dashboard-editable
  settings.
- `score_breakdown` gains `ranks: {semantic: 3, keyword: 1, graph: -, temporal: 7}`
  and `rrf`.

### 2.5 Budget

- `hydrate.Greedy`: skip-and-continue when a candidate exceeds the remaining
  budget; if nothing fits, return the top candidate truncated to the budget.

### 2.6 Surface

- Params: `epic`, `business`, `since`, `until`, `arms` (csv, default all),
  `limit`. MCP `SearchInput` mirrors them.
- Dashboard Search page: show the per-arm rank chips from `score_breakdown`.

### Verify

- Unit: window parser table; RRF on two hand lists; coverage bucketing.
- Integration on `agentmem_test`: fixture with a dated thread, a key-only
  match, and a semantic-only match; assert each arm's rank and the fused order.
- Eval set (10 questions in the analysis doc), expected node ids written by
  hand before the round starts, scored as hit@5 per question, before and
  after. Record in `docs/ai/results-retrieval-round2.md`.
- Latency: p95 of `/api/graph/search` on the hub before and after, from the
  worker log; four arms in parallel should stay under 2× today's.

Estimate: 1 round, 1 worker, 3 to 4 days. The largest round; split the
temporal arm into its own PR if the worker asks.

## Round 3: per-epic briefs

Goal: each on-board epic has a standing brief that is a DB read to fetch.

### 3.1 Schema

- Migration: `graph.epic_briefs(epic_key text primary key, brief text,
  highlights jsonb, open_items jsonb, member_signature text, sources text[],
  updated_at timestamptz, previous text, previous_at timestamptz)`.

### 3.2 Job `refresh_epic_brief`

- Enqueued by `refresh_jira_board` after the membership rebuild, one job per
  on-board epic whose `member_signature` changed. Signature = sha256 over sorted
  member node ids + each member's `thread_summaries.signature` (or
  `artifact_index.refreshed_at` for non-Slack). Floor: skip if `updated_at`
  is under 1h old.
- Prompt inputs: epic title + Jira description (now on the node from round 0),
  member summaries changed since `updated_at` (delta mode) or all members
  capped at N=40 by recency (first build), and the previous brief. Output JSON:
  `brief` (≤ 200 words), `highlights[]`, `open_items[]` each citing member
  node ids. Summary tier via the gateway (`Generate`, not `GenerateCheap`).
- `UsesLLM: true`, `PoolSize: 1`, counted under `llm_hourly_call_cap`.
- Canary: 5 epics, dry-run flag that writes to the log not the table, then
  measure calls per epic (expect 1) before enabling.
- Setting `graph.epic_briefs.enabled` (dashboard) and
  `graph.epic_briefs.min_interval_minutes`.

### 3.3 Read side

- `/api/graph/epic/{key}`: brief + members by type + window + `updated_at`.
- MCP `graph_epic` gains `brief`, `highlights`, `open_items`.
- `/live` board: swimlane header expands to the brief; the "NEW" badge logic
  is untouched.
- Session start (`internal/context/builder.go`): when the hook's cwd is a
  payments repo and the branch matches `PAY-\d+`, look up the epic via
  `jira_epic_map` and prepend that brief to the flat-memory context. DB read
  only; skip silently when absent.
- `notify_watch_channels`: if the thread has an epic row, append one line
  "epic: <key> · <first sentence of brief>" to the DM.

### Verify

- The PAY-2307 brief names India GST, cites at least three member node ids
  that exist, and lists open items whose Jira status is not DONE.
- `select count(*) from graph.jobs where type='refresh_epic_brief' and
  enqueued_at > now()-interval '1 day'` ≤ 25 × 24 worst case; expect < 50.
- Re-run the eval set; questions 1, 3, 7, 8 should now be answerable.

Estimate: 1 round, 1 worker, 2 to 3 days.

## Order and dependencies

```
round 0 (data)  →  round 1 (hierarchy)  →  round 2 (retrieval)  →  round 3 (briefs)
                                             ↑ eval set frozen here
```

Round 0 and round 1 can run as two workers only if they are in separate
checkouts; they touch different files except `fetch_body.go` (round 0) and
`refresh_jira_board.go` (round 1). Round 2 needs 1 for scoping. Round 3 needs
2 for the retrieval the brief is built from and 0 for Jira status.

## bd issues to file when approved

- `agent-mem: round 0 data gaps (jira metadata, created_at, PAY subtree index)`
- `agent-mem: round 1 epic hierarchy + membership table + scoped search`
- `agent-mem: round 2 four-arm retrieval with RRF`
- `agent-mem: round 3 per-epic briefs`

Each round gets its own `docs/ai/round-*.md` brief in the usual self-contained
form before dispatch.

## Landmines (for the worker)

- Migrations are plain goose files named `YYYYMMDDHHMMSS_<name>.sql` in
  `migrations/`; there is no `make migration_create` here. Pick a timestamp
  later than the newest file. Handler tests apply them to `agentmem_test`.
- Baseline on this branch: `go build ./... && go vet ./...` clean. 4 failures in
  `internal/hooks` + `internal/skills` are pre-existing on `main`.
- Never set `DATABASE_URL` to the dev or prod DB when running tests; handler
  tests call `truncateGraphHandlerTables`.
- `count(distinct embedding)` on `graph.artifact_index` crashes Postgres.
- Every Slack node is a message; the thread root is `metadata->>'thread_ts'`
  (equal to the message ts for roots). Membership is computed on roots.
- Job handlers self-reschedule via `jobs.Entry`; do not add a new
  self-rescheduling job with a short tick (see the 490k/30d churn).
- Do not deploy, do not touch the hub or its DB, do not run backfills; code,
  migrations and tests only. The lead runs backfills after deploy.
- `internal/worker/dashboard/` is the embedded build; only rebuild it if you
  change `dashboard/src`.
