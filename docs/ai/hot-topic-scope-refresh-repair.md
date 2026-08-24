# Plan — repair the hot-topic scope refresh (`agent-mem-uab5`, `-ob36`, `-v9q7`)

**Repo:** `~/go/src/github.com/agent-mem` · **Branch off:** `main` · **Verified against** `8e81b0c`
**Deploy target:** the hub = payments Mac mini (`ssh enzo@payments`). See `CLAUDE.md` — do NOT use `make deploy`.

You have no memory of the conversation that produced this. Everything you need is below.
Every `file:line` was read against the tree at `8e81b0c`; every HTTP claim was executed live on
2026-08-24.

---

## The incident this fixes

Enzo added a 5th knowledge source to topic subscription `1` (`payments`) in the dashboard's
HOT-TOPIC ALERTS panel and clicked save. The `analyzing…` badge stayed up for ~16 minutes, then
silently reverted to `↻ refresh` — and the scope summary that had been on screen since 2026-07-01
was gone. Four distinct defects stacked up.

### 1. The refresh job was never claimed for 16 minutes (LLM cap starvation)

`graph.jobs 5428442` (`refresh_topic_scope`) sat `status='queued'`, `attempts=0`, from
`23:44:15Z` to `00:00:0?Z`. It was not slow — it was never picked up.

`refresh_topic_scope` is registered with `UsesLLM: true` (`internal/graph/handlers/handlers.go:98-104`).
`TypeDispatcher.Run` refuses to claim **any** `UsesLLM` job while the hourly generate cap is binding
(`internal/graph/jobs/dispatcher.go:93-104`), which is deliberate — the comment at
`internal/graph/jobs/registry.go:27-29` says it exists to preserve the retry budget. Worker logs at
`23:11Z` confirm the pause fired for `link_topics`, `index_artifact`, `describe_attachment`,
`summarize_thread`, `derive_feature_entity`, `refresh_topic_scope`, `detect_hot_topics`.

The cap (`LLMHourlyCallCap`, default surfaced at `internal/config/config.go:96-101`, live-editable
via `PUT /api/settings`) was fully consumed by `worker.(*Server).processObservation` — the Claude
Code observation pipeline, which is **not** a dispatcher and therefore not subject to the pause. It
requeues on `ErrCapped` every ~30s and re-consumes the budget the instant it frees.

The meter resets on a **clock-hour boundary** (`internal/llmgateway/client.go:124-132`,
`tickLocked`). At `00:00:02Z` the counter zeroed, the dispatcher claimed the job on its next 5s idle
tick (`internal/worker/server.go:293`), and it finished in ~2 seconds. So the worst-case interactive
wait today is "however long is left in the current clock hour".

### 2. Every source read failed — and the Confluence one for a config reason, not a permissions one

| Source | Result |
|---|---|
| `…/wiki/spaces/PA/pages/2122252293/Payment+PRDs` | `404` Jira dead-link page |
| `wego/payments` | `401 Bad credentials` |
| `wego/payments-react-component` | `401 Bad credentials` |
| `wego/payments-dispute-automation` | `401 Bad credentials` |
| `wego/payments-knowledge` | `401 Bad credentials` |

**Confluence.** The hub has `AGENT_MEM_CF_BASE_URL=https://wegomushi.atlassian.net` — no `/wiki`
suffix. `cfBase()` uses `CFBaseURL` **verbatim when non-empty** and only appends `/wiki` in the
`JiraBaseURL` fallback branch (`internal/graph/fetchers/sources.go:30-37`). So
`ConfluenceDescendants` (`:50-55`) requested
`https://wegomushi.atlassian.net/api/v2/pages/2122252293/descendants?limit=250` — a Jira path.
Jira's router answered with its HTML 404, which is the `Oops, you've found a dead link. - JIRA`
body in the log.

Verified live with the hub's own `AGENT_MEM_CF_TOKEN` and `enzo@wego.com`:

```
/api/v2/pages/2122252293                        -> 404
/wiki/api/v2/pages/2122252293                   -> 200
/wiki/api/v2/pages/2122252293/descendants       -> 200, 45 pages, one API page (limit=250)
```

The token is valid, the page is readable, the sub-tree is 45 pages (`2023`, `Q2`, `Restriction on
BNPL Usage below an amount`, `VCC Understanding - Flights`, `Charge Currency Optimization`,
`Selling Gift Cards`, `Card Instalments`, …). **Nothing is wrong with Confluence or the
credential — only with the base URL.**

**GitHub.** `AGENT_MEM_GH_TOKEN` is commented out on the hub, deliberately: the `.env` note says the
`gh` CLI token carries `admin:org`/`delete_repo`/`workflow`, far beyond what agent-mem needs, and
asks for a fine-grained read-only PAT instead. `ghAuth` still sends `Authorization: Bearer ` with an
empty value (`internal/graph/fetchers/github.go:136`), so GitHub answers `401`. That same `.env`
comment claims the failure is "loud, no retry grind" — it is **not**: `refresh_topic_scope` only
`log.Warn()`s and swallows it (`internal/graph/handlers/refresh_scope.go:88-92`), so from the
dashboard it is indistinguishable from success.

### 3. The failed refresh destroyed a working scope

`genScope` returns `("", "")` on every failure path — no material (`refresh_scope.go:217-219`), LLM
error (`:226-228`), unparseable JSON (`:233-235`). The caller then writes those empty strings
**unconditionally**:

```go
// internal/graph/handlers/refresh_scope.go:113-121
scopeDef, scopeSum := genScope(ctx, deps, topic, titles, docs)
status := "ready"
if scopeDef == "" { status = "error" }
_, err := deps.DB.Exec(ctx,
    `UPDATE graph.topic_subscriptions
     SET scope_definition=$2, scope_summary=$3, scope_status=$4, scope_refreshed_at=NOW()
     WHERE id=$1`, p.SubscriptionID, scopeDef, scopeSum, status)
```

Observed on sub `1`: `scope_summary` went from 866 chars (distilled 2026-07-01) to `0`, and
`scope_definition` to `0`. **A failed read deleted the judge's scope guidance.** Since
`scope_definition` is what the hot-topic relevance judge consumes, the subscription is now
strictly worse than before the refresh.

### 4. The dashboard never says any of this

- `LiveGlobe.tsx:3401` — `const refreshing = s.scope_status === 'refreshing'`. `'error'` is not
  handled anywhere, so on failure the button just returns to `↻ refresh` with no message.
- `refreshSubScope` (`LiveGlobe.tsx:1237-1256`) polls `listSubscriptions` every 5s and gives up at
  `tries > 40` — a hard **200-second** ceiling with no state change and no message. A job waiting on
  the LLM cap outlives the poller every time, which is exactly the "spinner forever" Enzo saw.
- The list endpoint returns `scope_summary` and `scope_status` but not `scope_refreshed_at`
  (`detect_hot_topics.go:780-784`), so the UI cannot even say how stale the scope is.

---

## Goal

1. A Confluence source pointed at a page the user can read actually gets read — all 45 descendants
   of `2122252293` ingested and distilled.
2. A failed refresh **never** destroys a scope that was working.
3. Every failure is visible in the dashboard, naming the source and the reason.
4. A refresh that is waiting on the LLM budget says so, and the UI keeps watching it to completion
   instead of abandoning it at 200s.

## Non-goals

- **Do not** re-architect the LLM cap. No per-caller quotas, no reserved interactive headroom, no
  priority lanes. Part 4 makes the wait *honest*, not shorter. The upgrade path is named in
  "Deferred" below — build it only if a measured wait actually hurts.
- **Do not** mint or install a GitHub token as part of this work. That is Enzo's call and a
  credential decision (see "Enzo's tasks").
- **Do not** touch `detect_hot_topics` judging, the DM builder, `notify_watch_channels`, or the
  `graph.topic_judgments` cache.
- **Do not** widen `graph.jobs` semantics or add job-status plumbing to the dashboard API.
- The orphaned-lease janitor bug (`agent-mem-h06j`) is a separate plan:
  `docs/ai/janitor-orphan-null-lease.md`.

---

## Approach

### Part 0 — hub config: `/wiki` (ops, no code) · `agent-mem-uab5`

On the hub, in `~/go/src/github.com/agent-mem/.env`:

```
AGENT_MEM_CF_BASE_URL=https://wegomushi.atlassian.net/wiki
```

Then restart the worker (no rebuild needed — this is env only):

```bash
ssh enzo@payments 'cd ~/go/src/github.com/agent-mem && PATH=/opt/homebrew/bin:$PATH docker compose up -d worker'
```

`AGENT_MEM_JIRA_BASE_URL` stays `https://wegomushi.atlassian.net` — Jira's REST paths are
site-rooted; only Confluence lives under `/wiki`. Do not add `/wiki` to the Jira variable.

### Part 0b — code: make `cfBase()` tolerant so this cannot recur

`internal/graph/fetchers/sources.go:30-37`. Normalise: after trimming the trailing slash, if
`CFBaseURL` does not already end in `/wiki`, append it. Both `https://…atlassian.net` and
`https://…atlassian.net/wiki` then produce the same correct base.

```go
// ponytail: a bare site host is the easy mistake; normalise instead of documenting it.
func (r *Registry) cfBase() string {
	base := strings.TrimRight(r.cfg.CFBaseURL, "/")
	if base == "" {
		base = strings.TrimRight(r.cfg.JiraBaseURL, "/")
	}
	if !strings.HasSuffix(base, "/wiki") {
		base += "/wiki"
	}
	return base
}
```

One table-driven unit test: bare host, host with `/wiki`, host with a trailing slash, and the
`CFBaseURL==""` fallback all yield `https://x/wiki`. Keep Part 0 anyway — the env should be correct
on its own, not rescued by the code.

### Part 1 — never wipe a good scope on failure · `agent-mem-ob36`

`internal/graph/handlers/refresh_scope.go:113-121`. Split the write in two. On success write the
scope as today; on failure touch **only** the status and the timestamp.

```go
scopeDef, scopeSum := genScope(ctx, deps, topic, titles, docs)
var err error
if scopeDef != "" {
	_, err = deps.DB.Exec(ctx,
		`UPDATE graph.topic_subscriptions
		 SET scope_definition=$2, scope_summary=$3, scope_status='ready',
		     scope_refreshed_at=NOW(), scope_error=''
		 WHERE id=$1`, p.SubscriptionID, scopeDef, scopeSum)
} else {
	// ponytail: a failed read must not delete the scope the judge is using.
	_, err = deps.DB.Exec(ctx,
		`UPDATE graph.topic_subscriptions
		 SET scope_status='error', scope_refreshed_at=NOW(), scope_error=$2
		 WHERE id=$1`, p.SubscriptionID, srcErrText)
}
```

Sub `1`'s July scope is already gone and is **not** recoverable from the DB — no history table. It
regenerates on the first successful refresh after Part 0. Say so in the report; do not hand-write a
replacement scope.

### Part 2 — record *why* it failed, and show it · `agent-mem-v9q7`

The per-source failures are currently only `log.Warn()`s inside the handler
(`refresh_scope.go:65`, `:73`, `:85`, `:91`, `:106`). Collect them instead of only logging them, and
persist one short string.

**Migration** `migrations/20260824000001_topic_scope_error.sql`, matching the house style of
`20260629000001_topic_scope_sources.sql` (`+goose Up` / `+goose Down`, `IF NOT EXISTS`,
`NOT NULL DEFAULT ''`):

```sql
-- +goose Up
ALTER TABLE graph.topic_subscriptions
  ADD COLUMN IF NOT EXISTS scope_error TEXT NOT NULL DEFAULT '';  -- last refresh's per-source failures

-- +goose Down
ALTER TABLE graph.topic_subscriptions
  DROP COLUMN IF EXISTS scope_error;
```

A column, not a reuse of `scope_summary`: after Part 1 the summary is the *preserved good* value on
failure, so the error genuinely has nowhere else to live.

In the handler, accumulate one line per failed source — `"<type> <url>: <err>"` — alongside the
existing `log.Warn()` (keep the logs). Truncate the joined text to ~1000 chars and to at most one
line per source; a raw Atlassian HTML 404 body is useless in a dashboard, so keep only the status
line, e.g. `confluence …/pages/2122252293: status 404`. If nothing failed but `genScope` still
returned empty, write `"no readable content in 5 source(s)"` so `'error'` is never reasonless.

**API** (`detect_hot_topics.go`): add `COALESCE(scope_error,'')` and `scope_refreshed_at` to the
list `SELECT` at `:780-784` and to the `subscription` struct at `:40-50`; mirror both in
`dashboard/src/api.ts:578-588` (`scope_error?: string; scope_refreshed_at?: string;`).

**UI** (`LiveGlobe.tsx`, the subscription card starting `:3399`): when `scope_status === 'error'`,
render the `scope_error` lines in `C.red` under the source list, plus `last ok: <scope_refreshed_at>`
when a preserved `scope_summary` is still shown — so a stale-but-valid scope is never mistaken for a
fresh one. Reuse the existing `subError` styling (`:3378`); no new component.

### Part 3 — distinguish "queued" from "analyzing", and stop abandoning the poll · `agent-mem-v9q7`

Today the HTTP handler sets `scope_status='refreshing'` at **enqueue** time
(`detect_hot_topics.go:903`), so a job blocked behind the LLM cap is indistinguishable from one
actively running. Free fix, no new column and no job-table exposure:

1. `refresh` handler (`detect_hot_topics.go:895-921`) sets `scope_status='queued'`.
2. `NewRefreshTopicScope` sets `scope_status='refreshing'` as its **first** DB write, before any
   fetching (`refresh_scope.go`, right after the subscription row loads at `:46-51`).
3. `LiveGlobe.tsx:3401` treats both as in-flight:
   `const busy = s.scope_status === 'queued' || s.scope_status === 'refreshing'`, with the label
   `queued…` vs `analyzing…`. Title attribute on `queued…`: *"waiting for LLM budget; the hourly cap
   resets on the hour"*.
4. `refreshSubScope` (`:1237-1256`): drop the `tries > 40` cliff. Poll every 5s for the first
   minute, then every 20s, and stop only when the status leaves `queued`/`refreshing` or the panel
   closes. Clear the interval on unmount — the current code leaks it if the overlay closes mid-poll.

### Part 4 — the truthful cap message

No dispatcher change. `queued…` plus its tooltip from Part 3 **is** Part 4. The claim-pause is
working as designed (`registry.go:27-29`); the only defect was that the UI lied about it.

---

## Files expected to change

| File | Change |
|---|---|
| `<hub>:~/go/src/github.com/agent-mem/.env` | `AGENT_MEM_CF_BASE_URL` gains `/wiki` (Part 0, not in git) |
| `internal/graph/fetchers/sources.go` | `cfBase()` normalises `/wiki` |
| `internal/graph/fetchers/sources_test.go` | new table test for `cfBase()` |
| `migrations/20260824000001_topic_scope_error.sql` | new: `scope_error` column |
| `internal/graph/handlers/refresh_scope.go` | collect source errors; split the success/failure write; set `'refreshing'` on start |
| `internal/graph/handlers/detect_hot_topics.go` | `refresh` sets `'queued'`; list returns `scope_error` + `scope_refreshed_at`; struct fields |
| `dashboard/src/api.ts` | `scope_error`, `scope_refreshed_at` on `TopicSubscription` |
| `dashboard/src/pages/LiveGlobe.tsx` | `queued`/`refreshing` labels, error block, poll without the 200s cliff |
| `internal/worker/dashboard/*` | rebuilt embed — see below |

**Dashboard embed rebuild is mandatory before committing** (`CLAUDE.md`):

```bash
cd dashboard && npm run build
cd .. && rm -rf internal/worker/dashboard/* && cp -R dashboard/dist/* internal/worker/dashboard/
```

---

## Acceptance criteria

1. `cfBase()` returns `https://wegomushi.atlassian.net/wiki` for a bare host, a `/wiki` host, and a
   trailing-slash host; unit test proves all three.
2. A refresh on sub `1` after Part 0 logs `refresh_topic_scope: done` with `titles=45` (not `0`) and
   `status=ready`, and `scope_definition`/`scope_summary` are non-empty in the DB.
3. Forcing a failure (point a sub at a garbage Confluence id) leaves the previous
   `scope_definition`/`scope_summary` **byte-identical**, sets `scope_status='error'`, and writes a
   readable `scope_error`.
4. The dashboard shows that `scope_error` in red, and still shows the preserved summary labelled
   with its `last ok` timestamp.
5. With the hourly cap saturated, clicking refresh shows `queued…` (not `analyzing…`), and the badge
   still transitions to `ready`/`error` on its own after the hour rolls over — i.e. the poll survives
   past 200s.
6. GitHub sources still fail (no token) but now say `github wego/payments: status 401` in the UI
   instead of failing silently.
7. `go build ./... && go vet ./...` clean; `go test ./internal/graph/...` passes. Handler integration
   tests run against the `agentmem_test` scratch DB **only** — never the live dev DB (it truncates
   the graph and syncs to prod).

## How to verify on the hub

```bash
# deploy (arm64 native, no GHCR round trip)
ssh enzo@payments 'cd ~/go/src/github.com/agent-mem && git pull --ff-only \
  && PATH=/opt/homebrew/bin:$PATH docker compose up -d --build worker'

# prove the running binary carries the change — never trust the build log
ssh enzo@payments 'PATH=/opt/homebrew/bin:$PATH docker exec agent-mem-worker-1 \
  grep -c "waiting for LLM budget" /usr/local/bin/agent-mem'

# trigger, then watch
ssh enzo@payments 'PATH=/opt/homebrew/bin:$PATH docker exec agent-mem-postgres-1 \
  psql -U agentmem -d agentmem -c "SELECT scope_status, scope_refreshed_at, \
  length(scope_definition) d, length(scope_summary) s, left(scope_error,200) e \
  FROM graph.topic_subscriptions WHERE id=1;"'

ssh enzo@payments 'PATH=/opt/homebrew/bin:$PATH docker logs --since 10m agent-mem-worker-1 2>&1 \
  | grep topic_scope'
```

If the cap is saturated when you test, the job stays `queued` until the top of the hour. That is
expected now, and the UI must say so. To test the happy path immediately, raise the cap temporarily
via `PUT /api/settings {"llm_hourly_call_cap": N}` and put it back afterwards.

## Enzo's tasks (not the implementer's)

- **GitHub read access.** Mint a fine-grained read-only PAT (contents:read + metadata:read on the
  four `wego/payments*` repos) and append `AGENT_MEM_GH_TOKEN=` to the hub `.env`. Until then the
  four GitHub sources contribute nothing and the scope is Confluence-only. Alternative: drop them
  from the subscription so the source count stops overstating coverage.
- Confirm whether the 45-page `Payment+PRDs` tree alone is enough scope material, or whether more
  Confluence roots should be added.

## Deferred — named, not built

- **Reserved LLM headroom for interactive work.** Add `Interactive bool` to `jobs.Entry` and have
  background `UsesLLM` dispatchers pause at `cap - reserve` rather than `cap`, leaving the last
  slice for user-clicked jobs. This only works if the same reserve is enforced in
  `worker.(*Server).processObservation`, which is not a dispatcher and is the actual burner — so it
  is a two-place change to a shared global meter. Build it only if a measured wait actually hurts;
  the honest `queued…` label may be sufficient.
- **Confluence-token separation.** `AGENT_MEM_CF_TOKEN` and `AGENT_MEM_JIRA_TOKEN` are currently the
  same Atlassian token value on the hub. Harmless, but the two-variable design implies they can
  diverge; nothing needs doing now.

## Known landmines

- **`/wiki` on the wrong variable.** Adding it to `AGENT_MEM_JIRA_BASE_URL` breaks every Jira
  fetcher. Only `AGENT_MEM_CF_BASE_URL` gets it.
- **The 401 body is HTML.** Both the GitHub and Confluence error strings embed raw response bodies
  (`sources.go:79`, `:204`). Never write those into `scope_error` verbatim — a 256-byte HTML head
  fragment in a dashboard card is worse than no message. Extract the status line.
- **`genScope` fails silently three different ways** (`:217`, `:226`, `:233`). Part 2's "no readable
  content" fallback must also cover the LLM-error and bad-JSON paths, or `'error'` with an empty
  `scope_error` reappears.
- **The embed.** Dashboard changes that are not re-embedded ship a stale UI that looks like the fix
  did not work. Rebuild before committing, not after.
- **`scope_status` is a bare `TEXT`** with no CHECK constraint
  (`20260629000001_topic_scope_sources.sql:10`, comment says `'' | 'refreshing' | 'ready' | 'error'`).
  Adding `'queued'` needs no migration, but update that comment and every read site — grep for
  `scope_status` before assuming you found them all.
- **Test DB.** Handler integration tests truncate the graph and the fixtures sync to prod. Use
  `agentmem_test`.
