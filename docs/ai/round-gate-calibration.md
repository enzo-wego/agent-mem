# Plan — calibrate the eligibility gate threshold from a labelled day

Written 2026-08-24, to be executed **no earlier than 2026-08-25 12:00 UTC** (24h of `dry_run` data).
**Self-contained: assume no memory of the conversation that produced it.**

Repo `~/go/src/github.com/agent-mem`. Production hub `ssh enzo@payments`
(docker at `/opt/homebrew/bin`, containers `agent-mem-postgres-1`, `agent-mem-worker-1`).

Predecessors: `docs/ai/round-eligibility-gate.md` (`0f6d481`), `docs/ai/round-gate-scoring.md`,
`docs/ai/results-gate-phase0.md`, dedup + thread inheritance (`1e6922f`).
The mechanism is explained end-to-end in `docs/ai/eligibility-gate-walkthrough.html`.

## Decision already taken — do not revisit

Enzo chose, on 2026-08-24: **change nothing in production, label in one day.** No
`enforce`, no threshold edits, no `gated_channels` population. The gate keeps running
in `dry_run` and keeps dropping nothing.

The reason changing thresholds early buys nothing: **`score` is stored on every scored
row**, so any candidate threshold can be evaluated offline against real traffic. The
`decision` column is a pure function of `score` and the thresholds — recomputable, never
worth a production change to observe.

## Goal

Pick `low_threshold` from a **labelled** sample of a full day of real traffic, and report
a false-negative rate that does not rest on 6 examples. Produce a recommendation, not a
config change.

## Non-goals

- **Do not enable `mode: enforce`.** Separate decision, separate round, with the
  distribution in hand.
- **Do not enable `llm_adjudicate`.**
- **Do not change the scoring method**, the embedding model, or `GraphEmbeddingDims`.
  Any change to scoring invalidates the accumulated data for calibration purposes.
- **Do not populate `gated_channels`** here — empty means "all channels", which is what
  maximises calibration coverage while in `dry_run`. Populating it belongs to the enforce
  round.
- Do not set `task_type` on embeddings (`embedding_options.go:13` omits it deliberately;
  OpenRouter rejects it and a vector embedded with one lands in a different space).

## Finding that must shape the analysis: `payments-alerts` contaminates the distribution

Measured 2026-08-24 on the hub, and **this was not known when the 0.596 candidate was
derived**:

| source | scored rows | min | max | avg |
|---|---|---|---|---|
| `payments-alerts` (`C08S954G2LX`) | 94 | 0.5864 | 0.6632 | **0.6149** |
| everything else | 233 | 0.4836 | 0.6644 | 0.5836 |

Three consequences:

1. **29% of the score distribution is machine-generated alert text**, and it clusters
   *above* the 0.596 candidate. A threshold read off the raw distribution is skewed by
   bot output.
2. **Every one of those 94 messages is discarded anyway**, by alert-bot fingerprinting —
   which runs at stage 6, *after* the gate spent an embed at stage 5. All 94 have **no
   node at all**: `LEFT JOIN graph.nodes` on `natural_key = channel_id || ':' || message_ts`
   returns NULL for every one, while the same channel logged 115
   `graph.alert_fingerprint_events` in the same window.
3. Therefore they are also **unlabellable** — there is no body to read, because no node
   and no `graph.artifact_bodies` row was ever written.

**Exclude `C08S954G2LX` from every calibration query.** Filed separately as the reordering
fix (see "Follow-up already filed").

## Data extraction — verified working on the hub

Body lives in `graph.artifact_bodies.body_full`, keyed by `node_id`. There is **no**
`ab.body` column and **no** `graph.channels` table (channel names are in
`graph.slack_channels.name` keyed by `slack_channel_id`). Both cost a failed query when
guessed.

```sql
SELECT d.channel_id,
       d.message_ts,
       round(d.score::numeric, 4) AS score,
       regexp_replace(ab.body_full, '\s+', ' ', 'g') AS body
FROM graph.eligibility_decisions d
JOIN graph.nodes n
  ON n.natural_key = d.channel_id || ':' || d.message_ts
 AND n.type = 'slack'
JOIN graph.artifact_bodies ab
  ON ab.node_id = n.id
WHERE d.decision_source = 'scored'      -- inherited rows carry score = NULL by construction
  AND d.channel_id <> 'C08S954G2LX'     -- alert channel, see above
ORDER BY d.score;
```

`decision_source = 'scored'` is not optional: a paired CHECK constraint guarantees
`inherited_root` rows have `score IS NULL`, so including them injects NULLs.

## Sampling design

Do **not** label everything — a day yields roughly 1,300 scored rows and the labels are
the scarce resource, not the scores. Labels only change the answer near the boundary.

1. Bucket the non-alert scored rows into 8 bands across the observed range with
   `width_bucket(score, 0.48, 0.67, 8)`. For reference, the pre-round shape was:

   | band | range | rows |
   |---|---|---|
   | 1 | 0.4836–0.5031 | 10 |
   | 2 | 0.5126–0.5274 | 23 |
   | 3 | 0.5299–0.5511 | 33 |
   | 4 | 0.5514–0.5725 | 25 |
   | 5 | 0.5791–0.5987 | 58 |
   | 6 | 0.6010–0.6215 | 131 |
   | 7 | 0.6259–0.6383 | 22 |
   | 8 | 0.6473–0.6644 | 24 |

2. Label **all** rows in bands 4–7 (the boundary region, roughly 0.55–0.64) up to a cap of
   30 per band, plus 10 sampled from each of bands 1–3 and 8 as controls. Expect 100–140
   labelled messages. If a band is thinner than its quota, take all of it and say so.

3. **Label on content, never on channel.** This is the specific error that produced a wrong
   conclusion earlier in this work: the lowest score of the first 24 (0.4885) was
   "@minh.do he talk to himself and he start talking nonsense" sitting in
   `payments-x-hotels-devs`. A payments channel is full of non-payments talk.

## The labelling authority — `scope_definition` verbatim

Do not label from intuition. Subscription 1's definition is the contract, and its OUT OF
SCOPE clause already resolves the cases that look marginal:

> **IN SCOPE:** payment method integrations and partners (BNPL providers like Tabby and
> valU, Apple Pay/Google Pay, STC Pay Wallet, GoPayFast, Nium, Ant International, crypto
> payments, loyalty point payments like Mokafaa/Qitaf, Pay by Bank, card instalments,
> VCC/virtual card handling, Juspay orchestration, payment links, network tokenisation and
> saved cards); payment orchestration and gateway integration/back office tooling
> (Orchestrator Integration, Back Office Requirements, Payments UI on Back Office, Payment
> Links via Back Office/Juspay); transaction lifecycle features (delayed auto capture,
> decline codes handling, decline messaging, charge currency optimization, scan card,
> dynamic messaging, payment method selection/experiments); tax and invoicing logic tied
> to payment flows (Tax Engine and Invoicing, country-specific tax logic e.g. EG/PK/IN,
> E2E Invoicing Flow); payments analytics/tracking (Looker dashboards, Genzo tracking);
> restrictions/eligibility rules on payment methods (BNPL usage limits, Tabby basket size
> restrictions); vertical-specific payment requirements (Hotels Pay at Property, VCC for
> Flights); gift card selling.
>
> **OUT OF SCOPE:** general product pricing/discounts unrelated to payment method
> mechanics, marketing/promotions not tied to payment method messaging, **fraud/risk
> systems not directly about payment decisioning or decline handling**, **customer support
> workflows**, non-payment back office features, logistics/fulfillment, and **broader
> supplier/partner contracts unrelated to payment processing**.

The three bolded clauses settle the four messages previously logged as "marginal"
(a fraud-tool investigation at 0.5430, a supplier balance top-up at 0.5451). They are
**out of scope**, so losing them is correct behaviour, not a false negative. Label them
that way.

## Report these, and nothing softer

1. A threshold sweep table over the labelled set: for each candidate, **in-scope lost**
   and **noise dropped**, in counts and percentages.
2. The count of labelled **in-scope** messages underpinning the recommendation. If it is
   below 30, say so and recommend waiting rather than picking a number.
3. The recommended `low_threshold`, with its false-negative rate stated as a count of real
   messages, each quoted.
4. Whether `high_threshold` and `llm_adjudicate` are worth anything at the chosen point,
   or whether a single threshold is sufficient.
5. Set the threshold **asymmetrically in favour of processing**. A false positive wastes
   one LLM call; a false negative loses an observation permanently, because the node is
   never created and nothing recovers it. The boundary belongs *below* a
   balanced-accuracy optimum.

## Acceptance criteria

1. At least 30 labelled in-scope messages, or an explicit recommendation to keep waiting.
2. `payments-alerts` excluded from every calibration query, and the exclusion stated in
   the result.
3. Sweep table with false-negative counts, not just rates.
4. Every recommended false-negative loss quoted verbatim so Enzo can overrule a label.
5. **No production change in this round.** Gate still `enabled: true`, `mode: dry_run`,
   `llm_adjudicate: false`, `gated_channels: []` at the end.
6. Result written to `docs/ai/results-gate-calibration.md`, and the file opened for Enzo
   (`open <path>` **and** SendUserFile — the card posting is not the same as him seeing it).

## Hard constraints

- **NEVER run `internal/graph/handlers` tests against the live or dev database.** They call
  `truncateGraphHandlerTables`, which damages the graph and syncs the damage to production.
  `openTestDB` refuses any DSN whose database name lacks "test". Use `agentmem_test`:
  `postgres://agentmem:agentmem@127.0.0.1:5433/agentmem_test?sslmode=disable`.
- `SELECT key, value FROM settings` prints API keys into the transcript. Select specific
  keys.
- Two handler tests fail on `main` already and are not yours:
  `TestImportBambooHR_CSVBytes_ParsesAndUpserts`, `TestIngestURL_AlreadyFresh`
  (`agent-mem-jvbg`).
- Do **not** use `make deploy` — it still targets the retired VPS. Deploy path is in
  `CLAUDE.md`.
- Migration version ids must be unique across branches. `20260824000001`, `20260824000002`
  and `20260824090040` are taken. A collision once caused a migration to silently never
  run while goose still logged "Migrations applied".
- Merging to `main` and deploying each need explicit approval **in the round that does
  them** — never carried over from a previous round.

## Follow-up already filed

The alert-channel waste is a separate, cheap fix and is **not** part of this round: move
alert-bot fingerprinting (stage 6, one indexed DB read) to run **before** the eligibility
gate (stage 5, one embed). The two are independent — `decideAlertBot` needs only
`channel_id`, body and the bot/subtype flags from the same request — so the reorder is
safe. It would have saved 94 of 327 embeds in the first 6 hours.
