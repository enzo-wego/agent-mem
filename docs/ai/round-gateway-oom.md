# Plan — stop the llm-gateway OOM-killing its own Claude subprocesses

Written 2026-08-24. **Self-contained: assume no memory of the conversation that produced it.**

**Target repo is `~/go/src/github.com/llm-gateway`** (Python/FastAPI), not agent-mem.

**The live checkout on the hub is `/Users/enzo/src/github.com/llm-gateway`** — note there is NO
`go/` in that path. A second, non-live checkout exists at `~/go/src/github.com/llm-gateway` and
pulling into it deploys nothing. Confirm which one is live from the container itself:
`docker inspect llm-gateway-llm-gateway-1 --format '{{index .Config.Labels "com.docker.compose.project.config_files"}}'`.

Deploy needs BOTH compose files or it fails on a missing `config/.env`:

```bash
ssh enzo@payments 'cd ~/src/github.com/llm-gateway && git pull --ff-only \
  && PATH=/opt/homebrew/bin:$PATH docker compose -f docker-compose.yml -f docker-compose.vps.yml up -d --build'
```

bd issue: `agent-mem-c25y` (P0). Predecessor round: `docs/ai/round-gateway-timeout-500s.md`,
shipped as llm-gateway `431f43a`.

## What the previous round established, so you do not re-derive it

The previous round fixed a real bug: `asyncio.CancelledError` escaping `_guarded` as a bare 500.
That fix works and `CancelledError` no longer appears in the logs. It also added a per-outcome log
line to `/generate` carrying `tier`, `backend`, `model`, `effort`, `status`, `reason` and
`duration_ms`. That instrumentation is what produced everything below — do not remove it.

It did **not** stop the 500s, and the data it produced disproved the hypothesis the round was
built on. Both of these are measured, not assumed:

1. **The failures are not timeouts.** Failing calls have p50=1,070ms and p95=3,684ms. Successful
   calls have p50=4,402ms. The failures are *faster* than the successes. Nothing is reaching the
   180s `CLAUDE_TIMEOUT_S` deadline.
2. **The failures are `ProcessError`, not `ClaudeError`.** The exception is
   `claude_agent_sdk._errors.ProcessError: Command failed with exit code -9`. `-9` is SIGKILL.
   `ProcessError` is not a subclass of `ClaudeError`, so it escapes `except claude.ClaudeError`
   in `main.py`'s `generate()` and falls to `except Exception`, producing a bare 500 with
   `reason=unexpected_error`.
3. **The container is being OOM-killed.**
   `docker inspect llm-gateway-llm-gateway-1 --format '{{.State.OOMKilled}}'` returns `true`.
4. **There is no concurrency limit anywhere.** `app/claude.py` has no semaphore, no worker pool,
   no queue. Every `/generate` calls `sdk.query(...)`, which spawns a Claude CLI subprocess.
   `docker-compose.yml` sets `mem_limit: 2g`.
5. **The failures arrive in bursts, not steadily.** Per 5-minute bucket after the deploy:

   | window (UTC) | ok | fail |
   |---|---|---|
   | 01:50 | 4 | 0 |
   | 01:55 | 6 | 0 |
   | 02:00 | 88 | 64 |
   | 02:05 | 78 | 56 |
   | 02:15 | 5 | 0 |

   Zero failures outside the 02:00–02:10 burst. In that window 53 of the generate calls came
   from `handlers.confirmTopicLink` — the `link_topics` backlog draining.
6. **Instantaneous memory readings are worthless here.** At rest the container sits at 90MB of
   2GB. The previous plan read 519MB/2GB and concluded "not memory". That is exactly the wrong
   inference: the spike only exists under concurrency, and a gauge read between bursts cannot
   see it. Use `OOMKilled` and per-subprocess RSS, never a point-in-time total.

## The causal chain

Concurrent `/generate` calls → N unbounded Claude CLI subprocesses → combined RSS exceeds the 2GB
cgroup limit → kernel SIGKILLs a child → SDK raises `ProcessError: exit -9` at ~1s → escapes as a
bare 500 → agent-mem retries (500 is retryable, `internal/llmgateway/client.go`) → bounded by
`max_attempts = 5` → jobs eventually fail terminally.

## Defect A — `ProcessError` escapes unclassified (`agent-mem-rdxq`, P1)

Cheap to fix and worth doing regardless of the memory work: it converts a bare 500 into a
classified 502 and stops terminal job failures.

Classify at the `claude.py` boundary rather than enumerating SDK exception types in `main.py`.
Catch the SDK's error base class and convert to `ClaudeError` so `main.py` answers 502. Keep the
existing behaviour that genuine external cancellation still propagates.

Make `reason=` in the log line name the SDK failure rather than `unexpected_error`, so the next
person reading the logs can tell an OOM kill from a real internal error.

**This is mitigation, not a fix.** It stops the damage pattern; the subprocess is still killed and
the upstream call is still wasted. Say so plainly in the report — do not present a reclassified
502 as a solved OOM.

## Defect B — unbounded subprocess spawning (the actual fix)

**Measure before you choose a bound. Do not guess a semaphore size.**

1. **Measure per-subprocess RSS under load.** Find the real resident size of one Claude CLI
   subprocess, and how it scales with a few running at once. `docker stats` on the container plus
   per-process accounting inside it (`ps` in the container, or cgroup `memory.current`). Record
   the number.
2. **Derive the bound from that number and `mem_limit`,** leaving headroom for the Python process
   itself. Show the arithmetic in the report.
3. **Then implement** an `asyncio.Semaphore` around the `sdk.query` call site so at most N
   subprocesses are resident. Calls above N wait rather than spawning.
4. Decide, with the measured number in hand, whether `mem_limit: 2g` should also rise. Raising the
   limit without bounding concurrency only moves the cliff — if you propose a new limit, say what
   concurrency it supports.

Queueing changes latency under burst: a bounded gateway makes callers wait instead of failing
fast. That is the correct trade (a waiting call succeeds; a killed call is retried and may die at
`max_attempts`), but measure the added wait and report it rather than leaving it implicit.

## Non-goals

- Do not change `EFFORT_SUMMARY`, `EFFORT_CHEAP` or `CLAUDE_TIMEOUT_S`. The timeout is not
  involved — that was the disproven hypothesis.
- Do not change tier backends, `OR_MODEL_CHEAP`, `OR_MODEL_SUMMARY`, or `FALLBACK_ON_QUOTA`.
  All three backends are `claude` and `FALLBACK_ON_QUOTA=false`. Verified 2026-08-23 and again
  after the last deploy.
- Do not add an OpenCode Go or OpenRouter provider. Go was evaluated and parked
  (`docs/ai/results-ab-opencode-go.md`).
- Do not remove or weaken the `/generate` duration instrumentation.
- Do not pace the agent-mem side in this round. Throttling `link_topics` would treat one caller's
  symptom and leave the gateway fragile for every other caller. It stays a separate option.
- Do not retry the already-terminally-failed `link_topics` jobs here.

## Acceptance criteria

1. Per-subprocess RSS measured and stated as a number, with the method used.
2. A concurrency bound derived from that number with the arithmetic shown, not guessed.
3. `ProcessError` produces a classified 502, covered by a test.
4. A test covers the concurrency bound — that the (N+1)th concurrent call waits rather than
   spawning.
5. `OOMKilled` stays `false` across a burst of the same shape as the 02:00–02:10 one.
6. The 500 rate re-measured across at least one burst and compared against both baselines: the
   ~5% long-run rate and the 40% burst rate.
7. Added wait time under burst reported, from the `duration_ms` field.
8. `FALLBACK_ON_QUOTA` still `false`, all three backends still `claude`, no model values changed.

## How to verify

The burst is the test case; a quiet window proves nothing. Either wait for a natural burst (the
`link_topics` drain produces them) or generate concurrent load deliberately.

```bash
# outcome mix and reasons, from the instrumented line
ssh enzo@payments 'PATH=/opt/homebrew/bin:$PATH docker logs llm-gateway-llm-gateway-1 --since 60m 2>&1' \
  | perl -pe 's/\e\[[0-9;]*m//g' | grep 'llm-gateway generate tier=' \
  | grep -oE 'status=[a-z0-9]+ reason=[a-z_]+' | sort | uniq -c | sort -rn

# was anything OOM-killed
ssh enzo@payments 'PATH=/opt/homebrew/bin:$PATH docker inspect llm-gateway-llm-gateway-1 \
  --format "OOMKilled={{.State.OOMKilled}}"'
```

Note the worker log strips to plain text only after removing ANSI escapes — `tier=` and
`llm_caller=` are wrapped in colour codes, so a naive `grep 'tier=[a-z]+'` silently matches
nothing. That cost time once already.

Report the numbers honestly even if the fix only partly works. A bound that reduces but does not
eliminate the failures is a real result; presenting it as elimination is not.
