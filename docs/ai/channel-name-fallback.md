# Plan — show real channel names on the Continents and Globe pages

**Repo:** `/Users/neocapitelo/go/src/github.com/agent-mem`, branch off `main`

## Goal

The Continents page renders 18 of 54 channels as a bare `C0…` id, and shows
their continent as "Other" regardless of what they are. The names already
exist — the pages drop them. Pass them through.

## The bug

`nameOf` and `continentOf` both take an optional third argument:

```ts
// dashboard/src/continents.ts:6
export function nameOf(channelId, cfg, fallback?) {
  return cfg.names[channelId] || fallback || channelId
}
```

`GET /api/graph/channels` already returns the Slack name as `ch.name`
(`internal/graph/handlers/channels.go:76` LEFT JOINs `graph.slack_channels`).
Three call sites never pass it, so they fall through to the bare channel id:

| Site | Current call | Missing |
|---|---|---|
| `dashboard/src/pages/Continents.tsx:187` | `continentOf(ch.channel_id, { ...cfg, overrides: {} })` | `ch.name` |
| `dashboard/src/pages/Continents.tsx:213` | `nameOf(ch.channel_id, cfg)` | `ch.name` |
| `dashboard/src/pages/Globe.tsx:21` | `continentOf(ch.channel_id, cfg)` | `ch.name` |
| `dashboard/src/pages/Globe.tsx:26` | `nameOf(ch.channel_id, cfg)` | `ch.name` |

`LiveGlobe.tsx:829` and `:1404` already do it right — copy that shape.

The continent one matters as much as the name: `continentOf` matches continent
prefixes against the **name**, so with no name it matches a `C0…` id against
`ext-wego-` / `payments`, hits nothing, and lands on the `*` catch-all. Those
18 rows all display "Auto (Other)" today even when they are Payment Partners.

Measured on prod (2026-08-25): 54 channels have messages, 52 have a name in
`graph.slack_channels`, 18 render as a bare id, and 16 of those 18 are
recoverable from the DB right now.

## Non-goals

- **Do NOT auto-populate `cfg.names` from `ch.name`.** The `names` map in the
  `graph_continents` blob is for manual overrides only. Writing resolved names
  into it would bloat the blob and re-stale it the moment a channel is renamed
  in Slack. The resolved name belongs in the input's *placeholder* (grey), and
  the stored override stays the input's *value*. Keep that split exactly as it
  is.
- No backend / Go changes. The API already returns what is needed.
- No fix for the 2 channels with no name in the DB at all
  (`C0BRPUSCHNC`, `C0BRJGC92KA`). They will still show ids. Separate issue —
  they need a `refresh_slack_channels` run.
- Do not touch `Settings.tsx`. It defines its own local `nameOf` at line 459
  that already reads `channels.find(...)?.name`; it is correct.

## Files expected to change

| File | Change |
|---|---|
| `dashboard/src/pages/Continents.tsx` | pass `ch.name` at the 2 call sites |
| `dashboard/src/pages/Globe.tsx` | pass `ch.name` at the 2 call sites |
| `internal/worker/dashboard/**` | regenerated embed |

Four arguments. If the diff to `dashboard/src` is more than ~6 lines, stop and
re-read this plan — something has gone wider than intended.

## Acceptance criteria

1. On the Continents page, every channel row shows a real name except
   `C0BRPUSCHNC` and `C0BRJGC92KA`. Count of bare-`C0…` rows drops from 18 to 2.
2. The Continent column reclassifies accordingly — channels whose names start
   with `ext-wego-` show `Auto (Payment Partners)`, `payments…` show
   `Auto (Payments Core)`, instead of all showing `Auto (Other)`.
3. The Name input still shows the resolved name as a grey **placeholder**, not
   as a filled value, and `cfg.names` is unchanged after a Save that touches
   nothing else. Saving must not add any new keys to the `names` map.
4. The Notify toggle shipped in `6bc0ae9` still works (tick/untick/Save/reload).
5. Globe page points are labelled with real names.
6. No Go file changed.

## How to verify

```bash
cd dashboard && npx tsc --noEmit && npm run build
cd .. && go build ./... && go test ./internal/graph/handlers/ -run 'Continent|Channel' -count=1
# embed must be regenerated BEFORE committing (repo CLAUDE.md):
rm -rf internal/worker/dashboard/* && cp -R dashboard/dist/* internal/worker/dashboard/
```

Then run the dashboard against the dev API (`npm run dev`) and report, as real
observed output:

- the number of rows still showing a bare `C0…` id (expect 2 locally, or state
  the local dev DB's own number and why it differs from prod's 54/18),
- three example rows with their before/after Continent label,
- the `names` key count in the settings blob before and after pressing Save
  (must be identical — criterion 3).

A green build is not evidence for criteria 1–3. Those need the browser.
