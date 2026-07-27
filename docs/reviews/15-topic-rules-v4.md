# Topic rules v4 — sibling defect cases + discriminators (2026-07-27)

Trigger: the `/live` timeline popup for `slack:C048WV1BZTK:1784600389.693489`
("Duplicate refund display for payment p9y0yhtbd5") — 4 confirmed same-topic
rows, 5 refused, two of the refusals wrong.

## What was actually wrong

Precision was fine. All three low-cosine refusals were correct:

| row | pair | cosine | v3 verdict | correct? |
|---|---|---|---|---|
| 9 | `slack:CUV9EAYGY:1776409156.492809` (crypto/TripleA wallet overpayment) | .84 | different | yes |
| 10 | `slack:C048WV1BZTK:1783595567.339819` (Voided vs Captured status mismatch) | .82 | different | yes |
| 11 | `slack:C05RNSE8TBR:1776747089.839549` (IN card surge, duplicated transactions) | .79 | different | yes |

Two refusals were false negatives — the same defect on a different payment,
same day, one of them the partner-channel raise:

| row | pair | cosine | v3 `why` |
|---|---|---|---|
| 7 | `slack:C05RNSE8TBR:1785131494.881939` | .86 | "two completely different payment IDs (p0yy6hmqdw vs p9y0yhtbd5) and distinct underlying issues" |
| 8 | `slack:C0736FUE03W:1785132442.506279` | .85 | "two completely different payment/order IDs …; having similar symptoms or involving the same partner/investigator does not make distinct payment issues the same topic" |

Both refused at 0.95 confidence under `bug_incident`. Cause: under v3, a
differing payment id is effectively decisive — `ops_investigation.different_when`
("the subject decides"), `bug_incident.different_when` ("different cause"), the
"same partner or same vocabulary is NEVER sufficient" tie-breaker and the
closing "when uncertain, answer DIFFERENT" all point the same way, while
`bug_incident.same_when` ("same root cause even if affected payments differ")
has nothing to make it win. The judge is also pairwise-blind: it cannot see
that the opened thread is already confirmed against the Juspay refund-dedup
defect (PAY-2255).

Not a defect: the `·82` / `·84` numbers. Cosine over summary embeddings of two
`#payments-*` Juspay-refund threads sits in 0.78–0.88 whether or not they are
the same case — that number cannot be pushed under 80 by tuning. It is a
shortlist nomination; the rules decide.

## v4 changes (`internal/graph/handlers/topic_rules.json`, JSON only)

1. New tie-breaker **sibling cases of one defect**: differing payment/order ids
   are not on their own a DIFFERENT signal. Same symptom on the same object,
   same partner, same flow, windows overlapping or ≤7 days apart ⇒ SAME.
   DIFFERENT requires a *named* distinct mechanism.
2. New tie-breaker **discriminators**, the guard that keeps rows 9/10/11
   refused: (a) transaction type (authorize/capture/refund/void/dispute/payout),
   (b) which object's state is wrong (payment STATUS vs refund RECORD),
   (c) method family (card/wallet/BNPL/bank/crypto). Any mismatch ⇒ DIFFERENT
   regardless of cosine.
3. `bug_incident.different_when` and `ops_investigation.different_when` now
   defer to (1); `how_it_works` gains a step 5 describing the gate.
4. Version 3 → 4. `topicLinkContentHash` includes `rules-v%d`, so every cached
   verdict invalidates and re-judges on the node's next `link_topics` run.

## Validation

`internal/graph/handlers/topic_judge_eval_test.go` +
`testdata/topic_link_golden.json`: 9 hand-labelled pairs from this popup — the
2 flips, the 3 precision anchors, and the 4 v3-confirmed rows as regression
anchors. Opt-in like the search recall eval, read-only (calls
`confirmTopicLink` directly, never writes judgments or edges):

```bash
ssh -N -L 5434:localhost:5433 enzo@enzogo.io.vn &   # VPS postgres
AGENT_MEM_EVAL=1 \
AGENT_MEM_LLM_PROVIDER=google AGENT_MEM_GOOGLE_API_KEYS="$GEMINI_API_KEY" \
DATABASE_URL='postgres://agentmem:agentmem@localhost:5434/agentmem' \
go test ./internal/graph/handlers -run TopicJudgeGolden -v -count=1
```

The judge is a sampled LLM, so the harness votes each pair 3× and takes the
majority (`AGENT_MEM_EVAL_RUNS`). This matters: single runs of v4 scored 7/9,
then 6/9, then 6/9 on identical input. Never compare a fresh run against the
verdicts stored in `graph.topic_link_judgments` — those are single samples too.

Measured 2026-07-27, 3 votes per pair, same 9 pairs:

| | v3 | v4 |
|---|---|---|
| correct | 5/9 | **6/9** |
| false positives | 1 | **0** |
| false negatives | 3 | 3 |

- v4's gain is row 10 (`C048WV1BZTK:1783595567`, Voided-vs-Captured): v3 votes
  2/3 SAME — a false positive the stored verdict hid; v4's discriminators make
  it 0/3.
- v4 does **not** fix the target rows 7/8 — both 0/3 SAME under either version.
  The judge consistently reads them as distinct mechanisms ("display bug from
  data-sync overwrite" vs "refund count discrepancy Juspay↔Worldpay") because
  it sees only the two summaries, each written independently. Rules wording is
  not the lever here; see Next.
- Row 4 (`C08S954G2LX`, refund retry alerts) votes 0/3 SAME under **v3 too** —
  its stored 0.90 confirmed edge is not reproducible. Any re-judge deletes it,
  v4 or not.
- An extra "hypothesis mechanisms don't count as distinct" clause was tried and
  reverted: it dropped the score to 6/9 on single runs and confused the judge.

Reproducibility caveat: judgments stored in production were each decided by one
sample of a cheap model. Two of nine spot-checked here disagree with a 3-vote
majority — roughly consistent with the ~1-in-4 borderline rate the vote splits
suggest. Best-of-3 in the pipeline itself is likely a bigger quality lever than
any rules wording, at 3× the judge cost.

## Re-scoring existing data — quota-aware backfill plan

A version bump does **not** re-score anything by itself: `link_topics` only
runs from `index_artifact` (re-index) or from a popup open, so v4 would spread
over months. No re-embedding is involved either — only judge calls.

Corpus at 2026-07-27: 33,291 cached judgments (29,759 refused / 3,532
confirmed), 2,581 `SAME_TOPIC` edges, 1,081 indexed thread roots. Of the
refusals, 5,716 have activity windows within 7 days — the only ones the new
sibling rule can flip; the other ~24k sit 30–600 days apart and cannot.

Node-level targeting is pointless: 980 of the 1,082 roots touch either a
flip-candidate refusal or a confirmed pair. And after a version bump every pair
cache-misses regardless of whether it *could* flip, so a full pass is ~33k
`GenerateCheap` calls. The only real lever is pacing.

### Why pacing matters more than cost here

Prod config (2026-07-27): `llm_provider=google`, 32 keys in `google_api_keys`,
`llm_key_rotate_hours=6`, graph model `google/gemini-3.6-flash`.

The pool is **not** round-robin per request. `Client.apiKey()` derives ONE key
per 6-hour window from the clock, shared by every goroutine and every process.
A 429 → `blockReason` blocks that key for **24 hours** (`llm_key_blocks`) and
the shrinking live-list moves subsequent calls to another key. Two consequences:

1. A per-minute rate breach is indistinguishable from a daily-quota breach —
   both are 429, both cost a key for a full day.
2. When every key is blocked, `liveKeys()` falls back to the full list, so calls
   keep failing. An unthrottled backfill can burn all 32 keys within a day and
   starve *every other LLM job* — `summarize_thread`, `describe_attachment`,
   hot-topic alerts — until blocks expire. Job priority does not protect them:
   `Claim` orders by `priority ASC` **within a job type**, so each type has its
   own pool; the contended resource is the key pool.

Throughput ceiling: `link_topics` runs `PoolSize 4` × `topicLinkConfirmConcurrency 3`
= 12 judge calls in flight. Measured latency ≈ 11 s/call ⇒ ~65 calls/min
unthrottled — far above any free-tier per-key RPM. Per-key RPM/RPD for
gemini-3.6-flash on this tier is unknown, hence the calibration step below.

### Steps

0. **Deploy v4.** `make deploy` (amd64 built locally → GHCR → VPS pulls). Confirm
   the worker serves the new rules: `curl -s <base>/api/graph/topic-rules | head -c 80`
   should show `"version": 4`. Nothing re-judges yet.

1. **Calibrate (≈30 min, ~600 calls).** Enqueue 20 roots one per minute:

   ```sql
   INSERT INTO graph.jobs (type, payload, priority, max_attempts, target_runner, available_at)
   SELECT 'link_topics', jsonb_build_object('node_id', n.id), 9, 5, 'any',
          NOW() + (row_number() OVER (ORDER BY n.id) * interval '1 minute')
   FROM graph.nodes n
   JOIN graph.artifact_index ai ON ai.node_id = n.id
   WHERE ai.summary_kind = 'thread_summary' AND n.deleted_at IS NULL
   ORDER BY n.id LIMIT 20;
   ```

   Then check, in this order:
   - `SELECT key_tail, reason, blocked_at, expires_at FROM llm_key_blocks ORDER BY blocked_at DESC LIMIT 10;`
     — **any new row means stop and slow down.** Zero rows is the pass bar.
   - `SELECT status, count(*) FROM graph.jobs WHERE type='link_topics' AND created_at > NOW() - interval '1 hour' GROUP BY 1;`
     — `failed` should be 0.
   - `SELECT count(*) FROM graph.topic_link_judgments WHERE judged_at > NOW() - interval '1 hour';`
     — how many pairs 20 roots actually cost. Multiply by 54 for the full pass.

2. **Main pass, paced.** Stagger the remaining ~1,060 roots with `available_at`.
   One root every 3 minutes ⇒ ~2.2 days wall-clock, ~10 judge calls/min average
   (≈30 pairs per root). If step 1 showed any block, use 6–10 minutes instead
   (4–7 days). Same INSERT as above with
   `(row_number() OVER (ORDER BY n.id) * interval '3 minutes')` and no LIMIT,
   plus `AND NOT EXISTS (SELECT 1 FROM graph.jobs j WHERE j.type='link_topics'
   AND j.payload->>'node_id' = n.id AND j.created_at > NOW() - interval '1 day')`
   so the calibration roots aren't redone.

3. **Watch while it runs** (every few hours): the same three queries, plus
   `SELECT count(*) FROM graph.jobs WHERE type='link_topics' AND status='queued';`
   for remaining work. Any `llm_key_blocks` row ⇒ raise the interval on the
   still-queued rows: `UPDATE graph.jobs SET available_at = available_at +
   interval '4 hours' WHERE type='link_topics' AND status='queued';`

4. **Verify.** `SELECT count(*) FROM graph.edges WHERE kind='SAME_TOPIC';`
   (baseline 2,581 before the pass), re-run the golden eval at 3 votes, re-open
   the p9y0yhtbd5 popup, and spot-check ~20 newly created edges' `why`.

5. **Sweep.** Re-enqueue any `failed` link_topics jobs — pairs already judged
   cache-hit under v4, so a second pass costs only what was missed.

Abort / rollback: revert the rules commit and redeploy. Verdicts written under
v4 stay until re-judged, and re-judging back to v3 costs another full pass — so
prefer fixing forward. Nothing is permanently lost either way: `SAME_TOPIC`
edges are derived data, rebuilt from judgments.

Note before spending any of this: v4 buys +1 precision on a 9-pair set and
zero recall. Row 4's confirmed edge disappears under v3 too. It is legitimate
to hold the backfill entirely and let v4 apply lazily as threads are re-indexed
or opened — the mixed-rules window costs nothing but consistency.

## Next — what would actually fix rows 7/8

Rules wording is spent: both rows are 0/3 under v3 and v4. The judge sees two
independently-written summaries and no mechanism string in common. Candidates,
cheapest first, each measurable on the same fixture:

1. **Cluster context in the prompt.** Pass each side's existing confirmed
   same-topic labels ("already confirmed same-topic as: 'Duplicated Juspay
   refund records display and sync bug'"). One query + one prompt line. The
   opened thread has three such partners naming the exact defect; the judge
   currently cannot see any of it.
2. **Best-of-3 voting in the pipeline** for pairs whose first verdict lands
   mid-confidence. Fixes the reproducibility problem measured above, at 3× the
   judge cost on the affected slice — needs the quota headroom from the pacing
   plan above.
3. **Raw-text evidence** for the top candidate pairs (a few messages, not just
   summaries). Most tokens, most likely to bridge differently-worded reports.

## Deferred

- **Lazy load below cosine 0.85** in `neighbors.go`: return SIMILAR rows ≥0.85
  plus a `weak_count`, weak rows only on `?weak=1` (the SHOW toggle refetches).
  Also stops enqueueing up to 12 judge jobs per popup open for rows that come
  back DIFFERENT. Recall-safe: `shortlistTopicLinks` still judges weak pairs in
  the background.
- **Facet extraction** (transaction type / wrong object / method family) into
  `artifact_index` columns with SQL-level candidate gating. Needs a
  re-summarize + re-embed of the corpus; only if v4's prompt-level
  discriminators measurably fall short.
