# Payments as the root entity: what to take from Hindsight, and what not to

Written 2026-09-27. Sources: production DB on the hub (read-only queries,
`ssh enzo@payments`), this repo at `a4d1e39`, and the Hindsight checkout at
`~/go/src/github.com/hindsight` (`ccfe85b48`), read by a second Claude session
whose full answer is in `~/go/src/github.com/hindsight/HINDSIGHT-FOR-AGENT-MEM.md`
(file:line citations for every Hindsight claim below live there).

## The answer in four lines

1. Do not replace agent-mem with Hindsight, and do not run it as a sidecar. Its
   memory model is a flat bag of atomic facts per bank. The thing you want,
   Payments > epic > issue > thread/PR/incident/doc, is a hierarchy, and that is
   exactly what Hindsight does not model and agent-mem already half has.
2. Port four of its ideas into agent-mem: a temporal arm, a keyword arm with RRF
   fusion, an explicit hierarchy that every search arm can scope to, and
   precomputed per-epic briefs (their "mental models").
3. Skip its observation/consolidation layer and the cross-encoder. They cost the
   most and fit our artifact-level data the least. Revisit with a measured trial
   only if the four ports leave a gap.
4. Before any of that, fix three data gaps that would silently cap the result:
   embedding coverage (56% of Slack, 35% of Jira, 44% of PRs), empty Jira
   metadata (no status, dates, type, epic on the node), and epic membership that
   lives in a side table the search path cannot see.

## What Hindsight is, against what we run

| | agent-mem graph (prod today) | Hindsight |
|---|---|---|
| Unit of memory | one row per artifact: 37.3k Slack messages (about 3k summarized threads), 1.6k Jira issues (538 PAY), 1.5k PRs, 157 Confluence, 99 GDocs, 88 Datadog, 12 PagerDuty | one row per extracted fact (what/when/where/who/why, occurred_start/end), `world` or `experience` |
| Synthesis | thread summaries (2.7k substantive), cluster summaries on demand (65), hot-topic DMs | observations (deduped beliefs with proof counts), mental models (standing answers refreshed in the background) |
| Entities | typed nodes + 157 `feature` entities + 943 people with org tree, teams, ACL | flat `entities` table per bank, trigram + co-occurrence resolution, no hierarchy |
| Edges | REFERENCES 18.2k (deterministic), SAME_TOPIC 7.4k (LLM-judged, with topic/why/confidence), REFERS_TO 338 | temporal (within 24h), semantic (top-50 ANN), caused_by |
| Time | `nodes.created_at` (event time), 316 nulls | `occurred_start`, `occurred_end`, `mentioned_at` on every fact |
| Search | one semantic arm (3072-d halfvec, HNSW) + weighted sum sem .50 / rec .15 / edge .15 / team .15 / auth .05 (`internal/graph/handlers/search.go`) | four arms in parallel (semantic, BM25, graph, temporal) → RRF k=60 → cross-encoder → ±10% multiplicative boosts → token budget |
| Context bundle | `graph_resolve`: seeds → BFS depth 2, 0.5 attenuation per hop, greedy hydrate to a token budget | `recall` with `max_tokens`; `reflect` agentic loop, 10 iterations max |
| Scoping | ACL by org membership (`graph.member_scopes`) | tags pushed into every arm, five match modes |
| Runtime | one Go binary + Postgres 16 + llm-gateway, on the Mac mini | Python API + worker + Postgres; in-process bge-small (384-d) and MiniLM reranker; 3.7 GB arm64 image, 2 GB RAM |

The honest read of that table: Hindsight is ahead on retrieval technique and on
precomputed answers. agent-mem is ahead on everything that is org-shaped
(people, teams, ACL, typed artifacts, cross-source edges). Retrieval technique is
portable. Org shape is not.

## Why not adopt it

Numbers, not taste:

- Re-extraction. Every artifact would go through Hindsight's LLM extractor: one
  call per 3,000-character chunk, plus `ceil(facts/8)` consolidation calls. The
  37k Slack messages alone are tens of thousands of calls against our 300/hour
  cap, to re-derive facts from threads that already have summaries.
- Re-embedding. Hindsight fixes the embedding dimension per install and pgvector
  HNSW on plain `vector` stops at 2000 dims. Our 3072-d halfvec index (23k rows,
  494 MB) cannot be reused; everything is re-embedded with bge-small-en, a
  384-d English model.
- Hierarchy. Hindsight entities have no parent column and no entity-to-entity
  edge. "Payments > PAY-2307 > PAY-2333" can only be expressed as facts that
  happen to share tags. We would compute epic membership in agent-mem anyway and
  push it in as tags, then keep two graphs in sync.
- Two systems. A second queue (`async_operations`), a second dashboard, Alembic
  migrations on every startup in our Postgres, and a Python process next to the
  Go worker on the Mac mini.

Where it would win: the coding-agent integration (`hindsight-integrations/
coding-agents`) overlaps our flat memory (157k observations, 11.5k session
summaries), and its "boot from settled knowledge pages" idea is good. That idea
is what section 5 ports, without the runtime.

## The design: Payments as root, epics as first-class nodes

### Hierarchy

```
business:payments                     new node type `business`, one row
  └─ jira:PAY-2307  (epic)            existing jira node, PART_OF business
       └─ jira:PAY-2333 (issue)        existing jira node, PART_OF epic
            ├─ slack:C…:ts  (thread root)   REFERENCES issue  (exists, 2,455 slack→jira edges)
            ├─ gh_pr:wego/payments#N        REFERENCES issue  (exists, 994 gh_pr→jira edges)
            ├─ cf:…, gws_doc:…              REFERENCES issue  (exists)
            └─ datadog:…, pagerduty:…       via the thread that references them
```

`PART_OF` is already named in the schema comment (`migrations/20260527000001_graph_schema.sql`)
and the extractor (`internal/graph/extractor/extractor.go:34`) but zero rows
exist. Epic membership today is `graph.jira_epic_map` (538 rows, 279 on the
active board), which only the `/live` PINS board reads. Search, resolve, MCP and
the notifier cannot scope by epic.

The deterministic part is cheap: `refresh_jira_board` already pulls issue→epic
every 6h for all 538 PAY issues. Emitting `PART_OF` edges from the same rows, plus
one `business:payments` root, is a small change to that job.

### Membership: three sources, measured coverage

Membership of a thread/PR/doc in an epic subtree comes from three sources, in
decreasing confidence. The numbers say why all three are needed.

| Source | Rule | Coverage today |
|---|---|---|
| Key reference | artifact REFERENCES a PAY issue → member of that issue's epic | 1,129 Slack messages, 202 PRs, 219 Jira, 11 Confluence reach a PAY issue. Only 11 board epics get any Slack traffic this way; PAY-1983 "ad hoc Requests & live issues" takes 327 of it and PAY-2307 190. 206 references land on issues with no epic (64 of 538 PAY issues have none). |
| Topic link | artifact SAME_TOPIC (confidence ≥ 0.8) to a member → member, one hop only | 7.4k SAME_TOPIC edges exist with `confidence`; propagation is a SQL join, no LLM |
| Payments-eligible, no epic | eligibility gate says "payments" but no key and no link → member of `business:payments` only | 4,759 eligible messages, of which 21 carry a PAY key. This is most of the payments conversation. |

The third row is the important one. Nearly all payments chatter has no ticket
key in it. An LLM epic classifier (input: thread summary + the ~20 on-board epic
titles and one-line descriptions; output: epic key or none, cached by summary
signature) is the only way to lift those into a subtree. It is per-thread not
per-message (about 3k threads, cheap tier), and it should be a later phase,
measured against a labelled sample before it writes edges. Hindsight has nothing
equivalent; its entity resolution is name matching.

Store the result in one table:

```sql
graph.epic_membership(node_id, epic_key, via, confidence, first_at, last_at)
-- via ∈ {key, epic_self, topic_link, classifier}; PK (node_id, epic_key)
```

Rebuilt from edges by `refresh_jira_board` (6h) to start; move to incremental
writes from `link_topics` and `index_artifact` when staleness shows up. A
node can be in several epics; `first_at`/`last_at` come from the members'
`created_at` and give each epic an activity window for free.

Caveat the data raised: the Jira epic tree is only as good as Jira hygiene.
PAY-1983 is a bucket, not a topic. The design should not pretend otherwise; the
per-epic brief for a bucket epic will read like a changelog, and that is fine.

## Retrieval: four arms, fusion, scope, budget

Current `/api/graph/search` is one ANN query with a weighted sum on top. Port the
Hindsight shape but keep our signals:

| Arm | Hindsight | agent-mem port |
|---|---|---|
| Semantic | HNSW per fact type | exists: `artifact_index.embedding` |
| Keyword | tsvector `ts_rank_cd` (their `native` backend) | add a generated `tsvector` column on `artifact_index` over summary + title + identifiers, GIN index (no `pg_trgm`, no new extension; `vector` and `citext` are the only ones installed). Identifiers already solved the exact-match gap once (`migrations/20260712000001_identifier_linking.sql`); this generalizes it. |
| Graph | entity co-membership + semantic links + causal, from 20 seeds | reuse `bfs.Expander` from the top 10 semantic hits, one hop over REFERENCES / SAME_TOPIC / PART_OF, score `1/(1+hop)` (`scoring.Edge` exists, unused in search) |
| Temporal | parse window → overlap on occurred/mentioned → 60 ANN candidates in-window → 8 time buckets round-robin → spread along temporal/causal links ×0.7 | parse window in Go (month, month+year, "last week/month", quarter, ISO, "since X"; give a bare month the whole month, Hindsight's parser turns "in August" into one day) → filter `COALESCE(created_at, first_seen_at)` between, or the epic's `first_at`/`last_at` overlap → 8-bucket coverage → spread one hop via REFERENCES/PART_OF ×0.7 |

Then:

- Fusion: RRF with k=60 over the arms that ran. Rank-based, so BM25 scores and
  cosines never need calibrating against each other.
- Boosts: turn today's additive weights into multiplicative `1 + α(signal − 0.5)`
  factors with α = 0.2 for recency, temporal proximity and team, 0.1 for
  authority. Same signals, but they can no longer outvote relevance.
- Scope: every arm takes `epic_keys []string` and/or `business` and filters through
  `graph.epic_membership` in SQL, the way Hindsight pushes tags into every arm.
  ACL scope stays as it is, applied in the same WHERE.
- Budget: `hydrate.Greedy` already packs to a token budget; add Hindsight's
  skip-and-continue rule (a result too long for what is left is skipped, not
  terminal) and the "always return the top one" floor.
- Reranker: none. Hindsight's cross-encoder is a 90 MB local torch model; we
  have no torch on the hub and no gateway rerank endpoint. Their own docs say
  the pipeline works without it (RRF-derived scores). Add an LLM rerank of the
  top 20 only if the eval set in section 8 shows ordering problems.

Surface: `/api/graph/search` gains `epic`, `since`, `until`, `arms` parameters and
returns per-arm ranks in `score_breakdown` (the dashboard already reads that
object). `graph_search` in the MCP server gains the same fields. `graph_resolve`
gets `epic` as a seed form (`epic:PAY-2307` expands to the subtree roots).

## Per-epic briefs: the mental-models port

Hindsight's best idea for us is not the observation layer; it is that a
standing question gets a standing, background-refreshed, delta-edited answer,
and reading it is a DB read.

We already have the mechanics in two places and both are the wrong shape for
epics: thread summaries are keyed by one thread; cluster summaries are keyed by
a BFS neighbourhood whose signature invalidates on any neighbour change and
drags in loosely related tickets (memory `project_agent_mem_summary_pipelines`,
decided 2026-07-01). An epic is a deterministic, stable set, which is what makes
a cached brief safe.

```sql
graph.epic_briefs(epic_key PK, brief TEXT, highlights JSONB, open_items JSONB,
                  member_signature TEXT, sources TEXT[], updated_at, previous TEXT)
```

- Trigger: a job `refresh_epic_brief` runs when `member_signature` (hash of
  member node ids + their thread-summary signatures) changes, with a minimum
  interval of 1h per epic. Hindsight's `delta` mode is the model: feed the LLM
  the previous brief plus only the members whose summaries changed since
  `updated_at`, and ask for edits, not a rewrite. That is the same
  incremental pattern `summarize_thread` already uses on a thread.
- Cost ceiling: about 25 on-board epics × at most 1 summary-tier call per hour,
  in practice a few calls per day. Within the 300/h cap with room to spare.
- Keep `previous` so a brief that says something surprising can be diffed.

Where it lands:

1. `/live` board section already groups threads by epic (`pins.go` `board()`);
   the brief is the swimlane header's expandable text.
2. MCP: new tool `graph_epic(key)` returning brief + members by type + activity
   window. Coding agents in the payments repo call it once instead of ten
   `graph_search` calls.
3. Session start in the payments repos: when the branch name carries a PAY key,
   inject that key's epic brief (DB read, zero LLM). This is Hindsight's
   knowledge-pages-at-boot idea applied to our flat-memory context builder
   (`internal/context/builder.go`).
4. The notifier: a hot-topic DM about a thread in epic E can carry E's brief
   line, not just the thread summary.

## What not to port, and when to reconsider

- Observations / consolidation. Hindsight's unit is the atomic fact; ours is the
  artifact with a summary. Cross-thread beliefs are what SAME_TOPIC plus the
  epic brief approximate for us, at a fraction of the calls. Reconsider only if,
  after the ports above, the eval questions that need "what do we believe about
  X across many threads" still fail. The cheapest trial then is one Hindsight
  bank fed only PAY-epic artifacts (about 3k threads + 538 issues + 200 PRs)
  through the Go client, tagged `epic:<key>`, consolidated `per_tag`, scored on
  the same eval set. Not before.
- Cross-encoder reranker. No local torch, no gateway endpoint. RRF first.
- Reflect loop. `graph_cluster_summary` and `graph_resolve` already give a coding
  agent a bounded bundle; an agentic loop over our own MCP tools is the agent's
  job, not the server's.
- Their coding-agent hooks. Our flat memory already does session capture; the
  one thing to lift is the boot-time page (item 3 above).

## Data gaps that gate all of this

Found in production; each one caps a later phase if left alone.

1. Embedding coverage. `artifact_index` has 40.5k rows and 23.3k embeddings:
   Slack 21.0k/37.3k, Jira 568/1,622, PRs 651/1,495, Confluence 127/157. Every
   arm except keyword reads this table. The keyword arm is partly a workaround
   for this gap; closing it (index the PAY subtree first) matters more than any
   ranking change.
2. Jira metadata is `{}` on every jira node. The fetcher requests
   `status,assignee,reporter,labels,created,updated` (`internal/graph/fetchers/jira.go`)
   and the normalizer keeps only summary and description. Status, issue type,
   created/resolved dates and the epic link belong on the node for temporal
   filtering and for "what is open on this epic".
3. The Slack unit is the message, not the thread. All 37.3k slack nodes are
   `slack:<channel>:<ts>` with a `thread_ts` in metadata; p50 body is 108
   characters. Thread-level summaries exist for 2.7k threads. Membership and
   briefs must be computed per thread root and inherited by replies, or the
   subtree fills with one-line fragments.
4. `created_at` null on 316 slack nodes; the temporal arm falls back to
   `first_seen_at`, which is ingest time, not event time.
5. Job churn: in the last 30 days `notify_watch_channels` ran 490k times and
   `summarize_thread` 494k, nearly all signature short-circuits (bd
   `agent-mem-3z0`). Not a blocker, but any new self-rescheduling job must not
   copy that cadence.
6. Epic hygiene: 64 PAY issues have no epic; PAY-1983 is a catch-all. The design
   handles it (business-level membership), the reader should know it.

## Phases and sizes

Each phase is independently shippable and has its own check. Sizes are my
estimate against this codebase, not measured.

| # | Phase | Change | Check |
|---|---|---|---|
| 0 | Data gaps | Jira normalizer keeps status/type/dates → `nodes.metadata`; backfill `created_at`; enqueue `index_artifact` for the PAY subtree first (about 3k threads + 538 issues + 200 PRs) | embedding coverage ≥ 95% on subtree; `metadata->>'status'` non-empty on PAY nodes |
| 1 | Hierarchy | `business:payments` node; `PART_OF` edges from `refresh_jira_board`; `graph.epic_membership` with `via` in {key, epic_self, topic_link}; `expandableThrough` treats `business`/epic as non-traversable hubs | `select epic_key, count(*)` matches the board; a thread linked by SAME_TOPIC ≥ 0.8 to a member appears with `via=topic_link` |
| 2 | Retrieval | keyword arm (tsvector), temporal arm (Go window parser + bucket coverage), graph arm from semantic seeds, RRF, multiplicative boosts, `epic`/`since`/`until` params on search + MCP | eval set below; per-arm ranks visible in `score_breakdown` |
| 3 | Epic briefs | `graph.epic_briefs`, `refresh_epic_brief` job with signature + 1h floor + delta edits; `/live` header; MCP `graph_epic`; session-start injection by branch PAY key | brief for PAY-2307 names India GST work, its open items, and cites member node ids; refresh count ≤ 25/day |
| 4 | Epic classifier (optional) | cheap-tier per-thread classification of eligible-but-unkeyed threads into epics, cached by summary signature, dry-run first | precision ≥ 0.85 on a 100-thread labelled sample before writing edges |
| 5 | Hindsight trial (only if 2+3 leave a gap) | one bank, PAY subtree only, Go client, `per_tag` consolidation | same eval set, side by side |

Order matters: 0 before everything (it is also the cheapest), 1 before 2
(scoping needs membership), 2 before 3 (briefs are built from scoped retrieval).

Phase 4 goes through a canary and a per-node cost check (memory
`feedback_cap_and_monitor_llm_runs`).

## Eval set

Ten questions to freeze before phase 2 and run after each phase, with the
expected node ids written down once by hand:

1. What happened on the India GST epic in August 2026?
2. Which PRs closed PAY-2333 and what did the review discuss?
3. What is open on PAY-2307 right now?
4. Who owns the Razorpay checkout UI change and what did they decide?
5. Which incidents (Datadog/PagerDuty) touched the tax determination path since July?
6. Show the thread where the refund for a customer was confirmed from the gateway side. (identifier recall)
7. What did payments discuss last week that has no ticket? (business-level, no epic)
8. Which epics were active in the first week of September?
9. Was `tax_country` renamed from `site_code`, and where? (keyword arm, SAME_TOPIC edge exists)
10. What did the PK VAT CSV work depend on in another team's ticket? (cross-project edge, DECLAW-197)

Today's search answers 6 and 9 partly through `identifiers`; 1, 3, 7 and 8 it
cannot express at all.

## Decision needed

Two things only:

1. Direction: port the four ideas into agent-mem (this document), rather than
   trial Hindsight first. My recommendation is the port; the trial is phase 5
   and conditional.
2. Phase 0 + 1 as the first round. They are the cheapest, they are deterministic
   (no LLM), and phases 2 and 3 cannot be evaluated without them.

Anything past that becomes a written round plan per phase, as usual
(memory `feedback_plan_before_implement`).
