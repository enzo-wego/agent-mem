# Plan — fix two gate defects at the Slack ingest chokepoint

Written 2026-08-24. **Self-contained: assume no memory of the conversation that produced it.**

Repo `~/go/src/github.com/agent-mem`, branch off `main`. Two `bd` issues, **one PR**:

- **`agent-mem-hzu8`** (P2) — move alert-bot fingerprinting *before* the eligibility gate.
- **`agent-mem-8nx0`** (P2) — guard an empty message body before it is embedded.

Background on how the gate works end to end: `docs/ai/eligibility-gate-walkthrough.html`.
Tomorrow's separate calibration round: `docs/ai/round-gate-calibration.md` — **do not do that
work here**.

## Why these two are one PR and not two

Both edit `internal/graph/handlers/ingest_content.go` and
`internal/graph/handlers/eligibility_gate.go`. Two workers in one checkout race on the same
branch, so they go to one worker, one branch.

There is also a verification trap. Every empty-body message observed so far is in
`payments-alerts`, and `decideAlertBot` returns `Skip: true` when the fingerprint is empty —
which is exactly what an empty body produces. So **hzu8 alone would hide 8nx0's symptom**: the
reorder would discard those messages before the gate ever saw them, and the logs would go quiet
whether or not the guard actually works. Therefore:

- **8nx0 is verified by test**, not by logs.
- **hzu8 is verified by production logs and row counts.**

## Fix A — `agent-mem-hzu8`: reorder

In `ingest_content.go`, the `req.Source == "slack"` block currently runs, in order:

1. `channelContentSkip` (channel filters)
2. `eligibilityGateSkip` — **one embedding call**
3. `decideAlertBot` — **one indexed DB read**

Move `decideAlertBot` above `eligibilityGateSkip`. That is the whole change.

**Measured justification.** In the gate's first 6 hours live, 94 of 327 scored decisions (29%)
came from `payments-alerts` (`C08S954G2LX`), and all 94 have **no node at all** — the alert
filter discarded them one stage after the gate had already paid for the embed. That same channel
logged 115 `graph.alert_fingerprint_events` in the window.

**Verified safe.** `decideAlertBot` (`alert_policy.go:56`) opens with
`if !automated || !channelIsAlert(...) { return alertBotDecision{} }`. So a human message, or any
channel whose *name* is not alert-shaped, returns `Skip: false` after a single indexed read on
`graph.slack_channels`. Nothing else in it can affect a non-alert message. Net DB reads per
message are unchanged — only the order is.

**Expectation to hold the verification to — do not overstate this.** `payments-alerts` will *not*
disappear from `graph.eligibility_decisions`. `recordAlertFingerprint` returns `escalate = true`
for an unseen fingerprint, so `decideAlertBot` yields `Skip: false` and a **novel** alert template
legitimately reaches the gate and is scored. Only the *repetitive* ones stop.

## Fix B — `agent-mem-8nx0`: empty-body guard

`ingest_content.go:191` hands `req.Body` to `eligibilityGateSkip` with no emptiness check, and
the gate embeds it unconditionally. A Slack message carrying only blocks or attachments has no
text, so we POST an empty string to `/embed`. OpenRouter answers
`400 {"code":"too_small","minimum":1,"path":["input",0]}`, the gateway turns that into 502, and
the gate fails open.

Add the guard **inside `eligibilityGateSkip`**, not at the call site, so every caller is covered:

- If `strings.TrimSpace(body) == ""`, return `(false, nil)` — process the message, no error.
  An empty body is not a failure; it is a message with nothing to judge.
- **Write no audit row.** A `scored` row with a NULL score violates the paired CHECK constraint
  from `20260824090040_eligibility_decision_sources.sql`, and inventing a third
  `decision_source` for this is not worth the schema churn.
- Place it after the `eligibilityGateApplies` check and **before** the dedup lookup, so an empty
  body costs nothing at all.

## Non-goals

- **Do not touch llm-gateway.** The 400-to-502 laundering is real and filed as
  `agent-mem-nsws`, but it is a different repo with its own deploy and its own round.
- **Do not change** `mode`, `high_threshold`, `low_threshold`, `llm_adjudicate`,
  `gated_channels` or `exempt_channels`. The gate stays `enabled: true`, `mode: dry_run`.
- **Do not change the scoring method**, the embedding model, or `GraphEmbeddingDims`. Any change
  to scoring invalidates the data being accumulated for tomorrow's calibration.
- Do not add a hardcoded channel id anywhere. The reorder is general; `C08S954G2LX` appears in
  this document only as evidence.
- Do not fix *why* `ingest_content` is invoked repeatedly for one message. Still open, still not
  this round.

## Acceptance criteria

1. `decideAlertBot` runs before `eligibilityGateSkip` for `source == "slack"`.
2. A whitespace-only or empty body never reaches `/embed`, and no audit row is written for it.
3. The message is still processed in both new paths — fail-open direction preserved.
4. Tests against `agentmem_test` cover: empty body, whitespace-only body, and that a
   non-automated message in an alert-named channel is unaffected by the reorder.
5. No behaviour change for non-automated messages or non-alert-named channels.
6. `go build ./...` and `go vet ./...` clean; gate tests pass.
7. Gate config unchanged at the end of the round.

## How to verify

- **Fix B, by test only** — see the trap above. Logs cannot prove this one.
- **Fix A, after deploy:**
  ```sql
  -- should trend toward 0
  SELECT count(*) FROM graph.eligibility_decisions d
  LEFT JOIN graph.nodes n
    ON n.natural_key = d.channel_id || ':' || d.message_ts AND n.type='slack'
  WHERE d.decision_source='scored' AND n.id IS NULL;
  ```
  And `payments-alerts`' share of new scored rows should fall sharply without reaching zero.
- Strip ANSI before grepping worker logs, or every `field=value` match silently returns nothing:
  `docker logs ... | perl -pe 's/\e\[[0-9;]*m//g'`.

## Hard constraints

- **NEVER run `internal/graph/handlers` tests against the live or dev database.** They call
  `truncateGraphHandlerTables`, which damages the graph and syncs the damage to production.
  `openTestDB` refuses any DSN whose database name lacks "test". Use
  `postgres://agentmem:agentmem@127.0.0.1:5433/agentmem_test?sslmode=disable`.
- Two handler tests already fail on `main` and are **not yours**:
  `TestImportBambooHR_CSVBytes_ParsesAndUpserts`, `TestIngestURL_AlreadyFresh`
  (`agent-mem-jvbg`). Verify against a control worktree at `main` before blaming your diff.
- `SELECT key, value FROM settings` prints API keys into the transcript. Select specific keys.
- No migration is needed for this round. If you think one is, stop and say why — migration
  version ids must be unique across branches (`20260824000001`, `20260824000002`,
  `20260824090040` are taken; a collision once caused a migration to silently never run while
  goose still logged "Migrations applied").
- Do **not** use `make deploy` — it targets the retired VPS. The hub is the payments Mac mini;
  deploy path is in `CLAUDE.md`.
- **Merging to `main` and deploying each need explicit human approval in the round that does
  them.** Open the PR and stop; do not merge, do not deploy.
