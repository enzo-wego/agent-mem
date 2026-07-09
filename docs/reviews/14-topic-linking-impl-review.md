# Implementation review — feat/topic-linking @ 939024c (Codex)

Date: 2026-07-09 · Reviewer: Claude (Fable 5) · Plan: `docs/plans/14-topic-linking.md`
Verdict: **solid, faithful to the plan — 1 blocker (B-1) and 2 cost fixes (M-1, M-2) before deploy.**

## Fix verification — 347d6d0 ("Prevent transient topic-link failures from becoming facts")

All five findings addressed and verified in the diff; `go vet ./internal/... ./migrations` and
`go test ./internal/gemini ./internal/graph/... ./migrations` pass on the commit (clean worktree).

- **B-1 fixed.** `confirmTopicLink` now returns `(judgment, error)`; API error, empty output, and
  unparseable JSON all return `jobs.ErrTransient` — nothing cached, no edge deleted. Bonus found
  while verifying: `gemini.Client.Generate` sets `responseMimeType: application/json`, so fenced
  output can't happen on this path by construction.
- **M-1 fixed.** New `GenerateCheap` on the adapter routes the confirm gate to the Gemini client
  (bypassing the Claude summary generator). *Residual:* it uses the configured `gemini_model` —
  confirm the prod setting is a flash-tier model (deploy checklist item 0).
- **M-2 fixed.** `enqueueLinkTopics` always passes force=false from index_artifact
  (`linkTopicsForceFromIndexArtifact`).
- **M-3 fixed.** `skipTopicLinkSource` drops DM (`slack:D*`) sources before shortlisting.
- **M-4 fixed.** Alert bot roots with replies get a forced `backfill_slack_thread`
  (`ForceAlertThread: true`) from both the channel backfill and the live root-missing path, and the
  forced thread ingests its bot root/messages, so human-replied alert threads get full treatment.

Two small residuals, neither blocking:
1. `forceAlertThreadBackfill` triggers on `ReplyCount > 0` regardless of who replied — an alert bot
   that threads its own updates re-admits that thread (plan wanted *human* reply). Refine later by
   checking `reply_users` for a non-bot, or requiring a human message before summarize/link.
2. The live root-missing recovery path now passes `ForceAlertThread: true` for **all** threads, so
   bot messages in a recovered *normal* thread are ingested where they were previously skipped.
   Arguably fine (bot updates are context), but it's noise-policy drift — scope it to alert
   channels if it shows up in the corpus.

## Verified good

- **Gemini request shape is correct.** `embedContentConfig` is the current documented shape; the
  old top-level `taskType`/`title`/`outputDimensionality` are marked "Deprecated: Please use
  EmbedContentConfig.* instead" in the REST reference (checked live, twice). Deprecated top-level
  still exists, so the old core path was never at risk either.
- **B1 (dims split) done right.** `EmbedWithOptions` per-call options; all three graph embed sites
  use 3072 + `SEMANTIC_SIMILARITY` (index_artifact, describe_attachment, search query-side);
  core `Embed`/`EmbedBatch` keep client default 768. Interfaces extended in handlers + search.
- **Migration matches the plan verbatim** (USING clause, halfvec HNSW rebuild, truncate,
  `edges.metadata JSONB NOT NULL DEFAULT '{}'`), plus a working goose Down. New tables:
  `alert_fingerprints`, `alert_fingerprint_events`, `topic_link_judgments` (judgment cache, PK on
  canonical pair, content-hash column).
- **B2 done.** Canonical `least→greatest` pair ordering (`canonicalTopicPair`), ON CONFLICT upsert,
  and `deleteSameTopicEdge` on a flipped verdict.
- **Phase 2 done.** `index_artifact` prefers the cached thread summary for Slack roots
  (`summary_kind='thread_summary'`); freshness check compares `thread_summaries.updated_at` vs
  `refreshed_at`; `summarize_thread` enqueues re-index; re-index enqueues `link_topics`.
- **Phase 0 mostly done.** Alert-channel classification by name, fingerprint templating
  (urls/timestamps/numbers), counts, escalation on novel fingerprint and spike (≥20/hr with 1h
  cooldown), applied in backfill_slack, backfill_slack_thread, and ingest_content.
- **Sync carries `edges.metadata`** in export, import, and pull paths.
- **Display (Phase 5-lite):** raw `SIMILAR %` dropped; SAME_TOPIC shows topic label + confidence,
  `why` in the tooltip.
- **Tests verified locally**: `go vet` + `go test` on internal/gemini, graph/handlers, graph/bfs,
  migrations all pass on the branch (run in a clean worktree).

## Blocker

### B-1 — LLM failure is cached as a permanent "not same topic"
`confirmTopicLink` (internal/graph/handlers/link_topics.go) returns `{SameTopic:false}` when
`Generate` errors, returns empty, or returns unparseable JSON (e.g. code-fenced). The handler then
**saves that judgment with the current content hash** and **deletes any existing SAME_TOPIC edge**.
A rate-limit blip during the backfill — exactly when call volume peaks — permanently records
"different topic" for that pair until one summary's text changes. summarize_thread has the same raw
`json.Unmarshal` but on failure it saves *nothing* and retries later; link_topics caches the miss.

**Fix:** make `confirmTopicLink` return `(judgment, error)`; on error return `jobs.ErrTransient`
from the handler without touching the cache or edges. Only persist judgments that actually parsed.

## Should fix (cost)

### M-1 — Confirm model is claude-sonnet-5, not a cheap model
`deps.Gemini.Generate` routes to Claude when an Anthropic key is configured, and
`AnthropicModel` defaults to `claude-sonnet-5`. The plan locked "cheap (Haiku / Flash)" for the
confirm gate. Backfill = threads × up-to-12 judgments on Sonnet. Either wire a cheap model for
this call path or consciously accept the cost.

### M-2 — `force` propagation defeats the judgment cache
`summarize_thread` → `index_artifact{force:true}` → `enqueueLinkTopics(…, p.Force)` →
`link_topics{force:true}` bypasses the cache and re-judges **all** shortlisted pairs on every
summary refresh, even when the content hash is unchanged. Changed summaries already bust the hash
naturally — pass `force:false` to `enqueueLinkTopics` (force should only mean "skip the 24h
freshness window", not "burn LLM calls").

## Worth fixing

- **M-3 — DM source leak.** The shortlist excludes DM *candidates* (`scope LIKE 'slack:D%'`) but a
  DM *source* thread still gets linked outward; its topic/why (derived from the DM summary) lands
  in edge metadata visible in neighbors/dashboard. Add the same guard on the source node.
- **M-4 — Human-reply escalation is partial.** Human replies in alert channels are ingested (not
  skipped), but the bot root of that thread was already fingerprint-skipped — so the "thread
  becomes a normal thread → full treatment" step never happens; replies reference a root node that
  was never created. Needs a retroactive ingest of the root (or thread backfill enqueue) when a
  human reply arrives. The "×143/24h" rate display also isn't surfaced anywhere yet.

## Noted, acceptable for v1

- **G2 gate technically skipped:** cutoff hardcoded as per-query `mean+2σ` with 0.65 floor and
  LIMIT 12 — one of the review's candidate formulas, so fine as a start, but the plan's Phase-2
  exit step still applies: re-run the Part 01 distribution after backfill and validate before
  trusting edges. Constants are compile-time; move to settings if tuning becomes frequent.
- Edge metadata updates don't reset `sync_version` (updated judgments never re-sync) and edge
  deletes don't propagate (no tombstones). Fine for single-machine prod; known limitation.
- Shortlist does a full-corpus scan + stats per job (the stats CTE prevents HNSW use). ~17k rows →
  fine; revisit if the corpus 10×es.
- `alert_fingerprint_events` grows unbounded (one row per bot alert); add retention eventually.
  `decideAlertBot` does a per-message channel-name lookup during backfill (N+1) — harmless.
- `cachedTopicLinkJudgment` swallows real DB errors as cache misses — harmless (re-judge) but masks
  outages.
- Phase 4 (org re-rank of ties, cluster labels, importance ordering) not implemented — Codex didn't
  claim it; department **is** passed into the judge prompt as planned.

## Deploy checklist (B-1…M-4 fixed in 347d6d0)

0. Confirm the prod `gemini_model` setting is a flash-tier model (it's now the confirm-gate model).
1. Build amd64 → GHCR → VPS pull (`make deploy`). Migration runs and **truncates** graph index /
   bodies / thread summaries — dashboard similarity goes dark until backfill.
2. One-time live smoke test of the embed shape: request dims 8, assert an 8-length vector returns.
3. Run backfill for `payments-team` (C05RNSE8TBR) + `payments-dev` (CUV9EAYGY), last 3 months —
   scope is operational, nothing in code enforces it.
4. Re-run the Part 01 distribution (G2 gate) — the band must spread; validate the 0.65/mean+2σ
   cutoff against it before trusting SAME_TOPIC edges in the UI.

---

# Addendum — independent pass (Opus 4.8, 2026-07-09)

Second reviewer, read the branch code directly rather than trusting the Fable pass. Verdict:
**faithful to the plan; B-1 fix is real; ready for a scoped payments-team + payments-dev deploy.**
One net-new expectation gap, one fix applied this session, one cost gate verified green.

## Verified directly (all good)
- **B-1 fix is real.** `confirmTopicLink` returns `(judgment, error)`; API-error / empty / invalid-JSON
  all return `jobs.ErrTransient` (`link_topics.go:234-249`) and the handler propagates without touching
  the cache or edges (`:91-97`). A rate-limit blip no longer becomes a permanent "not same topic".
- **DB contracts the code relies on exist.** `upsertSameTopicEdge`'s `ON CONFLICT (from_node_id,
  to_node_id, kind)` is backed by a real `UNIQUE` (`graph_schema.sql:86`); `edges.machine_id` exists
  (`:85`). No runtime "no matching ON CONFLICT" crash.
- **Edges are read bidirectionally.** Only the canonical `least→greatest` edge is stored, but
  `expand.go:41-45` reads via `UNION` on both `from_node_id` and `to_node_id` (selecting `metadata`),
  so the "greater" node still surfaces its link + confidence. No directional dropout.
- **Idempotent linking.** Canonical pairing + `(source,target)` PK judgment cache + content hash →
  re-running for either endpoint converges on the same edge. Migration matches the plan.

## Net-new finding — `Describe` is a stub, images aren't folded in
`gemini_adapter.go:63` `Describe` returns `ErrFatal` ("not yet implemented"); `describe_attachment.go:114`
routes `image/*` straight to it. PDFs mostly survive via LiteParse; **standalone images/charts/
screenshots fail to describe** and never reach a summary. Pre-existing (not a topic-linking regression),
but it contradicts the design doc's "image/chart → describe → embed" claim. Plan doc corrected to reflect
this. Fix is describe-step only (no second embedding model): implement `gemini.Client.Describe`.

## Fixed this session (M-4 / residual #1) — 14-topic-linking, alert human-reply gate
`forceAlertThreadBackfill` fired on `ReplyCount>0` regardless of who replied, so an alert bot threading
its own updates re-admitted noise. Added `alertThreadHasNonBotReply(ctx, deps, msg.ReplyUsers)` gated at
the `backfill_slack.go` call site (and `reply_users` added to the message struct). Semantics **err toward
caring**: only skip when *every* replier is a known bot (`graph.people.is_bot=true`); unknown repliers
and DB errors count as human, so a real incident is never dropped. `go vet` + alert tests pass.
*Residual:* the live root-missing path in `ingest_content.go:379` still forces `ForceAlertThread` for all
recovered threads — separate, non-blocking; scope to alert channels later if bot noise shows up.

## Cost gate verified green (M-1)
Prod `AGENT_MEM_GEMINI_MODEL=gemini-3.5-flash` — `GenerateCheap` → Gemini client runs the confirm gate on
a flash-tier model as the plan locked. Deploy-checklist item 0 is satisfied. No code change.

## Security note
While reading the prod env, the worker's `AGENT_MEM_ANTHROPIC_API_KEY` was printed in cleartext into the
session transcript (mask pattern missed it). **Rotate that Anthropic key.**
