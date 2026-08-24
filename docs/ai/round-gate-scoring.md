# Plan — make the eligibility gate's scores separable, embeddings only

Written 2026-08-24. **Self-contained: assume no memory of the conversation that produced it.**

Repo `~/go/src/github.com/agent-mem`. Hub `ssh enzo@payments`. Follows
`docs/ai/round-eligibility-gate.md`, shipped as `0f6d481` and live in `mode: dry_run`.

## Decision taken

**Stay on embeddings. Do not put a cheap-tier LLM call in the ingest hot path.**

Reason, measured today: `/embed` goes to OpenRouter over HTTP — no subprocess, and it does not
compete for the 4-slot semaphore that now bounds `/generate` (`llm-gateway` `fa20422`). A
cheap-tier judge would spawn a Claude CLI subprocess per message and queue behind the same 4 slots
that `link_topics` saturates during bursts, where p50 is 12.3s. Embedding stays ~200–500ms
regardless of gateway load.

The `llm_adjudicate` path (`eligibility_gate.go:320`, `GenerateCheap`) stays **off**. It remains
the right tool for a narrow uncertain band later, not for every message.

## The two problems to solve

**1. Scores are compressed into an unusable band.** 24 live samples span 0.488–0.662. Nothing has
ever scored below `low_threshold: 0.45`, so zero messages have been marked ineligible. In
`enforce` today the gate would skip nothing while still paying one embed per message.

The suspected cause is asymmetry: `graph.topic_subscriptions.scope_definition` for id 1 is **1,524
characters** — a dense multi-topic specification (Tabby, valU, Apple Pay, STC Pay, Juspay, VCC…).
Embedding it yields a vector near the centroid of the whole payments region, so a 56-character
message cannot align sharply with it. Everything compresses toward the middle.

**2. The gate scores a single message, never its thread.** `ingest_content.go` passes
`req.Body` only. `req.Metadata.ThreadTs` is available in the same request but unused. Thread
replies are short and context-free by nature — "done", "yes please", "thank you for the update" —
so they score near the floor while carrying the *outcome* of a conversation. In `enforce` the gate
would keep the question and drop the answer. Observed already: "thank you for the update" scored
0.533.

## Phase 0 — offline experiment, no production change. Do this first.

**Do not build anything until this says it is worth building.** The whole premise — that exemplar
scoring separates better than scope-document scoring — is untested. Test it offline, where it costs
nothing.

1. Export the accumulated `graph.eligibility_decisions` rows joined to message bodies. The join is
   `graph.nodes.natural_key = channel_id || ':' || message_ts` (type `slack`).
2. **Hand-label each message** relevant / not-relevant **on its own content**, not by channel.
   Channel membership is not the label — the lowest score of the first 24, 0.488, was
   "@minh.do he talk to himself and he start talking nonsense" sitting in `payments-x-hotels-devs`.
   Judging by channel is what produced a wrong conclusion earlier in this work.
3. Score the same messages three ways, offline, against the same embedding model:
   - **A (current):** cosine vs the single 1,524-char `scope_definition`.
   - **B (exemplars):** cosine vs each of ~10–15 short exemplar messages that typify in-scope
     traffic, score = max. Draw exemplars from real high-scoring messages plus hand-written ones.
   - **C (scope, chunked):** split `scope_definition` into its listed topics, score = max cosine
     over chunks. Cheaper to build than B and tests the same hypothesis.
4. Report for each method: the score range, and the **separation** between labelled classes —
   specifically whether any threshold exists that captures most irrelevant messages while losing
   little relevant content. Report the false-negative rate at the best threshold, because that is
   the cost that matters.

**Go / no-go:** if none of A, B or C separates the labelled classes with an acceptable
false-negative rate, then embedding cosine cannot gate this corpus and the finding is that the
approach must change — report that plainly rather than shipping a threshold that looks tuned.
Do not proceed to phase 2 on a weak result.

## Phase 1 — thread awareness. Independent of phase 0, build it either way.

A reply whose thread root was already judged eligible must not be scored in isolation.

Implement as a **lookup, not an embedding**: if `req.Metadata.ThreadTs` is non-empty and differs
from `req.Metadata.Ts`, check `graph.eligibility_decisions` for a row matching
`(channel_id, thread_ts)` with `decision = 'eligible'`. If found, the reply is eligible without
being scored — no embed call, exact rather than approximate.

Cases to handle explicitly:
- **Root not in the table** (ingested before the gate shipped, or the root was never gated): fall
  through to normal scoring. Do not treat a missing row as ineligible.
- **Root scored ineligible:** still score the reply on its own. A thread can turn relevant.
- Record these in the audit table with a distinguishable decision or mode value so calibration can
  tell an inherited decision from a scored one. Do not silently write them as if they were scored —
  that would poison the score distribution used for calibration.

This also *reduces* cost: inherited replies skip the embed entirely.

## Phase 2 — new scoring method. Only if phase 0 justifies it.

Implement whichever of B or C won, keeping the existing shape: embeddings cached and keyed on a
version, computed once, not per message.

If exemplars win, they need somewhere to live and a version to key the cache on — the current cache
key is `eligibilityScopeKey{subscriptionID, refreshedAt}` off `scope_refreshed_at`. Adding
exemplars means that key must change when the exemplar set changes, or stale vectors will be served
from cache. Note that `detect_hot_topics.go` already maintains `scope_refreshed_at` on subscription
create/update.

Fail-open behaviour must be preserved exactly as it is: every error path returns "process the
message". A gate that drops data at ingest never creates the node, so a bug here loses observations
permanently.

## Phase 3 — re-collect, then calibrate. Once.

**Any change to scoring invalidates the accumulated data for calibration purposes.** Do not
calibrate against scores produced by a method that is no longer running, and do not calibrate
twice. Order is: land phases 1 and 2, let `dry_run` accumulate a full day under the new method,
then pick thresholds from that distribution with a sampled read either side of the boundary.

Set thresholds **asymmetrically in favour of processing**. A false positive costs one wasted LLM
call. A false negative loses an observation permanently — the node is never created and nothing
recovers it. The boundary belongs below a balanced-accuracy optimum.

## Non-goals

- **Do not enable `mode: enforce`.** Not in any phase of this plan. It stays `dry_run` throughout,
  and turning it on is a separate decision with the distribution in hand.
- **Do not enable `llm_adjudicate`.** Not before thresholds are calibrated — the uncertain band is
  currently almost the entire observed range, so it would fire on nearly every message.
- **Do not set `task_type` on embeddings.** `embedding_options.go:13` omits it because OpenRouter's
  embeddings API rejects it, and `internal/gemini/types.go:14` records that a vector embedded with
  a task type lands in a different space — changing it would require re-embedding every stored
  vector. Out of scope.
- Do not populate `gated_channels` here. It stays `[]` while in `dry_run` so scores are collected
  across all channels, which is what calibration needs. Populating it is part of the enforce
  decision.
- Do not change the embedding model or `GraphEmbeddingDims`.

## Hard constraints

- **NEVER run `internal/graph/handlers` tests against the live or dev database.** They call
  `truncateGraphHandlerTables`, and `openTestDB` refuses any DSN whose database name lacks "test".
  Use `agentmem_test` — locally `postgres://agentmem:agentmem@127.0.0.1:5433/agentmem_test?sslmode=disable`.
- Two tests fail on `main` already and are not yours:
  `TestImportBambooHR_CSVBytes_ParsesAndUpserts` and `TestIngestURL_AlreadyFresh` (`agent-mem-jvbg`).
- Migration version ids must be unique across concurrent branches. `20260824000001` and
  `20260824000002` are taken. Two branches once shared a version id and the second migration
  silently never ran, with `goose` still logging "Migrations applied".
- Dashboard changes require the embed rebuild before committing: `cd dashboard && npm run build`,
  then from the repo root `rm -rf internal/worker/dashboard/* && cp -R dashboard/dist/* internal/worker/dashboard/`.
- Deploying and merging to `main` each need explicit approval in the round that does them.

## Acceptance criteria

1. Phase 0 reports labelled separation for A, B and C, with the false-negative rate at the best
   threshold for each, and an explicit go/no-go on whether embedding cosine can gate this corpus.
2. Thread replies inheriting an eligible root are not scored in isolation, and inherited decisions
   are distinguishable from scored ones in the audit table.
3. A missing or ineligible thread root never causes a reply to be skipped.
4. Fail-open preserved on every error path, covered by tests.
5. `go build ./...` and `go vet` clean; gate tests pass against `agentmem_test`.
6. Still `mode: dry_run`, `llm_adjudicate: false`, `gated_channels: []` at the end of this round.
