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

## Deploy: build here, the VPS only ever pulls

The VPS (`enzo@enzogo.io.vn`, Ubuntu/x86_64) is too small to build images — it
runs out of resources. NEVER run `docker build`/`docker compose build` on it.
Build the `linux/amd64` image on the dev machine, push to GHCR, and let the VPS
pull. `make deploy` does exactly this; use it. The VPS pins the image via a
gitignored `docker-compose.override.yml` (`pull_policy: always`), which is why
`docker compose pull worker` works there even though the committed compose has
`build: .`.

The dashboard is a React/Vite app embedded into the Go binary via
`//go:embed all:dashboard` in `internal/worker/dashboard.go`. After any
`dashboard/src` change you MUST rebuild and re-sync the embed dir before
deploying, or the binary ships the stale UI:

```bash
cd dashboard && npm run build && cd ..
rm -rf internal/worker/dashboard/* && cp -R dashboard/dist/* internal/worker/dashboard/
make deploy   # buildx amd64 -> push GHCR -> ssh VPS pull-only
```

Commit the regenerated `internal/worker/dashboard/` contents along with the
source change (the hashed asset filenames change each build).

<!-- BEGIN BEADS INTEGRATION v:1 profile:minimal hash:ca08a54f -->
## Beads Issue Tracker

This project uses **bd (beads)** for issue tracking. Run `bd prime` to see full workflow context and commands.

### Quick Reference

```bash
bd ready              # Find available work
bd show <id>          # View issue details
bd update <id> --claim  # Claim work
bd close <id>         # Complete work
```

### Rules

- Use `bd` for ALL task tracking — do NOT use TodoWrite, TaskCreate, or markdown TODO lists
- Run `bd prime` for detailed command reference and session close protocol
- Use `bd remember` for persistent knowledge — do NOT use MEMORY.md files

## Session Completion

**When ending a work session**, you MUST complete ALL steps below. Work is NOT complete until `git push` succeeds.

**MANDATORY WORKFLOW:**

1. **File issues for remaining work** - Create issues for anything that needs follow-up
2. **Run quality gates** (if code changed) - Tests, linters, builds
3. **Update issue status** - Close finished work, update in-progress items
4. **PUSH TO REMOTE** - This is MANDATORY:
   ```bash
   git pull --rebase
   bd dolt push
   git push
   git status  # MUST show "up to date with origin"
   ```
5. **Clean up** - Clear stashes, prune remote branches
6. **Verify** - All changes committed AND pushed
7. **Hand off** - Provide context for next session

**CRITICAL RULES:**
- Work is NOT complete until `git push` succeeds
- NEVER stop before pushing - that leaves work stranded locally
- NEVER say "ready to push when you are" - YOU must push
- If push fails, resolve and retry until it succeeds
<!-- END BEADS INTEGRATION -->
