# Plan — stop indexing duplicate heuristic summaries, and clean up the 16k already there

Written 2026-08-23. **Self-contained: assume no memory of the conversation that produced it.**

Hub only — `ssh enzo@payments`, repo `~/go/src/github.com/agent-mem`, containers
`agent-mem-worker-1` / `agent-mem-postgres-1`, docker at `/opt/homebrew/bin`.
**Do not touch the laptop instance or its database.** Tracked as `agent-mem-4i3e`.

## The finding this fixes (measured 2026-08-23, hub)

`graph.artifact_index` holds 29,636 embedded rows but only **13,540 distinct embeddings**.
16,561 rows (**56%**) sit in duplicate groups carrying just **465 distinct summary texts**.
Largest single cluster: **3,893 rows sharing one identical embedding**.

Every large cluster is a recurring `wego-payments` production error alert with
`summary_kind='heuristic'`:

```
[wego-payments] *errors.Error: failed to find mapping from database: record not…   x3893
[wego-payments] *errors.Error: error calling GetTransaction: failed after retri…   x2638
[wego-payments] *errors.Error: best-effort sync failed: ERROR: duplicate key va…   x1209
```

**This is not a summarizer bug.** The same production error fires thousands of times, each
occurrence becomes a node, each gets the same heuristic summary, so each gets an identical
embedding. Legitimate ingestion, pathological result: a query landing near one of those 465
texts returns an arbitrary subset of a multi-thousand-row identical cluster and crowds out
everything else.

**The HNSW index is healthy — do not REINDEX.** Self-recall probe scored 4/10 with the index
and 6/10 with `enable_indexscan=off, enable_bitmapscan=off` (exact search): indistinguishable
on n=10, which rules the index out. Definition is correct
(`hnsw (embedding halfvec_cosine_ops)` on `halfvec(3072)`), valid, zero null embeddings.
`agent-mem-e02` ("HNSW has 0/10 recall") most likely misattributes this same symptom.

## Goal

One indexed representative per identical heuristic summary. Stop creating new duplicates at
the source, and neutralise the ones already stored — without deleting any rows.

## Approach

### 1. Prevention, at the writer (the part that matters)

`internal/graph/handlers/index_artifact.go:122` upserts into `graph.artifact_index`.
Before embedding, when `summary_kind='heuristic'`, check whether an identical
`(summary, summary_kind)` already has a **non-null** embedding. If it does, write the row with
`embedding = NULL` and **skip the embedding call entirely**.

This is strictly cheaper than today: a duplicate alert costs zero embedding calls instead of
one. `internal/graph/handlers/describe_attachment.go:202` is the other writer — apply the same
guard only if it can produce `heuristic` rows; check rather than assume.

Match on exact summary equality. Do not introduce fuzzy or near-duplicate matching — these
texts are byte-identical, and a similarity threshold would be a new source of wrong answers.

### 2. Cleanup, as data

For each `(summary, summary_kind)` group with more than one non-null embedding, keep the
embedding on **one** representative row and set `embedding = NULL` on the rest. Prefer the
oldest by `refreshed_at` as representative, so the choice is deterministic and re-runnable.

Expect roughly **16,096** rows updated (29,636 − 13,540).

**Do not delete rows.** `node_id` is referenced elsewhere, `identifiers` is used by
`internal/graph/handlers/identifiers.go`, and the rows carry sync state. Nulling an embedding
is reversible by re-running `index_artifact`; a delete is not.

### 3. Search correctness (required, not optional)

`internal/graph/handlers/search.go:135` orders by `ai.embedding <=> $1` with **no
`IS NOT NULL` filter**. A NULL embedding makes that expression NULL, which sorts last in ASC —
so null rows only surface when there are fewer matches than `LIMIT`, which is exactly the
long-tail query where a junk result is most visible. Add `AND ai.embedding IS NOT NULL`.

Flat search already does this (`internal/database/search.go:38`), so this aligns graph with
existing practice rather than inventing a rule.

Check the same for the other two vector readers and add the filter where the semantics need
it: `internal/graph/bfs/expand.go:120` and `internal/graph/handlers/link_topics.go:256`.

## Behaviour change this causes — intended, and worth stating plainly

A deduplicated occurrence becomes invisible to **semantic search** and to **`link_topics`
candidate generation** (its `src` embedding is NULL, so `link_topics.go:262` yields cosine 0
via the existing COALESCE). It remains in the graph, reachable by graph edges, identifier
lookup and exact queries.

For 3,893 copies of one error alert that is the desired outcome — you want one representative,
not 3,893 competing for the same result slot. A side effect is **fewer `link_topics` judge
calls**, which reduces cheap-tier LLM volume.

If any consumer depends on every occurrence being semantically searchable, stop and report
instead of proceeding.

## Non-goals

- **No REINDEX.** The index is healthy; the measurements above say so.
- **No row deletion**, no schema change, no occurrence-count column. If a count is wanted
  later it is derivable by grouping on `summary`.
- **No fuzzy dedup.** Byte-identical only.
- **Do not run the backfill** (`docs/ai/round-backfill-safe-batch.md`) in this round — it is a
  separate, already-planned decision.
- Do not change `llm_hourly_call_cap`, gateway config, or any tier backend. Gateway was just
  corrected and verified: all tiers `claude`, `OR_MODEL_CHEAP=anthropic/claude-haiku-4.5`,
  `OR_MODEL_SUMMARY=anthropic/claude-sonnet-4.5`, `FALLBACK_ON_QUOTA=false`.
- Do not touch flat memory or `public.observations`.

## Acceptance criteria

1. **No duplicate non-null embeddings remain**: `count(embedding)` equals
   `count(distinct embedding)` on `graph.artifact_index`.
2. Row count unchanged at 29,636 — nothing deleted.
3. Self-recall probe rises from 4/10 to **≥ 9/10**:
   ```sql
   with s as (select node_id, embedding from graph.artifact_index
              where embedding is not null order by random() limit 10)
   select count(*) filter (where s.node_id =
     (select a.node_id from graph.artifact_index a
      order by a.embedding <=> s.embedding limit 1)) from s;
   ```
   Run it twice — n=10 is small, and a single sample is not evidence.
4. A test proves `index_artifact` writes `embedding = NULL` and issues **no embedding call**
   for a heuristic summary that already exists indexed. Use the `agentmem_test` scratch
   database — **never** the live DB: handler tests in that package truncate the graph, and the
   damage syncs onward.
5. Graph semantic search returns no NULL-embedding rows, including when total matches are
   fewer than the limit.
6. `go build ./...` and `go vet ./...` clean. If the dashboard changes, rebuild the embedded
   bundle before committing (`cd dashboard && npm run build`, then from the repo root
   `rm -rf internal/worker/dashboard/* && cp -R dashboard/dist/* internal/worker/dashboard/`).
7. Hub still healthy afterwards: 0 jobs stuck in `processing`, dispatcher completing work.

## How to verify

Read the database, not the logs. Report the before/after of: total rows, non-null embeddings,
distinct embeddings, and both self-recall runs. Include any number that undercuts the change.

Baseline to compare against, measured 2026-08-23:
`nodes 34,354 · artifact_index 29,636 · non-null embeddings 29,636 · distinct 13,540 ·
rows in duplicate groups 16,561 · distinct summaries within those groups 465 · self-recall 4/10`
