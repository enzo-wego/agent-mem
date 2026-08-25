# Plan — make `mode: enforce` with an empty channel list impossible

Written 2026-08-25. **Self-contained: assume no memory of the conversation that produced it.**

Repo `~/go/src/github.com/agent-mem`, branch off `main`. Small, single-purpose round.

## Why

`graph.eligibility_gate` currently accepts `mode: "enforce"` together with
`gated_channels: []`. An empty `gated_channels` means **all channels** —
`eligibilityGateApplies` (`internal/graph/handlers/eligibility_gate.go:178`) returns true for
every channel when the list is empty. So one save from the dashboard Settings panel puts
production into "discard traffic across every channel" state.

Measured on 105 labelled messages (`docs/ai/results-gate-calibration.md`), the current
thresholds would discard roughly **56% of real messages**, including genuine payments
discussion. A discarded message never creates a node and **nothing recovers it** — the loss is
permanent and silent.

The gate is deliberately parked in `dry_run` and its scoring is known not to separate the
classes well enough to enforce. This round does not change that. It removes the single-click
path from "parked" to "unrecoverable data loss".

## The change

In `validateEligibilityGateConfig` (`eligibility_gate.go:148`), reject the combination:

- `Mode == eligibilityModeEnforce` **and** `len(GatedChannels) == 0` → error.

Error message must state the reason, not just the rule — something to the effect of:
`enforce requires a non-empty gated_channels list; an empty list means every channel`.

That is the whole production change. Expect roughly 3 lines plus the test.

## Why this lands in two places at once, which is the point

`validateEligibilityGateConfig` is reached from `decodeEligibilityGateConfig`, which is called
by **both**:

1. `putEligibilityGate` (the dashboard `PUT /api/graph/eligibility-gate`) — the save is
   rejected with a 400 and the reason.
2. `loadEligibilityGate` — and note its behaviour on a decode failure
   (`eligibility_gate.go:103`): it returns `nil, nil`, so `eligibilityGateApplies(nil, …)` is
   false and the gate does not run. **A hand-edited database row setting `enforce` + `[]`
   therefore disables the gate rather than enforcing it.**

So the guard covers the API path and the direct-DB path, and both fail in the safe direction.
Do not "improve" the `return nil, nil` on decode failure into an error that blocks ingest —
that would convert a bad config into an outage. Its current fail-safe shape is deliberate.

## Non-goals

- **Do not change** `mode`, `high_threshold`, `low_threshold`, `llm_adjudicate`,
  `gated_channels` or `exempt_channels` in production. The live config stays
  `enabled: true`, `mode: dry_run`, `gated_channels: []` — which remains **valid**, because the
  new rule only bites when `mode == enforce`. No migration and no settings edit are needed.
- **Do not add a config field** (e.g. an `allow_all_channels` override).
  `decodeEligibilityGateConfig` requires *exactly* the supported field set
  (`len(fields) != len(required)` → error), so a new field would break the stored row, the
  dashboard payload and the existing tests. Out of proportion for this round.
- **Do not touch the scoring**, the thresholds, or `loadEligibilityScope`. The scoring is known
  to be inadequate and is tracked separately as `agent-mem-lh9z`.
- Do not delete any part of the gate. That decision is deferred to the outcome of
  `agent-mem-lh9z`.
- Do not change the dashboard UI. A rejected save surfaces the API error; that is sufficient.

## Accepted consequence, stated deliberately

This makes **workspace-wide enforce impossible without a code change**. That is the intent: the
code change becomes the review gate for a state that can destroy data across every channel.
If workspace-wide enforce is ever genuinely wanted, changing this validation is a deliberate,
reviewable act rather than a dashboard click.

## Acceptance criteria

1. `mode: enforce` with `gated_channels: []` is rejected by `validateEligibilityGateConfig`,
   with an error naming the reason.
2. `mode: enforce` with a non-empty `gated_channels` is still accepted.
3. `mode: dry_run` with `gated_channels: []` is still accepted — this is the live production
   config and must not break.
4. A test covers all three cases above.
5. A test asserts that `loadEligibilityGate` on a stored `enforce` + `[]` row yields a nil
   config (gate does not apply), proving the direct-DB path fails safe.
6. `go build ./...` and `go vet ./...` clean; gate tests pass against `agentmem_test`.
7. No production config change in this round.

## Hard constraints

- **NEVER run `internal/graph/handlers` tests against the live or dev database.** They call
  `truncateGraphHandlerTables`, which damages the graph and syncs the damage to production.
  `openTestDB` requires the database name to be exactly `agentmem_test`. Use
  `postgres://agentmem:agentmem@127.0.0.1:5433/agentmem_test?sslmode=disable`.
- Two handler tests already fail on `main` and are **not yours**:
  `TestImportBambooHR_CSVBytes_ParsesAndUpserts`, `TestIngestURL_AlreadyFresh`
  (`agent-mem-jvbg`). Confirm against a control worktree at `main` before attributing any
  failure to your diff.
- No migration is needed. If you think one is, stop and say why.
- `SELECT key, value FROM settings` prints API keys into the transcript. Select specific keys.
- Do **not** use `make deploy` — it targets the retired VPS. The hub is the payments Mac mini;
  the deploy path is in `CLAUDE.md`.
- **Merging to `main` and deploying each need explicit human approval in the round that does
  them.** Open a PR and stop.

---

# Addendum — the dashboard changes (added 2026-08-25 after review of the GUI)

The GUI is **not** broken by the guard: `saveEligibilityGate` (`dashboard/src/api.ts`) already
extracts `err.error` from the response and rethrows it, and `EligibilityGateSection.save()`
(`dashboard/src/pages/Settings.tsx`) renders `e.message` in an error toast. So a rejected save
surfaces the server's reason verbatim without any change.

Three small changes are still warranted.

## GUI-1 — mirror the new rule on the Save button

The component already establishes this idiom for the *other* validation rule:

```tsx
<button disabled={saving || config.low_threshold >= config.high_threshold} …>
```

Extend that condition so `mode === 'enforce'` with an empty `gated_channels` also disables
Save. Follow the existing pattern rather than inventing a new validation mechanism — the server
remains the authority, this is only to stop a pointless round trip.

## GUI-2 — the "Gated channels" hint becomes wrong

Current hint: *"Only these Slack channels are gated. An empty list means all Slack channels
except exemptions."*

That stays true under `dry_run` but is actively misleading under `enforce` once the guard
lands. Amend it to say that an empty list means all channels, **and that `enforce` requires an
explicit list**. Keep it to one or two sentences in the existing voice.

## GUI-3 — say why Save is disabled (fixes a pre-existing gap too)

Today, if `low_threshold >= high_threshold`, the button is disabled with **no visible reason** —
the user gets a dead button and no explanation. Render a short inline message next to or above
the button naming the blocking condition, covering both cases:

- thresholds inverted (`low >= high`)
- `enforce` selected with no gated channels

One line of text per condition is enough. Do not add a modal, a tooltip-only affordance, or a
new component.

## Non-goals for the GUI

- Do not redesign the eligibility panel or restructure `EligibilityGateSection`.
- Do not add client-side validation for rules the server does not enforce — the two must not
  drift.
- Do not change the Exempt channels picker, the channel-fetch logic, or any other Settings
  section.

## Hard constraint — the embedded dashboard MUST be rebuilt before committing

The Go binary serves an embedded copy of the built dashboard. Editing `dashboard/src` alone
ships nothing. From the repo root:

```bash
cd dashboard && npm run build
cd .. && rm -rf internal/worker/dashboard/* && cp -R dashboard/dist/* internal/worker/dashboard/
```

Commit the rebuilt `internal/worker/dashboard/` output together with the source change. A PR
that edits `dashboard/src` without the rebuilt embed output is incomplete and will appear to
work in dev while changing nothing in production.

## Added acceptance criteria

8. Save is disabled when `mode === 'enforce'` and `gated_channels` is empty, and enabled again
   as soon as a channel is added.
9. Save is still disabled when thresholds are inverted (no regression).
10. A visible reason accompanies every disabled state.
11. The Gated channels hint states that `enforce` requires an explicit list.
12. `npm run build` succeeds and the rebuilt embed output is committed in the same PR.
13. A rejected server save still shows the server's own message in the error toast — do not
    swallow it behind a client-side check.
