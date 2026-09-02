# Plan — wire refresh_slack_channels to the same bot token as every other handler

**Repo:** `/Users/neocapitelo/go/src/github.com/agent-mem`, branch off `main`
**Issue:** `agent-mem-q8tm` (P1)

## Goal

`refresh_slack_channels` has been unable to authenticate since 2026-08-12. Make
it read the token the rest of the codebase reads, so the channel-name refresh
and the `conversations.info` backfill (shipped in `688f9c4`, never yet
executed) can run.

## Root cause — confirmed, not inferred

```go
// internal/graph/handlers/refresh_slack_channels.go:31
token := os.Getenv("SLACK_BOT_TOKEN")
if token == "" {
    return fmt.Errorf("%w: refresh_slack_channels: SLACK_BOT_TOKEN not set", jobs.ErrFatal)
}
```

Every other Slack handler uses `deps.SlackBotToken` (e.g.
`notify_watch_channels.go:110`), which `internal/worker/server.go:164` fills
from `cfg.Graph.SlackBotToken`, itself populated from
`AGENT_MEM_SLACK_BOT_TOKEN` (`internal/config/config.go:518`) or the config
file.

The payments hub container exposes exactly two Slack variables —
`AGENT_MEM_SLACK_BOT_TOKEN` and `AGENT_MEM_SLACK_DM_USER`. There is no
`SLACK_BOT_TOKEN`. Evidence: job `5875471`, enqueued 2026-08-25, failed on
attempt 1 with `fatal: refresh_slack_channels: SLACK_BOT_TOKEN not set`, while
`notify_watch_channels` completed at 05:52:17 using the same credential through
`deps`.

Last successful refresh: `2026-08-12 19:07:15` — the day the hub moved off the
VPS, whose environment did set the unprefixed name.

**The 29 `HTTP 429` failures in the job history are VPS-era leftovers, not the
current blocker.** Do not spend any effort on rate limiting in this change.

## Approach

In `refreshSlackChannelsHandler`, read the token from `deps.SlackBotToken`,
exactly as `notify_watch_channels.go:110` does. **Delete the
`os.Getenv("SLACK_BOT_TOKEN")` read entirely** — do not keep it as a fallback.
The only environment that ever set the unprefixed name was the retired VPS;
a second token source that nothing populates is dead weight that makes the next
"which variable do I set?" question harder, not easier.

Keep the `jobs.ErrFatal` behaviour when the token is empty: a missing
credential is a misconfiguration, and failing loudly is deliberate (see
`agent-mem-egsf`, where reporting `done` on a missing credential hid a 10-day
outage).

Update the error message to name `AGENT_MEM_SLACK_BOT_TOKEN`, the variable that
is actually read.

Remove the `os` import if it becomes unused.

## Non-goals

- Do not touch rate limiting, backoff, or `Retry-After`. Unrelated.
- Do not change the backfill logic added in `688f9c4`. It is correct and
  tested; it has simply never run.
- Do not change how any other handler reads its token.
- Do not re-enqueue jobs or touch production. The conductor does that.

## Files expected to change

| File | Change |
|---|---|
| `internal/graph/handlers/refresh_slack_channels.go` | token source + error message |
| `internal/graph/handlers/refresh_slack_channels_test.go` | one test |

## Acceptance criteria

1. With `deps.SlackBotToken` set, the handler runs (does not return
   `ErrFatal`), regardless of whether `SLACK_BOT_TOKEN` is set in the
   environment.
2. With `deps.SlackBotToken` empty, it returns a `jobs.ErrFatal` error naming
   `AGENT_MEM_SLACK_BOT_TOKEN`.
3. `os.Getenv("SLACK_BOT_TOKEN")` no longer appears anywhere in the file.
4. The existing backfill tests still pass unchanged.
5. No other handler is touched.

## Test

Extend `refresh_slack_channels_test.go` with a table covering the two token
cases in criteria 1-2. Use the existing `httptest` Slack stand-in — no live
calls.

**Do not run handler tests against the live dev DB.** Use the `agentmem_test`
scratch DB:
`DATABASE_URL='postgresql://agentmem:agentmem@localhost:5433/agentmem_test'`.

Note: two tests in this package — `TestImportBambooHR_CSVBytes_ParsesAndUpserts`
and `TestIngestURL_AlreadyFresh` — already fail on clean `main` (tracked
separately). Do not try to fix them and do not let them block you; just confirm
your change does not add new failures.

## How to verify

```bash
DATABASE_URL='postgresql://agentmem:agentmem@localhost:5433/agentmem_test' \
  go test ./internal/graph/handlers/ -run 'RefreshSlackChannels' -count=1 -v
go build ./...
```

Paste the real test output. No dashboard or embed rebuild is needed — this
change is Go-only.
