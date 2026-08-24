# Result — the llm-gateway 500s were an OOM, not a timeout

Written 2026-08-24. Closes `agent-mem-c25y` (P0) and `agent-mem-rdxq` (P1).

## Outcome

Fixed and proven in production. llm-gateway `fa20422` (PR #9), deployed to the hub.

| burst (5 min) | calls | failures | OOMKilled | p50 | p95 |
|---|---|---|---|---|---|
| **before the fix** | 152 | **64 (42%)** | **true** | 4,402 ms | 8,654 ms |
| after, burst 1 | **233** | 0 | false | 12,255 ms | 20,943 ms |
| after, burst 2 | 173 | 0 | false | — | 16,001 ms |
| after, burst 3 | 108 | 0 | false | — | 17,313 ms |

The first post-fix burst was **larger** than the one that originally produced 64 failures. 514 calls
across three bursts, zero failures, zero tracebacks, zero `ProcessError`, zero timeouts.

## The fix

Concurrency bounded at **4** concurrent Claude CLI subprocesses via `asyncio.Semaphore`, derived
from measurement rather than guessed: the bundled CLI peaks at **319.7 MiB RSS**, so
4 × 319.7 + 86.3 MiB service baseline = 1,365.1 MiB of the 2 GiB cgroup, leaving 682.9 MiB.

`sdk.query()` was replaced with an explicit `ClaudeSDKClient` connect / receive / disconnect
lifecycle, so the subprocess is cleaned up **before** the semaphore slot is released. Releasing a
slot while a subprocess lingers would defeat the bound.

`ProcessError` now converts to `ClaudeError` (it subclasses `sdk.ClaudeSDKError`, caught in
`_guarded`), so `main.py` answers a classified 502 rather than a bare 500.

## Why this took three rounds — worth reading before the next one

Two hypotheses were wrong, and both were wrong in the same way: **a single point-in-time
measurement was treated as evidence about behaviour under load.**

1. **"It's the 180s timeout."** The round-1 plan was built on this and was about to lower
   `EFFORT_SUMMARY`. Its own new instrumentation disproved it immediately: failing calls had
   p50 = 1,070 ms against p50 = 4,402 ms for successes. **The failures were faster than the
   successes.** Nothing was near the deadline.
2. **"It's not memory."** The round-1 plan ruled memory out by reading 519 MB of a 2 GB limit.
   At rest the container sits at 90 MB. The spike only exists during a burst, so no gauge read
   between bursts could ever have seen it. What actually found it was
   `docker inspect --format '{{.State.OOMKilled}}'` returning `true`.

The lesson that generalises: for a load-dependent failure, measure the **failure population**
(its latency distribution, its exception type, its arrival pattern) and use cumulative flags like
`OOMKilled`. An instantaneous total is close to worthless.

The arrival pattern was the other decisive clue. The failures were not spread across the window —
they were one 10-minute burst with **zero failures either side**, coinciding with the
`link_topics` backlog draining. A steady 5% average concealed a bursty 42%.

## Known trade-off, still open as a future risk

The semaphore is acquired **inside** the `asyncio.wait_for`, so queue wait counts against
`CLAUDE_TIMEOUT_S` (180s). At the observed ceiling of ~2,800 calls/hour this did not bite —
worst observed call was 27.4s. Above that, queued calls will begin timing out instead of being
OOM-killed. That is the better failure (a waiting call can still succeed; a killed one is retried
and may die at `max_attempts = 5`), but it is a trade rather than an elimination.

If bursts grow substantially, the fix is to acquire the slot **outside** the `wait_for` so the
deadline covers execution only, or to raise the bound after re-measuring RSS.

## Cost paid

Latency roughly tripled at p50 under burst (4.4s → 12.3s) and rather more than doubled at p95
(8.7s → 20.9s). That is the correct trade: before, 42% of burst calls were killed outright and
retried, and 12 `link_topics` jobs had already failed permanently at `max_attempts`.

## Operational notes that cost time today

- **The live llm-gateway checkout on the hub is `/Users/enzo/src/github.com/llm-gateway`** — note
  there is **no `go/`** in that path. A second, non-live checkout exists at
  `/Users/enzo/go/src/github.com/llm-gateway`, and pulling into it deploys nothing. Confirm which
  one is live from the container itself:
  `docker inspect llm-gateway-llm-gateway-1 --format '{{index .Config.Labels "com.docker.compose.project.config_files"}}'`.
  agent-mem is the opposite: its live checkout **is** `/Users/enzo/go/src/github.com/agent-mem`,
  verified the same way against `agent-mem-worker-1`.
- Deploying llm-gateway needs **both** compose files or it fails on a missing `config/.env`:
  `docker compose -f docker-compose.yml -f docker-compose.vps.yml up -d --build`.
- Worker and gateway log fields are wrapped in ANSI colour codes, so `grep 'tier=[a-z]+'` matches
  **nothing**. Strip first: `perl -pe 's/\e\[[0-9;]*m//g'`.
- Verification must span a burst. A quiet window is indistinguishable between a working fix and a
  broken one, because there were zero failures outside the burst.
