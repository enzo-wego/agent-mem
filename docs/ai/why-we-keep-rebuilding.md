# Why we keep rebuilding the whole corpus — and how to stop

Analysis written 2026-08-21 at Enzo's request: "why we need this action… could we
build data only one time, and re-use, re-build relationship later. this is second
time we rebuild all data."

All numbers measured on the production hub.

## Correction to the premise, first

**The rebuild is not what burned a week of quota.** The two are separate problems and
only one of them is expensive.

| | calls | when |
|---|---|---|
| the full summary rebuild (2026-08-17) | **1,347**, one time | finished 13:03, verified `remaining: 0` |
| the unattributed sustained burn | **600-850/hour hub + 500-730/hour laptop**, for 12h+ | still unexplained (`agent-mem-5k0`) |

The sustained burn is roughly **15,000-19,000 calls per day** — about 11-14x the entire
rebuild, every day, for days. That is what consumed the quota. The rebuild was a rounding
error against it.

So: fixing rebuild economics is worth doing (this document), but it will **not** fix the
quota problem. The unattributed path will. Do not let the rebuild question displace
`agent-mem-5k0`.

A second correction: the topic-link judging runs on `GenerateCheap` -> `claude-haiku-4-5`
(`link_topics.go:723`), not sonnet. Earlier in the session it was described as "17,262
claude-sonnet-5 calls", which was wrong on the model. Only summaries are sonnet.

## Corpus, for the cost math

| | count |
|---|---|
| thread summaries | 1,853 |
| slack nodes | 27,858 |
| artifact_index rows | 29,387 |
| topic-link judgments | 48,188 |

A full rebuild today: **1,853 sonnet** (summaries) + **~19,400 haiku** (judging, at the
measured 10.47 per node) + 1,853 embeddings (OpenRouter credits, not the Claude seat).

## Why a full rebuild was genuinely unavoidable this time

The `firstLine(body, 280)` bug was in the **transcript builder** — the *input* to every
summary. Every stored summary had been derived from truncated input, so every one was
wrong. A derived artifact cannot be repaired without re-deriving it. That part was real.

What was *not* inevitable is that it cost the whole corpus. Three design choices made the
blast radius total rather than partial.

## Cause 1 — search depends 100% on LLM output (the big one)

`indexSummaryForSlackRoot` (`index_artifact.go`) embeds only:

```go
return topic + "\n\n" + overview, "thread_summary"
```

The LLM summary, and nothing else. The raw Slack text is never embedded.

And the schema forbids embedding both:

```sql
CREATE TABLE graph.artifact_index (
  node_id      TEXT PRIMARY KEY REFERENCES graph.nodes(id) ...
  embedding    VECTOR(768),
```

`node_id` is the PRIMARY KEY — **exactly one embedding per node**.

Meanwhile the raw text is already stored, durably, and never used for retrieval:

```sql
CREATE TABLE graph.artifact_bodies (
  node_id    TEXT PRIMARY KEY ...
  body_full  TEXT NOT NULL,             -- full normalized body
```

So the artifact that is free, permanent and never invalidated by any code change
(`body_full`) is unused for search, while retrievability is staked entirely on the
artifact that is expensive and invalidated by every summarizer change.

That is why a summarizer bug becomes a *search outage* and forces an urgent full rebuild.

## Cause 2 — cache keys are hand-bumped global version numbers

The key is `v<N>:<msg-count>:<newest-updated-ms>`. Bumping `v<N>` invalidates **100% of
rows**, whether or not a given row's input actually changed.

This is not the second rebuild. `cluster_summary.go` documents its own invalidations in
comments — v3, v5, v6, v7, v9 — **five** of them. And `graph.thread_summaries` still
holds live rows at three different versions right now: v9 1,663 / v5 87 / v7 67.

Every one of those version bumps was a full re-derivation.

## Cause 3 — relationships are coupled to summaries

`topicLinkContentHash` (`link_topics.go:395`) includes **both** summaries. So changing one
summary invalidates every judgment it participates in — measured at 10.47 per node. That
coupling is *correct for accuracy* but it means a summary rebuild silently drags a
relationship rebuild behind it, and the relationship layer is 91% of the total cost.

We already broke this coupling by hand this session with `skip_judging`, taking the
measured cost from 17,262 to 1,347. That worked. It just is not the default.

## The fix — Enzo's instinct, made concrete

"Build once, reuse, rebuild relationships later" is the right model. Four changes, in
descending value.

### 1. Embed the raw body as well as the summary (biggest win)

Change `graph.artifact_index`'s primary key from `node_id` to `(node_id, kind)` so a node
can carry two vectors: one from `artifact_bodies.body_full`, one from the LLM summary.

Body embeddings are derived from text that **never changes**, so they are computed once
and are never invalidated by any prompt, model, transcript-builder or version change —
ever. Search then degrades gracefully: a summarizer bug costs ranking quality, never
retrievability, and fixing it stops being urgent.

Cost: a one-time embedding pass over 27,858 nodes on OpenRouter credits — **not** the
Claude subscription — plus index size. This is the change that makes the whole class of
incident non-urgent.

### 2. Key the cache on the input hash, not a version number

Hash the *rendered transcript* instead of bumping a global `vN`. Then a builder change
regenerates only the threads whose transcript actually differs.

Be honest about the payoff: for the `firstLine` bug ~80% of threads contained a multi-line
message, so this would have saved only ~20%. But for the *typical* change — author label
formatting, department suffixes, adding `[T#]` markers — most transcripts are untouched
and the saving approaches 100%. It converts "every summarizer edit costs the corpus" into
"an edit costs what it actually changed".

### 3. Never rebuild relationships as a side effect

Make `skip_judging` the default on every bulk path, not an opt-in the conductor remembers.
Re-judge lazily on view, or in an explicit, capped, separately-approved pass. This alone
is the 91%.

### 4. Estimate before executing

The actual failure on 2026-08-17 was not technical: nobody knew the operation was 17,262
calls until it was already running. An `?estimate=true` on the backfill endpoints
returning "N rows x M calls per row = X total" would have surfaced that in one second, and
Enzo would have rejected it before it started — exactly as he did once he saw the number.

Pair it with the hourly cap (`agent-mem-0x7`, landed 2026-08-21) as the backstop for when
an estimate is wrong.

## Recommended order

1. `agent-mem-5k0` — attribute the sustained burn. This is the actual quota problem.
2. #4 estimate flag — cheapest possible guard, hours of work.
3. #3 skip_judging by default — removes 91% of any future rebuild.
4. #1 body embeddings — removes the urgency of rebuilds permanently.
5. #2 input-hash keys — removes the routine cost of summarizer edits.

## What NOT to do

- Do not stop generating summaries. They are what makes the topic/cluster views and the
  link judge work; they are a good product feature with a bad dependency shape.
- Do not switch the summarizer to a cheaper model to make rebuilds affordable without
  measuring quality first. That trades a cost problem for a silent quality problem.
- Do not delete the version-key mechanism outright. Prompt and model changes DO need a
  deliberate global invalidation; the fix is that transcript-builder changes should not
  have to use the same hammer.
