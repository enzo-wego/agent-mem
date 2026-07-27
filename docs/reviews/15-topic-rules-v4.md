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

Pass bar: 9/9, specifically rows 7/8 → same and rows 9/10/11 → different.

## Re-scoring existing data

A version bump does **not** re-score anything by itself: `link_topics` only
runs from `index_artifact` (re-index) or from a popup open, so v4 would spread
over months. No re-embedding is involved either — only judge calls.

Corpus at 2026-07-27: 33,291 cached judgments (29,759 refused / 3,532
confirmed), 2,581 `SAME_TOPIC` edges, 1,081 indexed thread roots. Of the
refusals, 5,716 have activity windows within 7 days — the only ones the new
sibling rule can flip; the other ~24k sit 30–600 days apart and cannot.

Targeting is not worth the machinery: after the version bump every pair
cache-misses anyway, so a full pass ≈ 33k `GenerateCheap` calls
(~$10–15 of Flash, ~1.5h at pool 4 × concurrency 3). Enqueue with plain SQL,
no new code:

```sql
INSERT INTO graph.jobs (type, payload, priority, max_attempts, target_runner)
SELECT 'link_topics', jsonb_build_object('node_id', n.id), 6, 3, 'any'
FROM graph.nodes n
JOIN graph.artifact_index ai ON ai.node_id = n.id
WHERE ai.summary_kind = 'thread_summary' AND n.deleted_at IS NULL;   -- 1081 rows
```

Order of operations: deploy v4 → re-judge this one node → check the popup →
only then the full pass.

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
