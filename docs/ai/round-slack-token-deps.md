# Plan — every Slack handler reads the token from `deps`, not the unprefixed env var

**Issue:** `agent-mem-khqh` (P1) · **Repo:** `~/go/src/github.com/agent-mem` · **Branch off:** `main`
**Predecessor:** `agent-mem-q8tm` fixed exactly one handler (`refresh_slack_channels`). Seven others were missed.

You have no memory of the conversation that produced this. Everything below was verified
against the tree at `9b66260` and against the running payments hub on 2026-09-02.

---

## The incident this comes from

Ross Veitch (CEO) posted in `#payments` (`CKQ6XGTCZ`) at 2026-09-02T05:46:31Z about Google's
Universal Commerce Protocol, asking for a payments/checkout assessment. Enzo got no alert.

Primary cause was not a code defect: enzobot (`U0AG1DJP9K9`) was not a member of that channel,
Socket Mode only delivers `message.channels` events for channels the bot is in, so the message
never reached `/api/graph/ingest/content`. `graph.nodes` held zero rows for `scope='slack:CKQ6XGTCZ'`
for all time. Enzo invited the bot at ~07:40Z, which fixes future messages.

Recovering that one message needs a backfill. The backfill is broken:

```
job 6378279  backfill_slack_channel  failed  attempt 1
fatal: backfill_slack_channel: SLACK_BOT_TOKEN not set
```

The hub container sets `AGENT_MEM_SLACK_BOT_TOKEN` (valid, 55 chars, `auth.test` returns
`enzobot`). It does not set the unprefixed `SLACK_BOT_TOKEN`. Anything reading the unprefixed
name has been dead since the hub moved off the VPS on 2026-08-12.

## Live damage on the hub (2026-09-02, `graph.jobs`)

| Job type | failed | newest failure | last success |
|---|---|---|---|
| `resolve_identity` | 14096 | 07:49 today | 2026-08-14 |
| `describe_attachment` | 9060 | 06:56 today | 2026-08-18 |
| `backfill_slack_thread` | 139 | 2026-09-01 | — |
| `refresh_slack_bots` | 31 | 2026-08-26 | 2026-08-12 |
| `backfill_slack_channel` | 1 | today | — |

Consequences that matter: Slack attachment OCR is dead (so the hot-topic judge is blind to
screenshot-only reports, undoing `notify-screenshot-reports`), Slack display names stop
resolving (and `importance.json` override matching keys on `lower(display_name)`), and no
channel can be backfilled.

## Goal

Every Slack handler authenticates from `deps.SlackBotToken`, so one credential feeds all of
them and the hub's env var reaches every job.

## Non-goals

- Do **not** touch `notify_watch_channels`, `detect_hot_topics`, or `refresh_slack_channels` —
  they already read `deps.SlackBotToken` and work.
- Do **not** change the hot-topic gate, the relevance judge, the eligibility gate, or the
  continents config. The notification-coverage question is a separate round.
- Do **not** add a new config key, settings row, or dashboard field. The value already exists
  in `cfg.Graph.SlackBotToken` (`internal/config/config.go:518`, wired at
  `internal/worker/server.go:164` and `:470`).
- Do not attempt to retire the 15k historical `fetch_body` failures from July. Out of scope.

## Files expected to change

Seven call sites, all `internal/graph/handlers/`:

| File:line | Shape |
|---|---|
| `backfill_slack.go:45` | inside `func …(deps Deps) jobs.Handler` — swap in `deps.SlackBotToken` |
| `backfill_slack_thread.go:44` | same |
| `refresh_slack_groups.go:28` | same |
| `refresh_slack_bots.go:35` | same |
| `refresh_slack_users.go:32` | same |
| `describe_attachment.go:243` | inside `downloadWithAuth(ctx, url, source)`, a package-level func with no `deps`. Its only caller is `describe_attachment.go:69`, inside a handler that has `deps`. Thread the token in as a parameter. |
| `resolve_identity.go:112` | inside `fetchSlackUserInfo(ctx, userID)`, called from `resolve_identity.go:97`, inside a handler that has `deps`. Thread the token in as a parameter. |

Keep the existing fatal-error wording (`"%w: <job>: SLACK_BOT_TOKEN not set"`) so the
strings the runbook and this plan quote keep matching. Keeping `os.Getenv("SLACK_BOT_TOKEN")`
as a fallback when `deps.SlackBotToken` is empty is fine and matches what `q8tm` did for
`refresh_slack_channels`; do it the same way there, not a new way.

`describe_attachment.go:248-249` also reads `JIRA_TOKEN` / `JIRA_EMAIL` from the environment
while `Deps` carries them. Same class of bug, same file, so fix those two in this pass; do not
go hunting for further instances beyond this file.

## Approach

1. Read `internal/graph/handlers/refresh_slack_channels.go` and copy its accessor pattern
   verbatim. It is the fixed reference implementation.
2. Apply that pattern to the five handler-scoped sites.
3. For the two package-level helpers, add a token parameter and pass `deps.SlackBotToken`
   from the caller. Do not introduce a package-level variable or a `Deps` field on the helper.
4. Extend the existing convention test. `refresh_slack_channels_test.go:276` already asserts
   that handler uses `deps` and not the env var; add the other seven files to that assertion
   (a table over file paths, greping for `os.Getenv("SLACK_BOT_TOKEN")`, is acceptable and is
   the cheapest guard against the next sibling regression).

## Acceptance criteria

- `grep -rn 'os.Getenv("SLACK_BOT_TOKEN")' internal/ cmd/` returns only fallback reads that
  sit behind an empty `deps.SlackBotToken` check, and no hits at all inside a function that
  already has `deps` in scope.
- `go build ./...` and `go test ./internal/graph/handlers/...` pass.
- A test fails if someone reintroduces a bare env read in any of the eight Slack handlers.
- No behaviour change when `deps.SlackBotToken` is set and the env var is absent — that is
  precisely the hub's configuration.

## How to verify (on the hub, after deploy)

Deploy per `CLAUDE.md`: the hub is the payments Mac mini, `make deploy` is broken and targets
the retired VPS.

```bash
ssh enzo@payments 'cd ~/go/src/github.com/agent-mem && git pull --ff-only \
  && PATH=/opt/homebrew/bin:$PATH docker compose up -d --build worker'
```

1. **The binary carries the change** (never trust the build log):
   `docker exec agent-mem-worker-1 grep -c "<a new string literal from the diff>" /usr/local/bin/agent-mem`
2. **Backfill the channel that started this**, which is also the end-to-end proof:
   ```bash
   K=$(docker exec -i agent-mem-postgres-1 psql -U agentmem -d agentmem -X -q -t -A \
        -c "SELECT value FROM settings WHERE key='api_key'")
   curl -s -X POST http://localhost:34567/api/graph/backfill/slack \
     -H "Authorization: Bearer $K" -H 'Content-Type: application/json' \
     -d '{"channel_id":"CKQ6XGTCZ","months":1}'
   ```
   Then assert the job reaches `done`, and that
   `SELECT id, created_at FROM graph.nodes WHERE scope='slack:CKQ6XGTCZ'` returns Ross's
   message with `created_at = 2026-09-02 05:46:31+00` (ingest derives `created_at` from the
   Slack event time — `ingest_content.go:258`), which puts it inside the 24h
   `detect_hot_topics` lookback.
3. **Re-seed the four dead chains.** `refresh_slack_channels` (last success 2026-08-26),
   `refresh_slack_bots` (2026-08-12), `refresh_slack_users` and `refresh_slack_groups`
   (2026-07-28) have no queued rows — the fatal failures ended their self-reschedule chains.
   Enqueue one of each and confirm each completes and re-enqueues its successor.
4. **`resolve_identity` and `describe_attachment` recover.** Requeue a handful of the failed
   rows (`status='failed'`, `last_error LIKE '%SLACK_BOT_TOKEN%'`) and show them reaching
   `done`. Report the count requeued and the count that then succeeded; do not bulk-requeue
   all 23k in this round.
5. Quote the actual job rows and the `graph.nodes` row in the report. "No errors" is not
   verification.

## Known open question, deliberately out of scope

Once the node exists, the only alerting path for `#payments` is hot-topic subscription 1
(`channel_filter={}`, `min_participants=2`; Ross clears the bar via `has_important` because
`graph.people` id 452 carries `eeid 111` and matches the `Ross Veitch` entry in
`importance.json`). `notify_watch_channels` will never fire there: `#payments` classifies into
the `core` continent by name prefix and that job only DMs the `partners` continent
(`notify_watch_channels.go:18`). So the LLM relevance judge is the last gate, and subscription 1's
scope definition is written around Wego payment-method implementation while explicitly excluding
"broader supplier/partner contracts unrelated to payment processing". Whether a CEO message about
Google's UCP standard survives that judge is unknown. Step 2 above answers it empirically. Do not
pre-emptively change the judge, the scope text, or the importance rules in this round.
