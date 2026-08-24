# Phase 0 result — the current scoring works; the real problem is duplicate scoring

Written 2026-08-24. Offline analysis, **no production change made**. Plan:
`docs/ai/round-gate-scoring.md`. Gate is live in `mode: dry_run` (`0f6d481`).

## Verdict: GO, and **do not build the exemplar scoring**

Phase 0 existed to answer one question — can embedding cosine gate this corpus? **It can.** The
existing method (cosine against the single 1,524-char `scope_definition`) reaches a usable
operating point. Methods B (short exemplars) and C (chunked scope) were **not** run, because A
already clears the bar and building either would be work with no established need.

This reverses my earlier read that "the classes barely separate". That conclusion was wrong twice:
it used channel membership as the label, and it compared channel *averages* instead of sweeping a
threshold. Enzo caught the first error.

## Finding 1 — the gate scores the same message many times

**87 audit rows, but only 36 distinct messages and 32 distinct bodies.** A 2.4x multiplier.

| repeats | message | window |
|---|---|---|
| 13 | `C01SVR5DY9J:1787559780.622649` | 08:23:08 → 08:29:38 |
| 8 | `CDP50BYVD:1787560290.217559` | 08:31:39 → 08:38:09 |
| 6 | `C019B36KGNR:1787558740.529389` | 08:06:08 → **08:06:11** |

Every repeat produced an **identical** score (`count(distinct score) = 1`), so `ingest_content` is
being invoked repeatedly for the same message and the gate re-embeds each time. Two patterns are
visible: tight retry bursts (6 calls in 3 seconds) and slow repeats (13 calls over 6.5 minutes).

This behaviour predates the gate and was previously free — a duplicate ingest hit an idempotent
upsert. The gate turned it into 2.4x the embedding calls.

**Fix, and it is the highest-value item in this round:** short-circuit before the embed when a
decision already exists for `(channel_id, message_ts)`. Same shape as the Phase 1 thread lookup —
one indexed SELECT, no model call. It also removes the skew from calibration data.

Not investigated here: *why* ingest repeats. Worth a separate issue, since it likely wastes work
elsewhere in the pipeline too.

## Finding 2 — a usable threshold exists at ~0.596

31 distinct messages labelled by content: **6 clearly in scope, 21 clearly out, 4 marginal**
(fraud investigation, supplier balance top-up, a bare "find the contract below" in a tax channel).

- in-scope range: **0.5965 – 0.6588**
- out-of-scope range: **0.4885 – 0.6383**

The ranges **overlap** — there is no clean split. But the threshold sweep is favourable:

| threshold | in-scope lost | noise dropped |
|---|---|---|
| 0.5900 | 0/6 | 15/21 = 71% |
| **0.5960** | **0/6** | **18/21 = 86%** |
| 0.6000 | 1/6 | 18/21 = 86% |
| 0.6100 | 2/6 | 19/21 = 90% |
| 0.6200 | 2/6 | 20/21 = 95% |

**0.596 keeps every clearly in-scope message while dropping 86% of the noise.** Precision at that
point is 6/9 = 67%, recall 100%. Given the cost asymmetry — a false positive wastes one LLM call, a
false negative loses an observation permanently — 100% recall at 86% noise reduction is the right
shape of trade.

Note how far this is from the shipped placeholder: `low_threshold: 0.45` sits **below the entire
observed range**, which is why 87 decisions have produced zero skips.

## Two caveats that must gate any threshold change

**1. Six positives is far too thin.** The whole conclusion rests on 6 in-scope messages, and 0.5965
is the lowest of them — a knife-edge. One in-scope message at 0.58 in tomorrow's data would move
the operating point. **Do not set the threshold from this sample.** I would want ~30 in-scope
examples, and because of the 2.4x duplication that needs roughly 2.4x the elapsed time the raw row
count suggests.

**2. The labels are mine and subjective, and one decision is Enzo's, not mine.** All 4 marginal
messages are lost at any threshold above 0.57 — including *"we reviewed the case with the fraud
tool, and it seems that the email was associated with other suspicious activity"* (0.5430) and
*"Can we disable it till top up the balance"* (0.5451). Whether fraud investigation and supplier
balance issues are in scope for payments determines whether those are acceptable losses or
unacceptable false negatives. That is a scope question, and Enzo owns `scope_definition`.

## What the data says about the compression theory

The theory that the 1,524-char scope document compresses scores is **supported but not the blocker**.
The usable range really is narrow — the entire corpus spans 0.4885–0.6588, about 0.17 — and the
operating threshold sits inside a 0.04 window. Wider separation would make the threshold less
fragile.

But the compression is survivable, so widening it is now an *optimisation*, not a prerequisite.
Revisit methods B and C only if the threshold proves unstable as more data arrives.

## Revised recommendation

| priority | item | cost |
|---|---|---|
| 1 | Skip the embed when a decision already exists for `(channel_id, message_ts)` | one SELECT, −58% of gate embeds |
| 2 | Thread replies inherit an eligible root (Phase 1 of the plan, unchanged) | one SELECT, reduces embeds further |
| 3 | Let `dry_run` accumulate until ~30 in-scope samples exist | time only |
| 4 | Then set thresholds, with Enzo ruling on the marginal class first | — |
| — | **Dropped:** exemplar / chunked scoring (methods B and C) | not needed |
| — | File a separate issue for *why* `ingest_content` repeats | — |

Still unchanged and non-negotiable until calibration is done: `mode: dry_run`,
`llm_adjudicate: false`, `gated_channels: []`.
