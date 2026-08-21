# Meter and cap every LLM call at the gateway client

Worker brief. Self-contained: assume no knowledge of the conversation that produced it.

Issues: `agent-mem-0x7` (P1, the cap) and `agent-mem-5k0` (P0, the attribution).
Run `bd show agent-mem-0x7` and `bd show agent-mem-5k0` for the recorded evidence.
Repo: `/Users/neocapitelo/go/src/github.com/agent-mem`, branch `main`, clean.

## Why this exists

On 2026-08-18/19 agent-mem drained Enzo's entire Anthropic subscription quota twice.
Measured from llm-gateway access logs: **600-850 `/generate` calls per hour, sustained**,
across two instances (~1,370/hour combined). Both are now paused and the burn is 0.

**The path responsible was never identified.** Four hypotheses were tested against
production and disproven — see `docs/ai/quota-burn-containment.md` for the record. The
reason it could not be found is the point of this task: nothing logs *which code path*
makes a call, and nothing stops a runaway.

`internal/llmgateway/client.go`'s own package doc already claims the property it lacks:

> every call — whichever provider ultimately serves it — passes one place that can
> meter, alert and fail over.

It cannot meter. There is no counter, no ceiling, no attribution. This task builds that.

## Goal

Every LLM call is attributable to a named code path, counted, and refused past a
configurable hourly ceiling.

## The change — all in `internal/llmgateway/client.go` unless noted

### 1. Attribute each call to its caller — no call-site churn

`generate` (the shared body of `Generate`/`GenerateCheap`, ~line 197) and `Embed` must
record which code path called them.

Derive it with `runtime.Caller` / `runtime.CallersFrames`, walking out of this package
to the first frame whose package is not `llmgateway`. Return something short and
greppable — `handlers.summarizeThreadHandler`, `handlers.judgeTopic`, `worker.processObservation`.

**Do NOT add a `purpose string` parameter to the call sites.** There are ~10 of them, an
explicit parameter is churn for no gain, and a caller who passes the wrong string is
worse than no string. Runtime attribution cannot drift out of date.

Log one line per call at **info**: caller, tier, and elapsed ms. At the observed peak
this is ~850 lines/hour, which is fine and is the whole point — `docker logs` must be
able to answer "who is calling" without a code change.

### 2. Count calls in a rolling hour window

An in-process counter keyed by the current clock hour. On a new hour, reset.

`ponytail:` in-memory and per-process by design. A restart resets the window; that is
acceptable because the failure mode being defended against is a runaway loop over
minutes-to-hours, not an adversary gaming the counter. Do not add a table for this.

Expose the current count and the ceiling through a small accessor so a later dashboard
task can read it without touching internals.

### 3. Refuse past a ceiling

New setting `llm_hourly_call_cap` (int, **default 0 = unlimited**) in
`internal/config/config.go`. Follow the exact pattern of an existing int setting —
add it to the struct, the `json:` tag, the settings map (~line 155), the string parser
(~line 201) and the partial `Update` switch (~line 318) so it can be changed live
through `PUT /api/settings`. Read `internal/config/config.go` first and match it.

Default 0 so deploying this changes no behaviour until someone sets a number.

When the hour's count is at or above a non-zero cap, `generate` returns an error
**without making the HTTP request**. The error must:

- wrap or match `ErrUnreachable`, **or** otherwise satisfy `worker.isTransientLLMError`
- contain the string `hourly cap` so it is obvious in a log

Read `worker.isTransientLLMError` and confirm which it is before choosing. This matters:
handlers treat transient LLM failure as "leave the cache unadvanced and retry later"
(`summarize_thread.go:168`), and flat memory's `RequeueRetryablePendingMessage`
(`internal/database/pending.go:96`) *decrements* the attempt counter on a retryable
error, so hitting the cap must not burn a job's retry budget. Getting this wrong turns
a cap into data loss.

Log at **warn** the first time a cap is hit in a given hour; do not log every refusal.

### 4. Does the cap cover embeddings?

`Embed` goes to OpenRouter (paid credits), not the Claude subscription. Count and
attribute embeddings, but **do not** apply the same ceiling — that would couple two
unrelated budgets. Say what you chose in your report.

## Non-goals — do NOT do these

- **Do NOT change the summarizer model, any prompt, or the gateway service config.**
- **Do NOT add a dashboard UI.** The project convention is that settings are
  GUI-editable, and that is a real follow-up, but it needs a `dashboard/` build plus the
  embed refresh and is out of scope here. `PUT /api/settings` is the interface for now.
- **Do NOT try to fix the burn itself.** Do not touch `channels.go`, `summarize_thread.go`,
  `cluster_summary.go` or `detect_hot_topics.go`. Finding the path is what the
  attribution is *for*; changing suspects before the evidence exists is how the previous
  four wrong hypotheses happened.
- **Do NOT prune or modify `graph.jobs`** (`agent-mem-rik` is a separate task).
- **Do NOT start, unpause, or deploy to either instance.** Both are deliberately paused.
- Do not commit or push.

## Tests

Unit only. No database, no network, no fakes of the gateway.

- Counter rolls over on an hour boundary (inject the clock — do **not** call
  `time.Now()` inside the test's assertions).
- `cap == 0` never refuses, whatever the count.
- Count at cap refuses, and the returned error satisfies the transient-error predicate
  the handlers actually use, **and** contains `hourly cap`.
- Refusal happens without an HTTP request (a client pointed at a URL that would fail
  loudly still returns the cap error).
- Caller attribution returns a non-empty name that does not contain `llmgateway` when
  invoked through an exported method from another package.

## Verification — run these and paste the real output

```bash
cd /Users/neocapitelo/go/src/github.com/agent-mem
go build ./...
go vet ./internal/llmgateway/ ./internal/config/
go test ./internal/llmgateway/ ./internal/config/ -count=1
grep -rn "purpose string" internal/llmgateway/   # must be EMPTY — no call-site churn
```

**Database warning.** Leave `DATABASE_URL` unset. Tests elsewhere in this repo truncate
graph tables and both databases are live. Do not connect to either.

## Acceptance criteria

1. Every `generate` and `Embed` call logs caller + tier at info, one line per call.
2. Caller attribution is derived at runtime; **no call site gained a parameter**
   (the `grep` above returns nothing).
3. `llm_hourly_call_cap` exists, defaults to 0 = unlimited, and is changeable live via
   `PUT /api/settings` following the existing int-setting pattern.
4. At a non-zero cap, calls are refused with no HTTP request, the error is classified
   transient by the same predicate the handlers use, and it contains `hourly cap`.
5. Embeddings are counted and attributed but not capped, with the reasoning stated.
6. Nothing outside `internal/llmgateway/` and `internal/config/` changes, except tests.
7. `go build ./...` and `go vet` clean; the two package test runs pass.
8. No TODO placeholders, no `t.Skip` added, no always-passing assertions.

## Known limitation to state, not solve

The cap is **per instance**, and there are two instances (hub + laptop) sharing one
OAuth token. A cap of N therefore permits 2N total. Note it in your report; the decision
about whether the laptop should run a worker at all is `agent-mem-l3o`.

## Report back

The diff, the verbatim output of all four commands, your choice on the embedding
ceiling, and which transient-error predicate you matched and why. Do not commit or push;
the conductor reviews and ships.
