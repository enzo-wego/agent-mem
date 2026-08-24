# Plan — stop the llm-gateway 500s: catch the cancelled timeout, then find the slow calls

Written 2026-08-24. **Self-contained: assume no memory of the conversation that produced it.**

**Target repo is `~/go/src/github.com/llm-gateway`** (Python/FastAPI), not agent-mem. Deployed
on the hub as container `llm-gateway-llm-gateway-1`, bound to `127.0.0.1:8750`.
`ssh enzo@payments`. **Do not touch the laptop instance.**

## Measured evidence

Over 8 hours (2026-08-23 15:00–23:00 UTC): **1,547 `/generate` + 553 `/embed` calls, of which
79 `/generate` returned HTTP 500** — ~5%, spread evenly across every hour, not one incident.

All 59 tracebacks in a 3-hour sample die on the identical path:

```
/app/app/main.py    in generate
/app/app/claude.py  in generate
/app/app/claude.py  in _run
/app/app/claude.py  in _guarded     <- all of them
```

Exceptions reaching the ASGI layer: `asyncio.CancelledError: deadline exceeded` (~14),
`TimeoutError` (12), `ProcessError: Command failed with exit code -9` (2).

Not memory: the gateway sits at 519MB of its 2GB limit (25%). The `exit -9` is a consequence —
cancelling `_run` SIGKILLs the Claude CLI subprocess.

## Defect 1 — the timeout guard leaks `CancelledError`

`app/claude.py`:

```python
async def _guarded(prompt, options):
    """_run with a wall-clock cap — the SDK spawns a subprocess that can hang."""
    try:
        return await asyncio.wait_for(_run(prompt, options), timeout=config.CLAUDE_TIMEOUT_S)
    except TimeoutError as e:
        raise ClaudeError(f"timed out after {config.CLAUDE_TIMEOUT_S}s") from e
```

`asyncio.CancelledError` inherits from `BaseException`, **not** `Exception`, so
`except TimeoutError` never catches it. When `wait_for` cancels on its deadline and the
cancellation surfaces as `CancelledError`, it escapes uncaught and FastAPI renders a bare 500
instead of the intended `ClaudeError` that `main.py` would classify properly.

Fix: also catch `asyncio.CancelledError` and convert it to the same `ClaudeError`. Re-raise
genuine external cancellation (client disconnect, shutdown) rather than swallowing it — only a
deadline-driven cancellation of *our own* `wait_for` should become `ClaudeError`.

`CLAUDE_TIMEOUT_S` defaults to 180 and is unset in the hub environment, so 180s is live.

### This matters beyond tidiness

A 500 is retryable in agent-mem (`internal/llmgateway/client.go`, `StatusError.Retryable()`
includes `500`), so these retry — but retries are bounded by `max_attempts = 5`. **12
`link_topics` jobs have already failed permanently** with `/generate returned 500` in
`last_error`. Confirmed by query. So the leak converts real work into terminal failures, not
just noise.

## Defect 2 — calls are genuinely exceeding 180s (the actual waste)

Fixing defect 1 changes how the failure is *reported*; it does not stop it. Claude works for a
full 180 seconds, gets killed, and the upstream call is billed for nothing — then retried.
That is ~79 wasted calls per 8 hours.

Investigate, do not guess:

1. **Which tier and caller.** `EFFORT_CHEAP=low` is set; `EFFORT_SUMMARY` is unset and
   defaults to `medium`. Long thread summaries on `claude-sonnet-5` at medium effort are the
   leading hypothesis, but confirm it from the logs before changing anything — record which
   model/effort/tier the timing-out calls actually use.
2. **Log duration on every call.** Add a duration field to the gateway's own log line for
   `/generate` so the distribution is visible, and report the p50/p95/p99 alongside the
   timeout. Right now there is no timing data at all, which is why this is guesswork.
3. **Then decide** between lowering `EFFORT_SUMMARY`, raising `CLAUDE_TIMEOUT_S`, or capping
   input size. Both `EFFORT_SUMMARY` and `CLAUDE_TIMEOUT_S` are already in
   `config.EDITABLE_ENV_KEYS`, so either is a live config change with no redeploy — but make
   it a **separate, human-approved step** once the data says which one is right.

Also worth capturing while in here: `/embed` intermittently fails with
`connection reset by peer` from the worker side. Same container, plausibly the same root
cause. Note it if the timing data explains it; do not chase it speculatively.

## Non-goals

- **Do not change `EFFORT_SUMMARY` or `CLAUDE_TIMEOUT_S` in this round.** Instrument first,
  then propose a value with evidence. Changing effort silently changes summary quality, which
  Enzo judges by eye and has rejected models over.
- **Do not add an OpenCode Go or OpenRouter provider.** Go was evaluated and parked
  (`docs/ai/results-ab-opencode-go.md`).
- Do not change tier backends. All three are `claude`; `FALLBACK_ON_QUOTA=false` stays false.
- Do not touch `OR_MODEL_CHEAP` / `OR_MODEL_SUMMARY` — just corrected and verified as
  `anthropic/claude-haiku-4.5` / `anthropic/claude-sonnet-4.5`.
- Do not retry the 12 already-failed `link_topics` jobs here; that is a separate decision.

## Acceptance criteria

1. A test proves a deadline-exceeded cancellation produces `ClaudeError`, not a bare
   `CancelledError`, and that `/generate` answers with a classified status rather than 500.
2. Genuine external cancellation is still propagated, not converted.
3. `/generate` log lines carry a duration; p50/p95/p99 reported over a real window.
4. A statement of which tier/model/effort the timing-out calls use, from data.
5. Deployed to the hub, and the 500 rate re-measured over at least one hour and compared
   against the ~5% baseline.
6. `FALLBACK_ON_QUOTA` still `false`; all three backends still `claude`; no model values
   changed.

## How to verify

Count 500s directly, before and after:

```bash
ssh enzo@payments 'PATH=/opt/homebrew/bin:$PATH docker logs -t llm-gateway-llm-gateway-1 \
  --since <ISO8601> 2>&1 | grep -E "POST /(generate|embed)" \
  | sed -E "s/^([0-9-]+T[0-9]{2}).*POST (\/[a-z]+).*\" ([0-9]+).*/\1 \2 \3/" \
  | sort | uniq -c'
```

Report the 500 count honestly even if the fix only reclassifies them rather than removing
them — reclassification alone is a real improvement (it stops terminal job failures), and
conflating it with a latency fix would misrepresent the result.
