# Graph Memory — Full-Flow Test Guide

How to exercise the whole pipeline end-to-end: **insert → process → read**.
Drives a real Slack channel backfill, watches the job queue drain, then
queries the graph that was built.

> Run everything against the **VPS** (`enzo@enzogo.io.vn`), not a laptop.
> The worker, the 5 graph migrations, the `lit` (LiteParse) binary, the
> Gemini key, and the Slack bot token are all already deployed/configured
> there. The worker binds `0.0.0.0:34567`, so the dashboard is reachable
> from a browser at `http://enzogo.io.vn:34567/` and curl works over SSH.

---

## 0. Prerequisites — what's already set up on the VPS

| Item | State | Where |
|---|---|---|
| Worker | running, healthy | container `agent-mem-worker-1`, port 34567 |
| Graph schema (5 migrations) | applied | `graph.*` tables |
| `lit` (LiteParse) | `2.0.0` | in the worker image |
| `gemini_api_key` | set | `public.settings` (DB) — embeddings + media describe |
| `api_key` | set | `public.settings` (DB) — Bearer auth |
| `AGENT_MEM_SLACK_BOT_TOKEN` | set (EnzoBot `xoxb-`) | gitignored `.env` → `docker-compose.override.yml` |
| `AGENT_MEM_GRAPH_RUNNER` | `vps` | same |

**Optional, not set (Slack-only test works without them):**
`AGENT_MEM_JIRA_TOKEN` / `AGENT_MEM_GH_TOKEN` / `AGENT_MEM_CF_TOKEN` /
PagerDuty / Datadog / Sentry / GWS. Without these, when the extractor finds
a Jira/PR/CF URL it still creates the *node*, but the `fetch_body` job to
hydrate it will be marked `failed` (cleanly). Slack ingest itself is
unaffected.

To add more source tokens later, append them to
`/var/go/src/github.com/agent-mem/.env` (same env-var names as
`internal/config/config.go`, e.g. `AGENT_MEM_JIRA_TOKEN`) and
`docker compose up -d worker`.

---

## 1. Setup — env + access

```bash
ssh enzo@enzogo.io.vn

# Read the API key from the settings DB (one time):
docker exec agent-mem-postgres-1 psql -U agentmem -d agentmem -t \
  -c "SELECT value FROM public.settings WHERE key='api_key';"

# Then, on the VPS (or wherever you curl from):
export KEY=<that-api-key>
export BASE=http://localhost:34567        # on the VPS
# export BASE=http://enzogo.io.vn:34567   # from your laptop, if port open
```

**Invite EnzoBot into the test channel.** The bot token can only read history
of channels the bot is a member of. In Slack: `/invite @EnzoBot` into the
target channel.

Recommended first target — **`#payments-alerts`** (`C08S954G2LX`): the
Sentry/PagerDuty/Datadog SLO alert channel where EnzoBot ran the TRY-currency
and Tabby incident investigations. Dense with incident threads, partner names,
Jira keys, PR links, and PagerDuty/Datadog URLs — ideal for exercising
cross-source linking.

Other payments channels (for reference):
`#payments-team` = `C05RNSE8TBR`, `#payments-pull-requests` = `C0597404MS6`.

EnzoBot bot scopes required (set in the Slack app → OAuth & Permissions;
reinstall if you add any): `channels:history`, `groups:history`,
`users:read`, `users:read.email`, `usergroups:read`.

---

## 2. INSERT — start a Slack channel backfill

```bash
curl -s -X POST "$BASE/api/graph/backfill/slack" \
  -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  -d '{"channel_id":"C08S954G2LX","months":1}'
```

Expected `202`:

```json
{"job_id":1,"status":"queued","channel_id":"C08S954G2LX",
 "oldest_ts":"1772345678.000000","estimated_months":1}
```

Start with `months:1` for a small first run. Validation: `channel_id` must
match `^C[A-Z0-9]+$`; `months` is 1–24.

Dashboard equivalent: **Backfill** tab → channel id + months → submit →
"View job →" jumps to the Jobs page.

---

## 3. PROCESS — watch the queue drain

```bash
watch -n3 "curl -s -H 'Authorization: Bearer $KEY' \
  '$BASE/api/graph/jobs?limit=30' | python3 -m json.tool"
```

You'll see jobs cascade through these types:

```
backfill_slack_channel     pages conversations.history (200 msgs/page),
                           re-enqueues itself on next_cursor
  └─ backfill_slack_thread  one per threaded parent (conversations.replies)
       └─ ingest each message → graph.nodes + graph.artifact_bodies
            └─ extractor finds URLs/IDs/entities → graph.edges
                 ├─ fetch_body         (Jira/PR/CF URLs — fails w/o those tokens)
                 ├─ describe_attachment (PDFs/images: LiteParse → Gemini fallback)
                 └─ index_artifact      (Gemini summary + embedding)
```

`queue_depth` in the response shows `{queued, running, done, failed}`.
Wait until `queued` and `running` both reach 0.

Dashboard equivalent: **Jobs** tab — auto-refreshes every 5s while anything
is active; click a row to expand its payload JSON; Retry/Delete buttons.

Confirm data landed:

```bash
docker exec agent-mem-postgres-1 psql -U agentmem -d agentmem -t -c \
  "SELECT (SELECT count(*) FROM graph.nodes)  AS nodes,
          (SELECT count(*) FROM graph.edges)  AS edges,
          (SELECT count(*) FROM graph.people) AS people;"
```

Expect nodes/edges > 0 after the queue drains.

---

## 4. READ — query the graph

### Keyword / semantic search

```bash
curl -s -H "Authorization: Bearer $KEY" \
  "$BASE/api/graph/search?q=TRY%20currency&limit=5" | python3 -m json.tool
```

Returns ranked results with `score_breakdown`
(`sem`/`rec`/`edge`/`team`/`auth`).

### Seed → BFS context (the main resolve path)

```bash
curl -s -X POST "$BASE/api/graph/resolve" \
  -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  -d '{
    "seeds": ["jira:PAY-2128"],
    "query": "what is the TRY currency issue?",
    "asker_eeid": 982,
    "depth": 2,
    "budget_tokens": 4000,
    "include_bodies": true
  }'
```

Seeds can be a canonical node id (`jira:PAY-2128`, `slack:C…:ts`) or a raw
URL. Returns artifacts grouped by hop + a `graph_trace`
(`expanded_nodes` / `after_acl` / `took_ms`) + `cache_misses`.

### Direct lookups

```bash
curl -s -H "Authorization: Bearer $KEY" \
  "$BASE/api/graph/node?url=https://wegomushi.atlassian.net/browse/PAY-2128"

curl -s -H "Authorization: Bearer $KEY" \
  "$BASE/api/graph/node/jira:PAY-2128/neighbors?depth=2"
```

Dashboard equivalent: **Graph** tab → Search sub-tab (keyword) and Resolve
sub-tab (paste a seed URL, depth slider, token budget).

---

## 5. Optional — enable person-weighting

Scoring's `team` and `authority` components stay neutral until the people
graph + Slack groups are loaded:

```bash
# Import the org chart (one time):
docker exec agent-mem-worker-1 agent-mem entities import-bamboohr \
  --csv /path/to/bamboohr_org_chart_for_visio.csv

# Refresh Slack usergroups (enqueue the job):
docker exec agent-mem-postgres-1 psql -U agentmem -d agentmem -c \
  "INSERT INTO graph.jobs (type, payload, priority, machine_id)
   VALUES ('refresh_slack_groups', '{}', 5, 'manual');"
```

After both, `resolve` with your `asker_eeid` will boost artifacts authored
by people in your `@payments-geeks` / `@payments-ops` groups and closer to
you in the org tree.

---

## 6. Reset between test runs (optional)

```bash
# Wipe only the graph data (keeps schema + settings):
docker exec agent-mem-postgres-1 psql -U agentmem -d agentmem -c \
  "TRUNCATE graph.nodes, graph.edges, graph.artifact_index,
            graph.artifact_bodies, graph.jobs RESTART IDENTITY CASCADE;"
```

---

## Troubleshooting

| Symptom | Cause / fix |
|---|---|
| `401 unauthorized` | `$KEY` doesn't match `api_key` in settings DB |
| backfill job → `failed`, error mentions `not_in_channel` | EnzoBot isn't invited to that channel — `/invite @EnzoBot` |
| backfill job → `failed`, error mentions `invalid_auth` | Slack bot token wrong/expired; check `.env` + scopes |
| `fetch_body` jobs failing for Jira/PR/CF | those source tokens not set — expected for a Slack-only test |
| `index_artifact` failing | Gemini key issue — check `gemini_api_key` in settings DB |
| `describe_attachment` slow but works | normal — LiteParse for text PDFs, Gemini Vision for image PDFs |
| jobs stuck in `running` | janitor reclaims after lease expiry (~60–120s); check `lease_until` |

Inspect a failed job:

```bash
curl -s -H "Authorization: Bearer $KEY" \
  "$BASE/api/graph/jobs?status=failed&limit=10" | python3 -m json.tool
```

Worker logs:

```bash
docker logs --tail 100 agent-mem-worker-1
```

---

## What this proves when it passes

- **Insert**: a real Slack channel's messages become graph nodes + extracted
  edges, with attachments parsed via LiteParse/Gemini.
- **Process**: the Postgres-backed queue drains under the per-type dispatcher
  + semaphores, with janitor recovery for stuck jobs.
- **Read**: search + BFS resolve return ranked, ACL-filtered, hydrated context
  — the same path EnzoBot uses to answer "what's going on with X?".
