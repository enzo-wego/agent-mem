# Plan — self-heal missing channel names, and sort the Continents table A-Z

**Repo:** `/Users/neocapitelo/go/src/github.com/agent-mem`, branch off `main`
**Issues:** `agent-mem-p2ej` (missing names), `agent-mem-q8tm` (the 429 root cause)

Two independent changes, one round. Part B is the reason the user still sees
bare ids after `a59f521`; Part A is a direct request.

---

## Part A — sort the Continents channels table by name A-Z

### Goal

`GET /api/graph/channels` returns `ORDER BY count DESC`
(`internal/graph/handlers/channels.go:80`), so the Continents table is ordered
by message volume. With 54 rows that is hard to scan when looking for one
channel. Sort it alphabetically by the displayed name instead.

### Approach

Sort in the **Continents page only**, at render time. Do NOT change the SQL
`ORDER BY` — the same endpoint feeds `Globe.tsx`, `LiveGlobe.tsx` and
`Settings.tsx`, and the globe's placement and the Settings picker should not be
disturbed by this request.

In `dashboard/src/pages/Continents.tsx`, sort a copy of `channels` before
`.map()`, keyed on the **displayed** name — the same value the row renders,
i.e. `nameOf(ch.channel_id, cfg, ch.name)`. Use `localeCompare` so it is
case-insensitive and stable.

Channels with no name at all (the value falls through to the bare `C0…` id)
sort **last**, grouped together, rather than being interleaved into the
alphabet under "C". A reader scanning for a name should not hit a block of ids
in the middle.

Sort a copy (`[...channels].sort(...)`), never `channels` in place — it is
state.

### Acceptance criteria (A)

1. Continents table rows read A-Z by the name shown in the Name column.
2. The 2 (or fewer) channels with no resolved name appear at the bottom.
3. Renaming a channel via the Name input re-sorts on the next render.
4. `Globe.tsx`, `LiveGlobe.tsx` and `Settings.tsx` are unchanged, and the API's
   `ORDER BY count DESC` is unchanged.

---

## Part B — self-heal names for channels the full refresh never sees

### The problem, measured

`graph.slack_channels` was last refreshed **2026-08-12 19:07:15**. Every
`refresh_slack_channels` run since has exhausted its 5 attempts on
`HTTP 429` (29 failed rows, latest `available_at` 2026-08-25 04:11:08). The job
pulls the whole workspace — `conversations.list?limit=1000`, 2335 channels, 3
pages — and Slack rate-limits it.

So any channel first seen after 2026-08-12 has **no row at all** and renders as
a bare id. Confirmed: `C0BRPUSCHNC` (10 msgs, first 2026-08-19) and
`C0BRJGC92KA` (4 msgs, first 2026-08-20). Both have real bodies and authors, so
ingest can read them; only the name lookup is missing.

### Approach

Add a **targeted backfill** to `internal/graph/handlers/refresh_slack_channels.go`
that runs after the list pass, and **also runs when the list pass fails**:

1. Find channel ids that have nodes but no `graph.slack_channels` row:

   ```sql
   SELECT DISTINCT replace(n.scope,'slack:','')
   FROM graph.nodes n
   LEFT JOIN graph.slack_channels sc
     ON sc.slack_channel_id = replace(n.scope,'slack:','')
   WHERE n.scope LIKE 'slack:%' AND n.scope NOT LIKE 'slack:D%'
     AND n.deleted_at IS NULL AND sc.slack_channel_id IS NULL
   ```

2. For each id, call `conversations.info?channel=<id>` — **one call per unknown
   channel**, not a full list. Upsert `slack_channel_id` + `name` on success.

3. **Cap the batch** (e.g. 20 ids per run) and stop the loop on the first 429,
   so this can never become the rate-limit problem it exists to work around.
   Log how many were resolved, how many were skipped by the cap, and how many
   failed — a silent cap reads as "all done" when it is not.

4. A per-channel failure (`channel_not_found`, `missing_scope` — the bot is not
   in a private channel) is logged at debug/info and skipped. It must not fail
   the job: one invisible channel cannot block the other 19.

This is deliberately not a fix for the 429 itself. `agent-mem-q8tm` stays open
for that. This change means a stale full-list refresh stops producing nameless
channels in the UI, which is the user-visible symptom.

### Acceptance criteria (B)

1. Running the job resolves `C0BRPUSCHNC` and `C0BRJGC92KA` into
   `graph.slack_channels` with real names — **or**, if Slack returns
   `channel_not_found` / `missing_scope` for them, the job logs exactly that per
   id and completes successfully. Report which of the two happened, with the
   literal Slack error. Do not guess.
2. The backfill runs even when the `conversations.list` pass returns an error.
3. The batch cap is enforced and logged, including the number skipped.
4. A single channel's failure does not fail the job.
5. Existing `refresh_slack_channels` behaviour is otherwise unchanged; existing
   tests still pass.

### Test (B)

One table-driven Go test against an `httptest` server standing in for the Slack
API, covering: a successful `conversations.info` upsert, a `channel_not_found`
that is skipped without failing the job, and a 429 that stops the loop early.
No live Slack calls in tests.

**Do not run handler integration tests against the live dev DB** — it truncates
the graph and the fixtures sync to prod. Use the `agentmem_test` scratch DB.

---

## Files expected to change

| File | Part |
|---|---|
| `dashboard/src/pages/Continents.tsx` | A |
| `internal/graph/handlers/refresh_slack_channels.go` | B |
| `internal/graph/handlers/refresh_slack_channels_test.go` (new or extended) | B |
| `internal/worker/dashboard/**` | regenerated embed (A) |

## Non-goals

- Do not fix the 429 / rate-limit strategy. That is `agent-mem-q8tm`.
- Do not change the API's `ORDER BY`.
- Do not add a sort UI (clickable headers, direction toggle). A-Z, fixed.
- Do not touch production settings or deploy.

## How to verify

```bash
cd dashboard && npx tsc --noEmit && npm run build
cd .. && rm -rf internal/worker/dashboard/* && cp -R dashboard/dist/* internal/worker/dashboard/
go build ./... && go test ./internal/graph/handlers/ -count=1
```

Then, as real pasted output:
- the first 5 and last 3 rows of the rendered Continents table, showing A-Z
  order and the unnamed channels last;
- the new test's output;
- for criterion B1, the actual log lines from running the backfill (against the
  dev DB is fine for reading; state clearly which Slack response each of the two
  ids produced).

A green build is not evidence for the ordering or for B1.
