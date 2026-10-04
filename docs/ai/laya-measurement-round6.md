# Round 6: off-the-shelf Laya vs the link_topics judge

2026-10-04 · bd `agent-mem-eu3i` · design: HydraDB Kev Laya Verdict artifact, round 6 step 1.

## Verdict

**Fails the decision rule. Off-the-shelf Laya has no signal on our same-topic question.**
The rule was: proceed if the confident band (p ≥ 0.9 or ≤ 0.1) covers at least 50% of
pairs at ≥ 95% agreement. Coverage is 0%. Ranking quality is at chance (AUC 0.47).

Per the design, the next step is the fine-tune on our own judgments (round 6 step 2).
Round 7 (`decide` route, `laya-serve` container) does not start until a fine-tuned
checkpoint passes the same measurement.

## Setup

- Sample: `scripts/laya-measure/sample.sql`, read-only on the hub. 1,998 rows of
  `graph.topic_link_judgments` judged in the last 35 days, 999 yes / 999 no, seed 0.42.
  The pool had 84,834 judgments (10,075 yes).
- Input: today's `artifact_index.summary` for both nodes plus type and department, the
  same fields the judge prompt carries, minus time windows, shared identifiers and the tag
  rules digest. Median 730 characters per pair, so nothing was truncated at 512 tokens.
- Question: one `noul`, "Are artifact A and artifact B substantively about the same exact
  topic?", true = same concrete issue/incident/feature/change/request, false = same
  general area only.
- Model: `convaiinnovations/laya` (ModernBERT-large), `laya` 0.3.26, MPS on an M4 Pro,
  `batch_size=16`. 260 s for 1,998 pairs (0.13 s/pair). Laya warned at load that the
  checkpoint ships invalid temperatures, so its confidence is uncalibrated.

## Results

| Metric | Value |
|---|---|
| Agreement at p = 0.5 | 49.4% |
| Best agreement at any threshold | 50.1% (t = 0.77) |
| AUC vs haiku verdict | 0.473 |
| Recall on haiku yes / no | 36.0% / 62.8% |
| Confident band ≥ 0.9 / ≤ 0.1: coverage | 0.0% |
| Band ≥ 0.8 / ≤ 0.2: coverage, agreement | 2.5%, 42% |
| Band ≥ 0.7 / ≤ 0.3: coverage, agreement | 14.2%, 42% |

p quantiles (min, 5%, 25%, 50%, 75%, 95%, max): 0.12, 0.25, 0.38, 0.46, 0.55, 0.68, 0.85.
The model hedges around 0.5 for almost every pair.

AUC by pair type: slack–slack 0.53 (n = 1,128), gh_pr–gh_pr 0.45, gh_pr–slack 0.58,
gh_pr–jira 0.63, jira–slack 0.59, jira–jira 0.50. Pairs that include Jira or a PR do a
little better than Slack pairs. Still far from usable.

## Caveats

- Summaries are today's, not the ones the judge read. Restricting to 35 days keeps the
  drift small; it cannot explain an AUC below 0.5.
- The judge sees the tag rules digest, time relation and shared identifiers; Laya saw
  neither. That is the point of an off-the-shelf test, and a fine-tune is how the rules
  get in.
- A second wording of the question was not tried. With AUC at chance, prompt wording is
  unlikely to move the band from 0% to 50%.

## Next

1. Fine-tune (design step 2): JSONL of `{state, questions, label}` from the 84.8k
   judgments, 15% held out, Laya's Kaggle 2×T4 notebook. Re-run this measurement on the
   held-out set with the same script and sample shape.
2. If the fine-tune still misses the rule, stop and keep haiku. Kev-0.8B is the only
   other candidate that fits the hub, and the design rates it weaker.

## Reproduce

```bash
ssh enzo@payments 'PATH=/opt/homebrew/bin:$PATH docker exec -i agent-mem-postgres-1 \
  psql -U agentmem -d agentmem -At -v ON_ERROR_STOP=1' < scripts/laya-measure/sample.sql \
  | tail -n +2 > pairs.jsonl
uv venv laya-venv --python 3.12 && VIRTUAL_ENV=$PWD/laya-venv uv pip install laya
laya-venv/bin/python scripts/laya-measure/measure.py pairs.jsonl out.jsonl
```

`measure.py` calls `predict_batch` with no `batch_size`. On a 24 GB M4 Pro that ran out
of Metal memory over 1,998 pairs, so this run used a wrapper passing `batch_size=16`.
