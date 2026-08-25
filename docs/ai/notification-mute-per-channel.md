# Plan — per-channel notification mute (Continents page toggle)

**bd issue:** `agent-mem-h081`
**Repo:** `/Users/neocapitelo/go/src/github.com/agent-mem`, branch off `main`

## Goal

Let a Slack channel be muted from **DM notifications** through the dashboard,
without dropping its messages from the graph. Concretely: be able to tick
`#payments-pull-requests` (`C0597404MS6`) off in the UI and stop getting DMs
about it.

## Background — the backend knob already exists

Do **not** write new backend code. The mechanism is already shipped:

- `graph_continents` (a JSON blob in the `settings` table) has an `ignore`
  array of Slack channel ids.
- `ignoredChannelIDs()` — `internal/graph/handlers/notify_watch_channels.go:43`
  — reads it.
- Both DM notifiers honour it:
  - `internal/graph/handlers/detect_hot_topics.go:100` (`ignored[h.Channel]`)
  - `internal/graph/handlers/notify_watch_channels.go:174`
- It is served/saved as-is by `GET|PUT /api/graph/continents`
  (`internal/graph/handlers/channels.go:665` and `:687`) — the PUT stores the
  whole body verbatim after a `json.Valid` check.

What is missing is **only the UI**. Today the list can be changed only by
hand-editing the settings row in SQL.

### Do not confuse it with the other ignore list

`graph.channel_filters.ignore` (Settings page, `channel_filters.go`) is an
**ingest** filter: it drops the message entirely, so it never reaches the graph
or the globe. That is not what we want here — PR-channel messages should stay
searchable, they just should not DM.

### A non-issue, so nobody "fixes" it

`ContinentCfg` in `dashboard/src/api.ts:535` does not declare `ignore`. This is
a missing type field, not a data-loss bug: `fetchContinents()` returns the
parsed JSON object and `saveContinents()` re-stringifies that same object, so
the `ignore` key already round-trips at runtime. Adding it to the interface is
part of this change; no migration or repair of existing data is needed.

## Non-goals

- No backend / Go changes. No new endpoint, no migration.
- No new "mute" concept for the globe — an ignored channel still renders on the
  globe and still gets ingested. This toggle is notifications only.
- No management of ignored channel ids that do **not** appear in the channels
  table. The table is driven by `fetchChannels()` (channels that have messages);
  `C0597404MS6` is in it. An orphan id in `ignore` stays in the blob untouched
  and simply is not shown. Fine for now.
- No change to the Settings page ingest-filter UI.

## Files expected to change

| File | Change |
|---|---|
| `dashboard/src/api.ts` | add `ignore?: string[]` to the `ContinentCfg` interface (~line 535) |
| `dashboard/src/pages/Continents.tsx` | add a `Notify` column + toggle handler |
| `internal/worker/dashboard/**` | regenerated embed output (see Build below) |

## Approach

In `dashboard/src/pages/Continents.tsx`:

1. Add a `toggleNotify(channelId: string)` handler next to the existing
   `setName` / `setOverride` handlers. It reads `cfg.ignore ?? []`, adds the id
   when muting and filters it out when unmuting, and calls
   `update({ ...cfg, ignore: next })`. Follow the shape of `setOverride` —
   same immutable-copy style, same `update()` call so the Save button and the
   `saved` flag behave identically. No new save path, no auto-save: the
   existing **Save** button persists it with the rest of the config.
2. Add a `Notify` header cell to the channels table (`<thead>`, after
   `Continent`) and a matching `<td>` per row containing a checkbox:
   - `checked` = **not** in `ignore` (ticked means notifications ON, matching
     the agreed design; unticked = muted).
   - Give the muted rows a visible cue — e.g. render the word `muted` in muted
     grey next to an unchecked box — so the state reads at a glance when
     scanning ~30 rows.
3. Update the page's subtitle paragraph (currently "Configure how Slack
   channels group into continents on the Globe.") to mention that the Notify
   column controls DM notifications, and state explicitly that muting here does
   **not** stop ingestion — that is the Settings page's channel filters. One
   sentence; this is the only place a reader can tell the two lists apart.

Keep it in the existing file and the existing table. No new component, no new
page, no new API call.

## Acceptance criteria

1. The Continents page channels table has a `Notify` column with a working
   checkbox per row.
2. Unticking a row and pressing **Save** results in a `PUT /api/graph/continents`
   whose body contains that channel id in `ignore`; re-ticking removes it.
3. Reloading the page shows the saved state (the checkbox reflects
   `cfg.ignore`).
4. Channels already in `ignore` from the stored config (currently
   `C0B1BR522F5` / payments-staging) render unticked on first load — i.e. the
   existing list is read, not overwritten.
5. Editing a channel **name** or **continent override** and saving still leaves
   `ignore` intact (regression guard on the round-trip).
6. No Go file changed. `git diff --stat` shows only `dashboard/src/**` and the
   regenerated `internal/worker/dashboard/**`.

## Build (required before commit)

The dashboard is embedded in the Go binary, so the embed must be regenerated
**before** committing (per repo `CLAUDE.md`):

```bash
cd dashboard && npm run build
cd .. && rm -rf internal/worker/dashboard/* && cp -R dashboard/dist/* internal/worker/dashboard/
```

## How to verify

```bash
# 1. Type + build check — must be clean
cd dashboard && npx tsc --noEmit && npm run build

# 2. Nothing Go-side regressed
cd .. && go build ./... && go test ./internal/graph/handlers/ -run 'Continent|Channel'

# 3. The embed actually carries the new column (not just dist/)
grep -rc "Notify" internal/worker/dashboard/assets/*.js
```

For criteria 2–5, run the dashboard against the dev API (`npm run dev`), open
the Continents page, untick `payments-pull-requests`, Save, hard-reload, and
confirm the box is still unticked and `payments-staging` is unticked too. Paste
the observed `ignore` array into the report as evidence — a green build is not
evidence that the toggle persists.

## Out of scope for the worker

Do **not** deploy and do **not** touch the production settings row. Muting
`C0597404MS6` on the prod hub is a separate step the conductor gates with the
human.
