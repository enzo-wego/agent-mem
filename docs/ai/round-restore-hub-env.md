# Round plan — restore the hub's environment config, and make a missing credential impossible to ignore

Written 2026-08-22. **Self-contained: assume the reader has no memory of the conversation that
produced it.** This file is the worker's brief.

Background, all measured on production (`ssh enzo@payments`) on 2026-08-22:

The agent-mem hub moved from a VPS to the payments Mac mini on 2026-08-12. The move carried the
code and the database but **not the environment**. The worker container receives exactly one
variable, `DATABASE_URL`. There is no `.env` file on the hub and `docker-compose.yml` passes
nothing else.

`internal/config/config.go` reads 48 `AGENT_MEM_*` variables. 16 are also DB-backed through the
`settings` table, which is why the LLM gateway and API auth kept working and hid the problem. The
other ~32 are env-only and have been absent for 10 days.

Proof, with a date: `refresh_slack_channels` has 18 rows `failed` with
`fatal: refresh_slack_channels: SLACK_BOT_TOKEN not set`, and its last success was
**2026-08-12 19:04** — the day of the move.

Issues: `agent-mem-bctq` (P0, the missing config), `agent-mem-egsf` (P1, silent success),
`agent-mem-zn0` (P1, dead `vps` runner). Run `bd show <id>` for each.

## Goal

1. The hub's credentials are supplied through a file that a host migration cannot silently drop,
   and `.env.example` documents every variable so the next migration has a checklist.
2. A handler missing a required credential **fails loudly** instead of reporting success.
3. Jobs pinned to `target_runner='vps'` become claimable again.

## Non-goals — do NOT do these

- **Do NOT resume the hub or change `processing_paused`.** It is `true` and stays `true`. Resuming
  is a separate human decision after this lands.
- **Do NOT put real secrets in any file in this repository**, including `.env.example`. Placeholders
  only. `.env` itself must be gitignored — check that it already is and add it if not.
- **Do NOT add Datadog, PagerDuty or Sentry credentials.** The hub has zero nodes from those
  systems (`graph.nodes` grouped by id prefix: slack 28,843 / slack_file 2,183 / jira 1,170 /
  gh_pr 852 / jira_attachment 796 / cf 101 / gws_doc 78, and no datadog or pagerduty rows). Their
  fetchers exist but nothing reaches them.
- **Do NOT add the GWS service-key mount.** `AGENT_MEM_GWS_SERVICE_KEY_PATH` needs a file mounted
  into the container, which is a bigger compose change for 78 nodes. Out of scope; note it in
  `.env.example` as unsupported-for-now.
- **Do NOT change the rate-limit or base-URL defaults.** They already have sane values in the
  defaults block (`config.go:414` — Slack 5, Jira 5, GitHub 10, Confluence 5) so an unset env var
  is harmless. Only `JIRA_BASE_URL` and `CF_BASE_URL` lack defaults and must be settable.
- **Do NOT migrate credentials into the DB-backed settings table.** That is the better long-term
  design and it is a separate round — it touches secret storage and the dashboard.
- Do not run tests against a real database. Leave `DATABASE_URL` unset.

---

## Fix A — `.env.example` and wire `env_file` into compose (`agent-mem-bctq`)

1. Create `.env.example` at the repo root listing **every** `AGENT_MEM_*` variable the code reads,
   grouped by system, with placeholder values and a one-line comment each. Mark each as
   `# REQUIRED`, `# OPTIONAL`, or `# UNSUPPORTED (needs a volume mount)`.

   Derive the list from the code, not from this plan:
   `grep -oE 'os.Getenv\("AGENT_MEM_[A-Z_]+"\)' internal/config/config.go`
   Say in your report if the count differs from 48.

   Required for this hub, based on the node census above:
   - `AGENT_MEM_SLACK_BOT_TOKEN`, `AGENT_MEM_SLACK_DM_USER`
   - `AGENT_MEM_JIRA_EMAIL`, `AGENT_MEM_JIRA_TOKEN`, `AGENT_MEM_JIRA_BASE_URL`
   - `AGENT_MEM_GH_TOKEN`
   - `AGENT_MEM_CF_TOKEN`, `AGENT_MEM_CF_BASE_URL`
   - `AGENT_MEM_GRAPH_RUNNER` — see Fix C

   Include a header comment stating that the 16 DB-backed settings (api_key, llm_gateway_url,
   llm_gateway_api_key, machine_id, sync_*, context_*, log_level, allowed/ignored_projects,
   gemini_embedding_dims, public_base_url, skip_tools) live in the `settings` table and do **not**
   need to be in this file — that distinction is exactly what made this outage invisible.

2. Add `env_file` to the **worker** service in `docker-compose.yml`. It must not break a checkout
   that has no `.env`: use the optional form

   ```yaml
   env_file:
     - path: .env
       required: false
   ```

   Verify that form is supported by the compose version on the hub — run
   `ssh enzo@payments 'PATH=/opt/homebrew/bin:$PATH docker compose version'` and if it is too old,
   fall back to plain `env_file: [.env]` and say so in your report, because then a missing `.env`
   becomes a hard failure for local development.

   Keep the existing `DATABASE_URL` entry under `environment:` — values there win over `env_file`,
   which is correct: the container must always reach postgres by service name.

3. Confirm `.env` is gitignored.

### Acceptance for A

- `docker compose config` renders without error both with and without a `.env` present.
- `.env.example` contains every variable `config.go` reads, and no real secret.
- `git check-ignore .env` succeeds.

---

## Fix B — a missing credential must fail, not silently succeed (`agent-mem-egsf`)

`notify_watch_channels.go:110` and `monitor_hourly_report.go:60` both guard with
`if to == "" || deps.SlackBotToken == "" { … }` and return `nil`, so the job is marked `done`.

Measured consequence: `notify_watch_channels` holds **914,417 rows with `status='done'`**, and every
one since 2026-08-12 delivered nothing. The queue looks perfectly healthy while no notification or
hot-topic alert has reached anyone for 10 days. `refresh_slack_channels` is the one handler that
does the right thing — it returns `fatal: ... SLACK_BOT_TOKEN not set` — and it is the only reason
the outage was found at all.

1. In every handler that requires a credential, a **missing credential** must return
   `fmt.Errorf("%w: <handler>: SLACK_BOT_TOKEN not set", jobs.ErrFatal)` (or the equivalent for its
   own credential), matching the wording `refresh_slack_channels` already uses. `ErrFatal` is
   correct rather than transient: no amount of retrying conjures a token, and `IsRetryable`
   (`internal/graph/jobs/backoff.go`) already treats `ErrFatal` as terminal.

2. Keep the distinction between the two conditions in that guard. `deps.SlackBotToken == ""` is a
   **misconfiguration** and must fail. `to == ""` — no recipient resolved — is a legitimate
   data-dependent no-op and must stay a silent success. Do not collapse them; a handler that fails
   because a channel has no watcher would be worse than what we have now.

3. Audit the other registered handlers for the same `if credential == "" { return nil }` shape and
   fix each. Enumerate what you found in your report — I expect Slack-dependent ones
   (`notify_watch_channels`, `monitor_hourly_report`, `detect_hot_topics` alerting path,
   `refresh_slack_*`, `backfill_slack_*`) and possibly `refresh_jira_board`.

### Acceptance for B

- A test per fixed handler proving that with an empty credential and a **valid** recipient the
  handler returns an error satisfying `errors.Is(err, jobs.ErrFatal)`.
- A test proving that with a **present** credential and an empty recipient the handler still
  returns `nil` — the data-dependent no-op is preserved.
- No existing test deleted or weakened.

---

## Fix C — `GRAPH_RUNNER` (`agent-mem-zn0`, operational half)

The claim query is `AND target_runner IN ('any', $2)` (`internal/graph/jobs/queue.go:105`) where
`$2` is this worker's runner. With `AGENT_MEM_GRAPH_RUNNER` unset, the runner defaults to `"any"`
(`config.go:409`), so it claims only `'any'` rows and **every `'vps'` row is unclaimable** — 641 of
them, including 100 `backfill_slack_thread` still being created daily by `ingest_content.go:400`.

Setting the hub's runner to `vps` makes it claim `'any'` **and** `'vps'`, which un-strands them.
The name is wrong — it means "the machine holding the credentials", which is now the Mac mini — but
renaming it is a code change with a migration, and this round only needs the operational fix.

**This fix is a single line in `.env.example` plus documentation.** Do not change the default in
`config.go`, and do not touch `ingest_content.go`. Add to `.env.example`:

```
# Which target_runner values this worker will claim. The claim query matches
# target_runner IN ('any', $THIS). Historically 'vps' meant "the machine holding the
# Slack/Jira credentials"; since 2026-08-12 that machine is the payments Mac mini.
# Set this to 'vps' on the hub so the ~641 rows pinned to 'vps' remain claimable.
# Renaming this value is tracked in agent-mem-zn0.
AGENT_MEM_GRAPH_RUNNER=vps
```

### Acceptance for C

- Documented in `.env.example` with that reasoning. No code change.

---

## Quality gates — run these yourself and paste the output

```bash
go build ./...
go vet ./...
go test ./internal/graph/handlers/... ./internal/graph/jobs/... ./internal/config/...
docker compose config >/dev/null && echo "compose OK"
```

`DATABASE_URL` must be unset. DB-backed tests should skip, not fail.

## Definition of done

1. Gates clean, output pasted.
2. Every acceptance bullet has a named test that would fail if the behaviour regressed.
3. No real secret in any tracked file. `.env` gitignored.
4. No TODO placeholders, no `t.Skip` added, no stub tests, no existing test weakened.
5. `processing_paused` untouched, nothing deployed.

## Report back

The diff per fix; the pasted gates; the variable count from `config.go` and whether it differed
from 48; the full list of handlers you found with the silent-skip shape and what you did to each;
and whether the compose `required: false` form is supported on the hub.

## What happens after this round (not the worker's job)

Enzo creates `/Users/enzo/go/src/github.com/agent-mem/.env` on the hub from `.env.example` with
real values — Slack from the app that p-agent already uses, Jira/GitHub/Confluence from their
consoles. Then redeploy, and verify with:

```bash
ssh enzo@payments 'PATH=/opt/homebrew/bin:$PATH docker exec agent-mem-worker-1 \
  env | grep -oE "^AGENT_MEM_[A-Z_]+" | sort'
```

That must list the required set. The hub stays paused until then.
