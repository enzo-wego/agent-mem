# Phase 4 — summary-only backfill: regenerate summaries without LLM re-judging

Worker brief. Self-contained: assume no knowledge of the conversation that produced it.

Issue: `agent-mem-xyx` (P0 incident) — run `bd show agent-mem-xyx` for the full record.
Repo: `/Users/neocapitelo/go/src/github.com/agent-mem`, branch `main`, clean.

## Why this exists

The `v9` signature bump (PR #33/#34) exhausted Enzo's Anthropic quota. agent-mem has no
LLM key of its own: it calls llm-gateway, which is configured
`LLM_GATEWAY_BACKEND_SUMMARY=claude`, `MODEL_SUMMARY=claude-sonnet-5`, billed to Enzo's
Claude subscription. The worker on the hub is currently **stopped** to halt the burn.

Measured cost per regenerated thread:

| step | Anthropic calls | note |
|---|---|---|
| `summarize_thread` | 1.00 | |
| `link_topics` LLM judging | **10.47** | 1,665 fresh judgments ÷ 159 jobs |
| `index_artifact` embed | 0 | goes to OpenRouter, separate credits |
| total | 11.47 | |

1,505 threads still need regenerating. At 11.47 that is 17,262 `claude-sonnet-5` calls,
which Enzo has ruled out. Switching to OpenRouter is also ruled out — only $30 of credits.

**91% of that cost is the topic-link re-judging, and it is not what we need.** The search
fix depends only on the summary and its embedding, and the embedding is on OpenRouter. So
regenerate summaries, skip the LLM judging, and the whole corpus costs **1,505 calls** at
identical summary quality.

The deferred link re-judging is accepted, recorded debt — see "Known debt" below. Do not
try to solve it here.

## Goal

A backfill that regenerates thread summaries and embeddings while making **zero**
`link_topics` LLM calls, and still doing `link_topics`' free deterministic work.

## The key subtlety — read before designing

Do **not** simply stop enqueueing `link_topics`. That job does useful non-LLM work first:
`materializeThreadReferences` (`link_topics.go` ~line 115) turns pasted Slack permalinks
into deterministic `REFERS_TO` edges before any LLM is touched. Dropping the job would
throw that away for free.

Instead, let `link_topics` run and return **after** the deterministic step and **before**
any judging.

## The change

Thread one flag through three payloads. Name it `SkipJudging` everywhere — it is precise
about what is skipped (LLM judging), not "linking" in general.

### 1. `linkTopicsPayload` (`link_topics.go:34`)

Add `SkipJudging bool \`json:"skip_judging,omitempty"\``.

In the handler, after `materializeThreadReferences` succeeds and **before**
`shortlistTopicLinks` / `identifierCandidates` / any `confirmTopicLink`, return `nil` when
`p.SkipJudging` is set. Log at info with the node id so a skipped judge pass is visible in
the worker log.

Place the early return so the deterministic `REFERS_TO` materialization still happens.

### 2. `indexArtifactPayload` (`index_artifact.go:15`)

Add `SkipJudging bool \`json:"skip_judging,omitempty"\``.

At `index_artifact.go:137` the handler calls `enqueueLinkTopics`. Keep enqueueing — pass
the flag through so the queued `link_topics` job carries `skip_judging`. `enqueueLinkTopics`
(`link_topics.go:805`) builds a `map[string]any`, so add the key there and give the
function a `skipJudging` parameter.

### 3. `summarizeThreadPayload` (`summarize_thread.go:45`)

Add `SkipJudging bool \`json:"skip_judging,omitempty"\``.

The handler enqueues `index_artifact` with a `map[string]any` at ~line 162; add
`"skip_judging": p.SkipJudging`.

### 4. `enqueueSummarizeThread` (`summarize_thread.go:366`)

Add a `skipJudging bool` parameter. There are **8 existing call sites** — update each to
pass `false` explicitly. Prefer one function with the parameter over a second near-duplicate
helper; an explicit `false` at each site documents the intent.

### 5. `BackfillStaleThreadSummaries` passes `true`

The sweep is the backfill, and its whole purpose is now the cheap summary-only path, so it
always sets `skipJudging = true`. **Do not add a request field to turn judging back on** —
that is a knob nobody has asked for, and the quota reality says the sweep must never judge.
Document that in the function's doc comment.

### 6. Raise the default cap 20 → 100

`backfillStaleSummariesDefaultLimit` in `backfill_api.go`. At 1 Anthropic call per thread
instead of 11.47, the old canary cap of 20 is needlessly slow — 1,505 rows would be 76
POSTs. Leave the 500 ceiling and the 400 reject path unchanged. Update the constant's
comment: it currently justifies 20 by citing the ~15-judge cascade, which no longer applies
to this path.

## Non-goals — do NOT do these

- **Do NOT change the summarizer model, prompt, or the gateway config.** Summary quality
  must stay byte-for-byte comparable to today. `claude-sonnet-5` stays.
- **Do NOT add an age/date filter to the sweep.** At 1 call per thread the full corpus is
  affordable; a window would only reduce coverage.
- **Do NOT add an age bound to `/api/graph/channel/topics`.** Once the backfill lands, no
  thread is stale, so the lazy path has nothing to regenerate and the bound is dead code.
- **Do NOT touch `flattenLines`, `threadSummarySigVersion`, or the live-node guard.**
- **Do NOT remove `materializeThreadReferences` or move it after the early return.**
- **Do NOT start, restart, or deploy to the hub.** The worker is deliberately stopped.
- **Do NOT run the sweep.**
- Do not commit or push.

## Verification — run these and paste the real output

```bash
cd /Users/neocapitelo/go/src/github.com/agent-mem
go build ./...
go vet ./internal/graph/handlers/
go test ./internal/graph/handlers/ -count=1
grep -rn "enqueueSummarizeThread(" internal/ --include='*.go' | grep -v "func enqueue"
```

The last command must show all 9 call sites (8 pre-existing plus the sweep) with an
explicit boolean argument.

**Database warning.** Tests here skip unless `DATABASE_URL` is set. Leave it unset — they
truncate graph tables and the hub is live production. Do not connect to the production
database.

## Tests

Unit tests only, no database, no fakes:

- `SkipJudging` survives a JSON round-trip on all three payload structs (marshal then
  unmarshal, field still `true`) — this is the thing that silently breaks if a `json:` tag
  is misspelled, and a misspelled tag means the flag is lost and the corpus gets judged at
  full cost.
- `backfillStaleSummariesDefaultLimit == 100`, and `resolveStaleSummariesLimit` still
  rejects 501 and still defaults on 0.

Keep the existing tests in `summarize_thread_sig_test.go` and `channels_test.go` passing.

## Acceptance criteria

1. `SkipJudging` present on `linkTopicsPayload`, `indexArtifactPayload` and
   `summarizeThreadPayload`, with matching `json:` tags, and threaded end to end:
   sweep → `summarize_thread` → `index_artifact` → `link_topics`.
2. `link_topics` still runs `materializeThreadReferences` when `SkipJudging` is set, then
   returns before any shortlist or `confirmTopicLink` call.
3. `BackfillStaleThreadSummaries` sets it unconditionally; no request field added.
4. All 8 pre-existing `enqueueSummarizeThread` callers pass `false` explicitly.
5. Default cap is 100; ceiling 500 and the 400 path unchanged; the stale comment about the
   ~15-judge cascade is corrected.
6. Nothing in the diff changes the model, prompt, or gateway config.
7. `go build ./...` and `go vet` clean; full package tests pass with DB tests skipping.
8. No TODO placeholders, no `t.Skip` added, no always-passing assertions.

## Known debt — for context, do not act on it

After this lands, roughly 790 threads with no recent activity keep `SAME_TOPIC` verdicts
that were judged from truncated summaries. The neighbors popup will not fix them: it only
queues pairs with **no** judgment row (`neighbors.go:309-318`), so an existing
`same_topic=f` stays refused. Recovery is a future deliberate capped judging pass, or a new
message in the thread. That is tracked separately and is explicitly out of scope here.

## Report back

The diff, the verbatim output of all four commands, and any judgement calls you made. Do
not commit or push; the conductor reviews, ships and runs the backfill.
