# Round plan — make a resume safe: stop the cap eating jobs, stop not_authed grinding, sharpen attribution

Written 2026-08-22. **Self-contained: assume the reader has no memory of the conversation that
produced it.** This file is the worker's brief.

Read first: `docs/ai/round-deploy-cap-and-find-burner.md`, specifically its `## OUTCOME` section.
That round deployed an hourly LLM cap to the production hub, resumed the hub for 9 minutes, and
**permanently failed 1,002 jobs**. This round fixes the three things that made that resume unsafe
or uninformative. Nothing here is speculative — every claim below was measured on production.

Issues: `agent-mem-0d7` (P0), `agent-mem-7z3` (P0), `agent-mem-fr6` (P1), `agent-mem-6v1` (P1,
partial). Run `bd show <id>` for each.

## Goal

A resume of the hub can run for an hour without destroying queued jobs, and its log can name
which handler is spending the quota.

## Non-goals — do NOT do these

- **Do NOT resume the hub, unpause anything, or change `processing_paused`.** It is `true` and
  stays `true` for this whole round. Deploying is a separate, later, human-gated step.
- **Do NOT change any model, provider or tier.** Settled: summary `claude-sonnet-5`, cheap
  `claude-haiku-4-5`, embeddings `google/gemini-embedding-001` on OpenRouter.
- **Do NOT convert the 30 `%v`-severing error wraps** (`fmt.Errorf("%w: ...: %v", jobs.ErrTransient, err)`).
  That is `agent-mem-naj` and it is deliberately out of scope — see "Why not a sentinel" below.
  Fix B touches exactly one of them, named explicitly.
- **Do NOT rebuild any data**, re-embed, or bump any signature.
- **Do NOT touch the dashboard front-end** (`dashboard/`). Fix D is Go-only. The GUI half of
  `agent-mem-6v1` stays open for its own round, because a front-end build plus the
  `internal/worker/dashboard/` embed copy does not belong in a P0 correctness change.
- Do not run handler integration tests against a real database. Leave `DATABASE_URL` unset. Some
  test helpers in this repo truncate graph tables (`agent-mem-7bs`).

---

## Fix A — `agent-mem-0d7` (P0): a bound cap must not consume the retry budget

### The defect, measured

`graph.jobs` claiming runs `attempts = attempts + 1` **on claim**, before the handler runs
(`internal/graph/jobs/queue.go:115`). The cap refusal happens inside the handler. So every refusal
burns one attempt even though no work was attempted and no LLM call was made. Observed live within
12 minutes of the cap binding:

```
transient: link_topics confirm: llm-gateway unreachable: hourly cap of 300 generate
calls reached (caller=handlers.(*GeminiAdapter).GenerateCheap tier=cheap)
attempt=3 delay=109537
```

`max_attempts` is 5. A cap that binds for an hour walks every affected job to 5 and fails it
permanently. The cap was built to protect the quota and destroys the queue instead.

### Why not a sentinel (read this before choosing an approach)

The obvious fix is an `errors.Is` sentinel checked in the worker. It is not reliable here: 30 sites
in `internal/graph/handlers/` wrap errors as `fmt.Errorf("%w: ...: %v", jobs.ErrTransient, err)`.
The `%v` severs the chain, so a sentinel from `llmgateway` frequently does not survive to
`worker.go`. `link_topics.go:725` — the exact site observed above — is one of them.

So the primary fix must not depend on the error reaching the worker at all.

### The approach: don't claim work that cannot run

1. **Add `UsesLLM bool` to the handler registry entry** (the struct behind `d.entry` in
   `internal/graph/jobs/worker.go`; it already carries per-entry fields). Set it `true` at the
   registration site of every handler that calls the LLM: `summarize_thread`, `cluster_summary`,
   `detect_hot_topics`, `link_topics`, `describe_attachment`, `derive_feature_entity`,
   `refresh_scope`, `derive_person_roles`, `index_artifact`. Verify that list by grepping for
   `GenerateCheap`, `.Generate(`, `.Describe(`, `.Embed` in `internal/graph/handlers/` rather than
   trusting it — and say in your report if it differs.

   Prefer a field on the registry entry over a separate job-type→bool map: the map would silently
   drift when a handler starts calling the LLM.

2. **Give the jobs dispatcher a cap predicate.** Add a `CapReached func() bool` to the dispatcher
   config (nil means "never capped", so existing callers and tests are unaffected). Wire it in
   `internal/worker/` to the gateway client — `llmgateway.Client.CallCount()` already returns
   `(gen, embed, cap)`, so the predicate is `gen >= cap && cap > 0`.

   Do **not** import `internal/llmgateway` into `internal/graph/jobs`. The predicate is a plain
   func value; that keeps the dependency pointing one way.

3. **Skip dispatch while capped.** When `CapReached()` is true, the dispatcher must not claim jobs
   whose entry has `UsesLLM`. It leaves them queued — no claim, so no attempt consumed — and keeps
   claiming everything else (`fetch_body`, `resolve_identity`, and other non-LLM work must keep
   flowing). Log this once per cap window, not per skipped job.

4. **Safety net for the race.** The counter can cross the cap between the predicate and the call,
   so a few refusals will still reach the handler. Export a sentinel from `internal/llmgateway`
   (`ErrCapped`) that the cap refusal wraps **in addition to** `ErrUnreachable`, so existing
   retryability is unchanged. In `worker.go`, when `errors.Is(err, llmgateway.ErrCapped)` holds,
   refund the attempt instead of counting it: add `jobs.RetryRefund` (or a bool parameter on
   `Retry`) that additionally sets `attempts = GREATEST(attempts - 1, 0)`.

   This mirrors the pattern the flat path already uses for exactly this situation —
   `RequeueRetryablePendingMessage`, `internal/database/pending.go:96`, whose comment reads
   *"Infrastructure failures are not caused by the message and therefore must not consume its
   retry budget."* Follow that precedent, including the reasoning in the comment.

### Acceptance for A

- A dispatcher test proves that with `CapReached()` returning true, a `UsesLLM` job is **not**
  claimed (its `attempts` is unchanged) while a non-LLM job in the same queue **is** claimed.
- A test proves a handler error satisfying `llmgateway.ErrCapped` refunds the attempt, and that a
  job at `attempts == max_attempts` hitting the cap is **requeued, not failed**.
- A test proves `errors.Is(capErr, llmgateway.ErrUnreachable)` is still true, so
  `llmgateway.IsRetryable` behaviour does not regress.

---

## Fix B — `agent-mem-7z3` (P0): `not_authed` is permanent, not transient

### The defect, measured

`internal/graph/handlers/fetch_body.go:58` wraps **every** fetcher error as transient:

```go
return fmt.Errorf("%w: fetch_body fetch: %v", jobs.ErrTransient, err)
```

The hub worker has no Slack bot token, so `internal/graph/fetchers/slack.go:225` returns
`slack fetcher: API error: not_authed` — a permanent auth failure — and it is retried until the
budget is gone. In a 9-minute window: `graph.jobs` failed went 36,170 → **37,172** (+1,002
permanent), against only +528 done. `fetch_body` now holds 15,138 failed rows, every one with
`last_error` = `transient: ... not_authed`.

### The approach

1. **Classify Slack API errors at the fetcher**, in `internal/graph/fetchers/slack.go` around
   line 225, where the API error string is already in hand. Permanent (never retry):
   `not_authed`, `invalid_auth`, `account_inactive`, `token_revoked`, `token_expired`,
   `missing_scope`, `channel_not_found`, `thread_not_found`, `message_not_found`. Transient:
   `ratelimited`, `internal_error`, `service_unavailable`, `fatal_error`, plus anything unknown —
   **default to transient for unrecognised codes**, so a new Slack error code degrades to a retry
   rather than to silent permanent loss.

   `internal/graph/fetchers/` must not import `internal/graph/jobs` if that creates a cycle —
   check. If it does, return a typed error from the fetcher (e.g. an exported
   `fetchers.PermanentError`) and map it to `jobs.ErrFatal` in `fetch_body.go`.

2. **Preserve the chain at the one call site this needs:** change `fetch_body.go:58` to select
   `jobs.ErrFatal` vs `jobs.ErrTransient` from the classification, and wrap the cause with `%w`
   not `%v`. This is the single `%v` site in scope; leave the other 29 alone.

3. **Do not** attempt to supply the Slack token. It is env-only
   (`AGENT_MEM_SLACK_BOT_TOKEN`, `internal/config/config.go:519`) and not a DB-backed runtime
   setting, so restoring it is a deployment action the human performs. Note in your report that
   this fix makes the missing token **fail fast and visibly** instead of grinding the queue — it
   does not make Slack fetching work.

### Acceptance for B

- A table test over the classifier: `not_authed` → fatal, `ratelimited` → transient, unknown code
  → transient.
- A test proves a `not_authed` fetch fails the job on the **first** attempt rather than retrying.
- `grep -c '%v", jobs.ErrTransient' internal/graph/handlers/` drops by exactly 1 (from 30 to 29).

---

## Fix C — `agent-mem-fr6` (P1): attribution must name the handler, not the shim

### The defect, measured

`callerName()` (`internal/llmgateway/client.go:213`) returns the first frame outside
`internal/llmgateway`, which for every graph job is the shared `GeminiAdapter` shim
(`internal/graph/handlers/gemini_adapter.go:31`). The complete observed caller set on production:

```
177  handlers.(*GeminiAdapter).GenerateCheap [generate]
 60  handlers.(*GeminiAdapter).Generate      [generate]
 39  worker.(*Server).processObservation     [generate]
 29  worker.(*Server).processObservation     [embed]
 27  handlers.(*GeminiAdapter).EmbedWithOptions [embed]
  4  worker.(*Server).processSummary         [generate]
```

The flat-memory paths are correct. Every graph handler collapses into two adapter names, so the
round that shipped this could not tell `detect_hot_topics` from `cluster_summary` from
`link_topics` — which was the entire question it was deployed to answer.

Second defect, same feature: the field is logged as `Str("caller", ...)`. `caller` is a
**reserved zerolog field name**, so ConsoleWriter renders it as a message prefix (`name > message`)
instead of `caller=...`. Every `grep "caller="` against the console log finds nothing.

### The approach

1. In `callerName()`, keep walking when the frame belongs to the adapter shim, so the returned
   frame is the real caller. Match on the receiver (`(*GeminiAdapter)`) rather than on a file path.
   Leave a comment saying why the frame is skipped — a future reader will otherwise "fix" it back.
2. Rename the log field from `caller` to `llm_caller` at all three log sites
   (`client.go:329`, `:396`, `:431`). Keep the word `caller=` inside the cap-refusal error
   *message* as it is — that text is load-bearing for the runbook and is not a zerolog field.

### Acceptance for C

- A test that calls the client through a wrapper method emulating the adapter and asserts the
  attributed name is the **outer** function, not the wrapper.
- The existing tests in `internal/llmgateway/caller_attribution_test.go` still pass (update them
  for the field rename where they assert on it).
- `grep -rn 'Str("caller"' internal/llmgateway/` returns nothing.

---

## Fix D — `agent-mem-6v1` (P1, partial): make the cap readable through the API

`GET /api/settings` returns the hand-written `settingsResponse` struct
(`internal/worker/settings_handlers.go:19`), which has no field for `llm_hourly_call_cap`. The cap
is settable by `PUT` and persists correctly, but cannot be read back — during the last round it
could only be confirmed by querying postgres directly.

Add `LLMHourlyCallCap int` and `ProcessingPaused bool` to `settingsResponse` and populate both in
`handleGetSettings` from the snapshot. Two fields, two assignments. No front-end work.

### Acceptance for D

- A handler test asserts `GET /api/settings` includes both keys with the configured values.

---

## Quality gates — run these yourself and paste the output

```bash
go build ./...
go vet ./...
go test ./internal/llmgateway/... ./internal/config/... ./internal/worker/... \
        ./internal/graph/jobs/... ./internal/graph/handlers/... ./internal/graph/fetchers/...
```

`DATABASE_URL` must be unset. Tests requiring a database should skip, not fail — if one tries to
connect to a real database, stop and report it instead of pointing it at one.

## Definition of done

1. `go build`, `go vet`, and the test packages above are clean, with output pasted.
2. Every acceptance bullet in A, B, C and D has a named test that actually asserts it. A test that
   cannot fail is not evidence.
3. No TODO placeholders, no `t.Skip` added, no stub tests, no commented-out assertions.
4. No pre-existing test deleted or weakened. If one legitimately must change, say which and why.
5. `processing_paused` is untouched and nothing was deployed.

## Report back

The diff summary per fix, the pasted gate output, the verified `UsesLLM` handler list (and whether
it differed from the list above), and anything you found that the plan got wrong. If a fix turns
out to be wrong or impossible as specified, stop and say so rather than inventing a substitute.
