# Plan — an always-alert author is never dropped by the relevance judge

**Issue:** `agent-mem-3q20` (P2) · **Repo:** `~/go/src/github.com/agent-mem` · **Branch off:** `main` (at `0032ce4`)

You have no memory of the conversation that produced this. Everything below was verified on the
payments hub on 2026-09-02.

---

## The incident

Ross Veitch (CEO) posted in `#payments` at 05:46:31Z asking for an assessment of what Wego's
checkout and payments stack needs to support Google's Universal Commerce Protocol. Enzo got no
alert. `agent-mem-khqh` (shipped as #50) fixed the ingest half, and after a backfill the node
exists and the importance path fires correctly. The judge then rejected it:

```
08:34 detect_hot_topics: topic relevance
      node=slack:CKQ6XGTCZ:1788327991.032099 participants=1 relevant=false
```

`participants=1` is the proof `has_important` worked, since a lone message is only ever evaluated
via the importance path. The verdict is cached in `graph.topic_judgments`
(subscription 1, `msg_count` 1, `relevant=f`) and will not be re-rolled.

The judge is not merely strict here, it is inconsistent. Five minutes later the same author's
message in `#payments-b2c` (`slack:C02E70LD5MY:1788338058.761709`, also `participants=1`) was
judged `relevant=true` and alerted. Same person, same day, both payments-related, opposite verdicts.

## Goal

A thread authored by someone explicitly marked always-alert in `importance.json` reaches Enzo
without the LLM relevance judge getting a veto.

## Why a bypass and not a better prompt

Measured volume: person 452 (Ross) authored **20 messages in 30 days** across every channel
agent-mem sees (`product-hajj-umrah` 9, `product-issues` 7, `payments` 2, `payments-b2c` 1,
`mobile-public` 1). Under a bypass that is well under one DM a day, so the cost of over-notifying
is near zero while the cost of the current behaviour is a missed CEO directive.

## Non-goals

- **Do not touch subscription 1's `scope_definition`.** It is tempting, and it is out of scope
  here: that same text is the eligibility gate's scope
  (`settings.graph.eligibility_gate` → `scope_subscription_id: 1`), so rewording it re-embeds the
  gate's scope vector as well. Wider blast radius, separate round.
- Do not widen the `important` set. The org-distance set (reporting line plus ~2 hops) stays
  exactly as it is and keeps going through the judge. Only the explicit
  `importance.json` overrides that opt in get the bypass.
- Do not add a dashboard setting for this. `importance.json` is embedded and documented as
  "edit the file and rebuild" (`importance.go:12-14`); follow that convention.
- Do not touch `notify_watch_channels`, the continents config, the eligibility gate, or
  `min_participants`.
- Keep every other suppression intact: the channel `ignore` list, the "skip threads the
  subscriber already posted in" rule (`has_subscriber`), and the `topic_notifications` dedup.
  A bypass of the *relevance* gate is not a bypass of everything.

## Files expected to change

| File | Change |
|---|---|
| `internal/graph/handlers/importance.json` | Add `"always_alert": true` to the `Ross Veitch` override entry only. Leave Alexandre Morin and Surbhi Babbar as they are; adding them later is a one-line edit. |
| `internal/graph/handlers/importance.go` | The `Overrides` struct gains `AlwaysAlert bool \`json:"always_alert"\``. Add `alwaysAlertEeids(ctx, db, owner)` next to `overrideImportantEeids` (`importance.go:44`), resolving only the opted-in names, with the same owner check and the same `eeid IS NOT NULL AND merged_into IS NULL` display-name match. |
| `internal/graph/handlers/detect_hot_topics.go` | `hotThread` gains `HasAlwaysAlert bool`. `findHotThreads` takes the extra eeid slice, adds `(p.eeid = ANY($6::int[])) AS is_always_alert` in the `recent` CTE and `bool_or(...) AS has_always_alert` in `grp`, and selects it. In the handler loop (`detect_hot_topics.go:120-137`), skip both the cached-verdict check and `topicMatches` when `h.HasAlwaysAlert` — go straight to the dedup claim. Do not write a judgment row for a bypassed thread. |

Note the SQL already uses `$1..$5`; the new parameter is `$6`. Pass an empty slice, never nil,
the way `important` is normalised at `detect_hot_topics.go:300-302`.

## Acceptance criteria

- `go build ./...` clean; `go test ./internal/graph/handlers/... ./internal/worker/...` pass.
- A test proves a thread from an always-alert author alerts **with no LLM call at all** (a
  `topicMatches` that would return false, or a nil/failing Gemini client, must not prevent the
  alert). This is the whole point of the change; assert it directly.
- A test proves a thread from a merely-important author still goes through the judge, so the
  bypass has not leaked to the wider set.
- A test proves the bypass still respects `has_subscriber` and the ignore list.
- No judgment row is written for a bypassed thread.

## How to verify on the hub

Deploy per `CLAUDE.md`: `ssh enzo@payments`, `git pull --ff-only`, `docker compose up -d --build worker`.
The hub is the payments Mac mini; `make deploy` is broken and targets the retired VPS.

1. Confirm the running binary carries the change (`docker exec agent-mem-worker-1 grep -c
   "<new string literal>" /usr/local/bin/agent-mem`), not just the build log.
2. **Clear the cached verdict**, or the message stays silent no matter what the code does:
   ```sql
   DELETE FROM graph.topic_judgments
    WHERE root_node_id = 'slack:CKQ6XGTCZ:1788327991.032099';
   ```
3. Wait for the next `detect_hot_topics` tick (5 min; the job self-reschedules). Then show all
   three of:
   - a `graph.topic_notifications` row for `(1, slack:CKQ6XGTCZ:1788327991.032099)`
   - the worker log line `hot-topic alert sent node=slack:CKQ6XGTCZ:1788327991.032099`
   - **no** `topic relevance ... relevant=` line for that node on this run
4. Confirm Enzo actually received the DM, and quote its text in the report. A
   `topic_notifications` row proves the claim was taken, not that Slack delivered anything.
5. Quote the real rows and log lines. "No errors" is not verification.

## If the DM does not arrive

Check `deps.SlackDMUserID` and `subscriber_slack_id` on subscription 1 (`U07UAC0J7T3`, which
resolves to person 335, eeid 982, the `owner_eeid` in `importance.json`). The token itself is
fine as of #50.
