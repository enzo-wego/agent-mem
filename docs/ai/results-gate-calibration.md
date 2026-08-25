# Calibration result — do NOT enforce on the current scoring

Written 2026-08-25, 21:45 local. Plan: `docs/ai/round-gate-calibration.md`.
**No production change made.** Gate remains `enabled: true`, `mode: dry_run`,
`llm_adjudicate: false`, `gated_channels: []`.

## Verdict, and it reverses yesterday

**Yesterday's headline — "0/6 false negatives at 0.596, 88% of noise dropped" — does not
survive a larger labelled sample.** At 0.596 the real cost is **2 in-scope messages lost of
20**, and the noise removed is **67%**, not 88%.

Worse, the reason is structural rather than a matter of tuning: **13 of the 20 in-scope
messages score BELOW the highest-scoring out-of-scope message.** The top out-of-scope score
is 0.6499 — an automated "Error Monitor" report. Thirteen genuine payments discussions score
lower than that automated report does.

That is not a sample-size artifact. It is near-total class overlap, and no single threshold
fixes it.

## What was labelled

- **315** scored, non-alert decisions available with 100% body coverage, across 23 channels
  (two days: 2026-08-24 and 2026-08-25).
- **105** sampled systematically (every 3rd row ordered by score) — unbiased across the range.
- Labelled **on content only**. Channel names were deliberately excluded from the labelling
  extract, because judging by channel is the error that produced a wrong conclusion on
  2026-08-24.
- Result: **20 in-scope rows / 85 out-of-scope**. Collapsing duplicate bodies (the repeated-ingest
  issue, `agent-mem-rxy1`) gives roughly **15 unique in-scope / 60 unique out-of-scope**.

In-scope range **0.5791 – 0.6644**. Out-of-scope range **0.4836 – 0.6499**. The overlap
covers almost the whole in-scope class.

## Threshold sweep

A threshold `T` drops every message scoring `<= T`.

| T | in-scope lost | noise dropped | total traffic dropped |
|---|---|---|---|
| **0.5768** | **0 / 20** | 39/85 = **46%** | 37% |
| 0.5951 | 1 / 20 | 57/85 = 67% | 55% |
| **0.5960** (yesterday's pick) | **2 / 20** | 57/85 = **67%** | 55% |
| 0.5991 | 2 / 20 | 62/85 = 73% | 61% |
| 0.6047 | 2 / 20 | 68/85 = 80% | 67% |
| 0.6267 | 5 / 20 | 80/85 = 94% | 81% |

Precision is poor everywhere, because in-scope traffic is only ~19% of the corpus:
at 0.5991, keeping 41 messages yields 18 in-scope — **44% precision, 90% recall**.
At 0.5768: 30% precision, 100% recall.

## The two messages 0.596 would lose, quoted verbatim

Per the plan, every recommended false negative is quoted so you can overrule my label:

1. **0.5791** — *"Activated the PL & the routing is done @U029JDAFQU8 @U08B89MLU9L Please
   monitor"* — payment-link/processing-channel routing going live. Operational payments work.
2. **0.5965** — *"@Supriya that is why those have been highlighted to avoid any discrepancies
   in the invoice number. :slightly_smiling_face: We are actively looking the invoice numbers
   & c…"* — invoice numbering, squarely inside "tax and invoicing logic tied to payment flows".

## Why the zero-false-negative option is not safe either

0.5768 gives 0/20 false negatives — but the lowest in-scope score is 0.5791, so the margin is
**0.0023**. That is a knife edge, on 20 positives. One in-scope message landing marginally
lower is a permanent, silent loss. I would not enforce on a 0.002 margin.

## Recommendation

1. **Do not enable `enforce`.** Not at 0.596, not at 0.5768. The first loses real payments
   discussion; the second is too fragile to trust.
2. **Reopen methods B and C** from `docs/ai/round-gate-scoring.md` — short in-scope exemplars
   (score = max cosine over exemplars) and chunked `scope_definition`. Phase 0 dropped both as
   "not needed" because method A looked sufficient **on 6 positives**. That judgement was made
   on data too thin to support it and should be revisited now that A demonstrably fails.
3. **The compression theory is now supported, not merely plausible.** The whole corpus spans
   0.4836–0.6644 — a range of 0.18 — because a 1,524-character multi-topic scope document
   cannot align sharply with a 60-character Slack message. Widening separation is now the
   prerequisite it was previously assumed not to be.
4. **`dry_run` remains the correct state.** It is costing one embed per message and buying a
   real answer; that is a good trade and should continue.

## Labels I am least confident in

Stated so they can be checked rather than buried:

- **Supplier balance top-ups** (*"Can we disable it till top up the balance"*, 0.5451;
  *"we currently do not have sufficient balance on the AJet"*, 0.5979) — labelled **out**, per
  the scope's "broader supplier/partner contracts unrelated to payment processing". Arguable:
  a supplier balance blocking bookings is adjacent to payment processing. Both sit below every
  candidate threshold, so the label does not change the sweep.
- **Loyalty engine / ledger** (*"ledger is one of the use cases that the loyalty engine solves
  for"*, 0.5833, ×3) — labelled **out**. The scope covers *paying with* loyalty points
  (Mokafaa/Qitaf), not loyalty accounting. If you consider it in scope, 0.596 loses 3 more.
- **"Got it, routing it then"** (0.5389) — labelled **out** for lack of signal. In a payments
  channel "routing" is likely payment routing; context-free it is unlabellable. This is exactly
  the class of short reply that thread inheritance exists to rescue.

## Confirmed working, as a side result

- **The reorder holds over a full day.** Alert-channel scored rows: **107 (Aug 24) → 1 (Aug 25)**.
- **Body coverage went 71% → 100%** (315/315), because the bodyless rows were precisely the
  alert-channel ones the reorder removed.
- **Empty-input fail-opens: none** since deploy.
- Gate config verified unchanged despite four unrelated commits (#43–#46) shipping today.

## What was not done

- No threshold set, no `enforce`, no `gated_channels` populated.
- Methods B and C were not built — reopening them is a recommendation, not work done here.
- 20 in-scope rows is still short of the plan's 30-positive bar. Normally that would mean
  "keep waiting", and more data is still worth collecting. But the finding that matters —
  13 of 20 in-scope messages scoring below an automated error report — does not depend on
  sample size, and waiting will not fix it.
