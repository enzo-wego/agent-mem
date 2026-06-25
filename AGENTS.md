# agent-mem — project conventions for AI agents

## Slack content must be normalized to names — never raw ids

Any Slack text that gets **stored** in the graph or **displayed** in the
dashboard MUST have its Slack markup resolved to human-readable form. Never
persist or render raw Slack tokens like `<@U09…>`, `<!subteam^S01…>`,
`<#C012…>`, or `<https://…|label>`.

Required resolutions:

| Raw token | Rendered as | Source of the name |
|---|---|---|
| `<@U…>` / `<@W…>` (user) | `@Display Name` | `graph.slack_users` (refresh_slack_users job, from `users.list`) |
| `<!subteam^S…>` (group) | `@group-name` | `graph.slack_groups` (refresh_slack_groups job, needs Slack `usergroups:read`); dashboard also has an editable `groups` map in the `graph_continents` setting as a fallback |
| `<#C…>` (channel) | `#channel-name` | channel name lookup; falls back to id |
| `<url\|label>` | `label (url)` | n/a |

### Where this is enforced

- **Normalizer:** `internal/graph/normalizer/slack.go` does the token→name
  conversion. It needs a non-nil `normalizer.Cache` to resolve names — the
  worker wires a DB-backed cache (`internal/worker/name_cache.go`) into
  `normalizer.NewDefault(...)`. **Do not pass `nil`** (that yields a no-op cache
  that leaves ids unresolved).
- **Both ingest paths must normalize identically:**
  1. Live forward path — Claude-Code-Remote normalizes before POSTing to
     `/api/graph/ingest/content`.
  2. Backfill path — `ingestSlackMessage` (`internal/graph/handlers/backfill_slack.go`)
     runs the slack normalizer on `msg.Text` before storing. Edge extraction
     runs on the **normalized** text (the normalizer preserves URLs as
     `label (url)`).
- **Frontend:** the dashboard applies `applyGroupNames()` (`dashboard/src/continents.ts`)
  to message bodies as a last-resort fallback for group ids.

### Rule of thumb

If you add a new code path that ingests, stores, or renders Slack text, run it
through the slack normalizer (with a real name cache) first. If a marker shows
a raw `C…`/`U…`/`S…` id to the user, that is a bug — resolve the name.

## Name lookups stay fresh via jobs

`refresh_slack_users` and `refresh_slack_groups` populate the name tables. New
people/groups appear with raw ids until the next refresh; re-running the refresh
job (or a re-backfill) resolves them.
