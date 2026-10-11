# Graph-context relevance labels: round 2

**Verdict: APPROVED.** The round-1 blocking findings are resolved. No new blocking regression or high-confidence missed `must` / `must_not` emerged from the required full spot-checks and remaining samples. Approval concerns the draft labels, not the required subsequent human check or broad retrieval-quality claims.

## Scope and verification

Reviewed Part B, the round-1 findings, current label changes, replay lists, node metadata, and masked candidate evidence. Read every candidate in **case-01, probe-a, probe-b, case-06, case-10, and case-12**; sampled every other case, including all remaining positive and harmful labels. Rechecked the reported fixes against current files; no separate round-1 label snapshot was supplied for an exact revision-to-revision diff.

Read-only mechanical checks across all 20 cases found:

- Disjoint label sets, without duplicate entries or canonical seeds.
- Reasons and metadata for every labelled node; canonical seeds matching the replay.
- Every returned seed-thread reply labelled `may` for its endpoint.
- No missing metadata among returned non-seed nodes.
- Every Slack `must` timestamp earlier than its case's source-thread timestamp. This is a conservative temporal check where the actual request is a later reply.
- No remaining reason claiming a parent is `must` when that parent lacks a positive label.
- All labels remain draft; all human-check fields remain unset.

This is an offline review and arithmetic check, not an API rerun. Shortened, masked excerpts cannot independently establish hidden identity matches. Non-Slack creation times are absent from the metadata; their timing is supported by the request and earlier linked discussion, not authoritative creation fields. Earlier node creation also does not prove that every part of its current summary existed at request time.

## Round-1 finding disposition

### case-01: unsupported harmful labels — resolved

The three administrative nodes are now unlabelled, not merely moved to `may`:

- `slack:C019B36KGNR:1779443460.574969`
- `slack:C019B36KGNR:1779443938.213489`
- `slack:C019B36KGNR:1779444669.677019`

The four retained harmful nodes now have individual reasons identifying competing incident-specific conclusions:

- `slack:C019B36KGNR:1779444518.824219`
- `slack:C019B36KGNR:1779444531.517859`
- `slack:C019B36KGNR:1786686430.689919`
- `slack:C019B36KGNR:1787296531.074889`

Those labels are defensible under the narrow confusion rule. Their justification no longer depends on a supposed difference in thread roots. Multiple incidents can share `slack:C019B36KGNR:1779440396.626379`; shared membership alone neither establishes relevance nor makes administrative messages harmful.

All four returned image nodes are unlabelled, including `slack_file:F0B5JSWBCD8`. No unsupported image penalty remains.

### case-01: historical lookalikes — resolved

Both search nodes moved from `may` to `must_not`, with competing-incident reasons:

- `slack:C09USC3U9A9:1789227442.590279`
- `slack:C05RNSE8TBR:1789024318.585359`

Replay harmful counts are now **4 / 1 / 2** for resolve-asker / resolve-asker0 / search-hybrid. Duplication counts are unchanged. Precision remains 0 and recall n/a for all three variants.

### Other requested fixes — resolved

- **probe-d:** `slack:C05RNSE8TBR:1788444667.445439` is `may` on both endpoints, with a necessity-based reason. Both positive sets are empty.
- **case-02:** the two follow-up reply reasons correctly identify their parent as `may`.
- **case-10:** follow-up implementation replies remain `may`, and their reasons no longer describe the parent as `must`.
- **case-06:** `slack:C05RNSE8TBR:1790834828.596929` is now `must` on both endpoints. Search does not return it, so search recall correctly becomes **0**, not n/a. Resolve-asker recall is 1; resolve-asker0 recall is 0. The explicitly retained `may` on `slack:C0C6RMYH5U6:1791428761.127289` is unchanged.
- **case-10:** `slack:C0B78BH6810:1791154938.197259` is now `must` on both endpoints. Neither resolve variant returns it: both recalls correctly become **0**, while search recall is 1. Resolve precision remains n/a because its top-ten non-seed items are all `may`.

Gold nodes need not appear in an endpoint's returned pool. These cross-endpoint fixes conform to both the schema and the recall formula.

## Necessity, harmfulness, and omissions

The remaining positives in **case-05, case-06, case-08, case-09, case-10, case-13, and probe-e** retain their round-1 justification as direct targets, necessary antecedents, or requirement/dependency evidence. No definite post-request positive was found. The temporal and masked-identity qualifications above still apply.

Full spot-check outcomes:

- **case-01:** the two blocking inconsistencies are resolved; no supported new positive emerged.
- **probe-a / probe-b:** empty positive sets remain appropriate. Later outputs and helpful historical context do not become requirements merely because they are retrievable. Seed replies remain `may`.
- **case-06 / case-10:** the independently required nodes now expose actual retrieval misses. No further high-confidence promotion emerged.
- **case-12:** later output remains `may`; `slack:C05RNSE8TBR:1791207943.700459` cannot become a pre-request requirement. `gh_pr:wego/payments#2380` remains useful background rather than demonstrably indispensable evidence.

Harmful labels outside case-01 show no confirmed new regression. The previously recorded non-blocking human-adjudication questions remain qualifications, not newly elevated blockers: the abbreviated requests in **case-03 / case-14** limit independent certification of `slack:C08S954G2LX:1779709917.613979`; **probe-d** still merits checking whether `slack:C0C6RMYH5U6:1791515190.996339` meets the same-symptom confusion test rather than merely sharing a broad subject. Masking similarly limits identity verification in **case-08**. No additional label changes are required by this review.

## Empty-positive cases and Part B behavior

There are now **13**, not 12, cases with no `must` on either endpoint:

**case-01, case-02, case-03, case-04, case-07, case-11, case-12, case-14, probe-a, probe-b, probe-c, probe-d, probe-f.**

For successful variants of these cases:

- Recall is **n/a**, excluded from the recall mean. All 39 recalls in this stratum are n/a.
- Precision is **0** when any non-`may` item remains in the non-seed top ten; otherwise it is **n/a**, never automatically perfect.
- Unlabelled@10 still counts unlabelled items. Harmful counts cover the entire returned non-seed list.
- Reply duplication is still counted even for `may` replies; unknown metadata remains separately measurable.
- Failed calls are not successful empty results and must be reported separately.

Within this 13-case stratum, each resolve variant has **12 precision n/a cases and one precision 0 case**. Search has **one precision n/a case (probe-a) and 12 precision 0 cases**.

### Recalculated offline replay arithmetic — draft labels

Seeds were removed before selecting the top ten. All 60 stored statuses are 200. These figures describe the supplied replay, not the official first-successful-measured-run baseline; the replay alone does not establish latency, instability, or hybrid degradation across measured runs.

| Metric | resolve-asker | resolve-asker0 | search-hybrid |
|---|---:|---:|---:|
| Recall mean | 0.583333 | 0.500000 | 0.857143 |
| Recall value / n/a cases | 6 / 14 | 6 / 14 | 7 / 13 |
| Precision@10 mean | 0.666667 | 0.500000 | 0.087197 |
| Precision value / n/a cases | 3 / 17 | 2 / 18 | 19 / 1 |
| Mean harmful count | 0.200 | 0.050 | 0.500 |
| Mean unlabelled@10 | 0.050 | 0.100 | 5.700 |
| Mean duplicate replies | 22.700 | 14.550 | 0.000 |
| Unknown metadata | 0 | 0 | 0 |

Case-08 additionally has no resolve positive, explaining the resolve recall coverage of six cases despite seven cases having a positive somewhere.

## Results recommendations — non-blocking

The eval remains meaningful for harmful retrieval, duplication, unlabelled retrieval, and regression detection over a small positive set. It is not broad evidence of answer-context recall or overall context quality.

Keep the strict labels. In Results, report **draft labels**, value/n/a/failed counts beside every mean, and the no-positive stratum separately. In particular, resolve precision averages only three and two cases. Record the cross-endpoint misses rather than removing their gold nodes. Preserve temporal evidence for positives, and expand future positive coverage through independently adjudicated pre-request evidence rather than later answers or current-output-only pooling.

The known coverage caveats and remaining human-check qualifications are not approval blockers. Only this review file was written; labels and oracles were not changed.
