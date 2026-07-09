# Review — docs/plans/14-topic-linking.md

Date: 2026-07-09 · Reviewer: Claude (Fable 5) · Verdict: **approve after 4 fixes folded into the plan**

## Verified claims (all check out)

- Heuristic-embed bug is real: `internal/graph/handlers/index_artifact.go:76-79` embeds `heuristicSummary(...)` and stores `summary_kind='heuristic'` — never reads `graph.thread_summaries`.
- The inert `0.45` threshold lives at `internal/graph/bfs/expand.go:88` (`similarThreadMinCosine`).
- Index name in the migration SQL matches reality (`idx_artifact_index_embedding`, migrations/20260527000001_graph_schema.sql:103).
- `graph.edges.kind` is plain TEXT with no CHECK constraint — `SAME_TOPIC` inserts fine.
- pgvector 0.8.2 halfvec HNSW ≤4000 dims: correct, 3072 clears it.

## Blockers

### B1 — Global embedding dims config is shared with core memory tables
`gemini_embedding_dims` is a single config (internal/config/config.go:77) baked into the one
Gemini client (internal/gemini/client.go:23,148). That same client embeds into the **core**
`observations` / `session_summaries` / `user_prompts` tables (internal/worker/processor.go:129,196,
internal/worker/handlers.go:129, internal/search/search.go:29) — all `vector(768)`
(migrations/20260323000000_init_schema.sql). Flipping the config to 3072 breaks every core-memory
insert and search with a dim-mismatch error.

**Fix (Phase 1):** per-call dims — e.g. `Embed(ctx, text, dims)` or a second `EmbedGraph` method /
graph-specific dims config. Core stays 768; graph moves to 3072. Small change, must ship with the
migration. Graph query-side embedding (internal/graph/handlers/search.go:99) must also use 3072 +
`SEMANTIC_SIMILARITY` in the same deploy.

### B2 — `graph.edges` has no `metadata` column
The plan's edge INSERT writes `metadata jsonb_build_object('confidence',…)`, but the table
(migrations/20260527000001_graph_schema.sql:74-86) has no metadata column, and the real column
names are `from_node_id`/`to_node_id`, not `src`/`dst`.

**Fix (Phase 3):** migration `ALTER TABLE graph.edges ADD COLUMN metadata JSONB`. While there,
make the symmetric-dedupe mechanism explicit: `UNIQUE(from_node_id, to_node_id, kind)` does not
block the reversed pair — canonicalize `SAME_TOPIC` as `least(a,b) → greatest(a,b)` before insert.

## Gaps

### G1 — TRUNCATE wipes 9 node types; backfill restores 2 Slack channels
`TRUNCATE graph.artifact_index, artifact_bodies, thread_summaries` deletes embeddings for all
16,220 slack + 636 jira + 372 gh_pr + … nodes and every channel's thread summaries. The backfill
restores only payments-team + payments-dev. Consequences the plan doesn't state:

- Similar-threads popup, search, and summaries go dark for **every other channel** and for all
  standalone jira/cf/docs until scope widens. The "after: yes" table in Part 02 won't be true in v1.
- The hybrid-scope goal (link standalone Jira/CF) needs those nodes re-indexed; nothing in Phase 1
  re-embeds them.

**Fix:** either state the regression is accepted for v1, or extend backfill to re-index non-Slack
nodes referenced by the two channels' threads (they're needed anyway for resource-aware summaries
and as `SAME_TOPIC` targets — edges FK into `graph.nodes`, and linking to a jira node with no
embedding means it can never appear in a shortlist).

### G2 — "relative cutoff" is undefined
It's the gate that controls all LLM-confirm spend, and the plan never gives the formula. Pick one
before Phase 3 (e.g. per-query `mean + 2σ` over the corpus distribution, or top-k with a max-gap
drop). Re-running the Part 01 distribution after Phase 2 gives the data to choose it — add
"pick cutoff formula" as an explicit Phase 2→3 gate output.

## Minor

- Signature-change re-check: say explicitly that a confirm flip to "different topic" **deletes**
  the stale `SAME_TOPIC` edge, not just skips re-inserting.
- `ALTER COLUMN … TYPE halfvec(3072)` works on the truncated table (pgvector registers the
  vector→halfvec cast), but `USING embedding::halfvec(3072)` is cheap insurance.
- LLM-confirm one-time volume is bounded and fine: ~8 shortlist × threads in 2 channels ≈ low
  tens of thousands of cheap-model calls.

## Bottom line

Evidence (Part 01) and design (Part 02) are solid — embed the resource-aware summary, LLM decides
linkage, org graph only re-ranks. Fold B1 into Phase 1, B2 into Phase 3, resolve G1's scope
statement, and define G2's cutoff as a Phase 2 exit criterion. Then approve.
