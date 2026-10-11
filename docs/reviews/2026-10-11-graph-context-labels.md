# Graph-context draft-label review

**Verdict: CHANGES REQUIRED**

The owner’s four decisions remain binding. Empty `must` sets are not themselves a defect. The blocking findings below concern inconsistent application of the narrow `must_not` rule, not a request to manufacture positive labels.

## Scope and evidence

- Read Part B, the label changes, replay IDs, node flags, and masked candidate evidence.
- Read every candidate in **case-01, probe-a, probe-b, case-06, case-10, and case-12**. Sampled all remaining cases, concentrating on every existing `must` and `must_not`, their request context, and potential competing candidates.
- Mechanically checked all 20 cases: label sets are disjoint, exclude canonical seeds, have reasons and metadata, and match the replay’s canonical seeds. All remain `fable-draft` with `checked_by: null`.
- All returned seed-thread replies identified by the supplied metadata are `may`. All returned nodes have metadata. No labelled Slack `must` has a timestamp later than its source-thread timestamp.
- This is an offline label review, not an API rerun or a historical-content reconstruction. Candidate excerpts are shortened and masked. Exact identity matches hidden by masking cannot be independently established. Non-Slack creation timestamps are not present in the supplied node metadata.
- Only this review was written; labels and oracles were not changed.

## Blocking findings

### 1. case-01: administrative replies are not harmful incident evidence

**Confidence: high.** In resolve, these nodes are `must_not`:

- `slack:C019B36KGNR:1779443460.574969`
- `slack:C019B36KGNR:1779443938.213489`
- `slack:C019B36KGNR:1779444669.677019`

The visible evidence is administrative conversation, not a competing diagnosis or outcome for the same symptom. Being associated with another incident does not make every message in that conversation harmful. Under owner rule 2 these should be **unlabelled**, not moved to `may` merely to avoid a penalty.

There is also a verifiable problem with the reasons: the metadata places these nodes, the other older replies, and the replies currently described as belonging to the linked conversation under the **same** root, `slack:C019B36KGNR:1779440396.626379`. Thread membership therefore does not establish the asserted distinction. Distinct incidents can share a thread; justify any retained `must_not` by the individual node’s misleading evidence, not by an inaccurate thread distinction.

**Effect:** removing only the three unsupported labels changes replay harmful counts from 8 to 5 for resolve-asker and from 3 to 2 for resolve-asker0. Duplicate-reply counts remain unchanged. This materially affects the reported harmful baseline.

**Required change:** unlabel those three nodes and correct the remaining reasons that rely on the false thread distinction. Recheck the other five harmful labels individually; a different identity alone does not establish a matching symptom.

### 2. case-01: two historical lookalike incidents are exempted as background

**Confidence: high on the label inconsistency; exact identity relies on the draft’s own reasons because the sheet masks identities.** Search marks these as `may`:

- `slack:C09USC3U9A9:1789227442.590279`
- `slack:C05RNSE8TBR:1789024318.585359`

The draft itself identifies them as different historical incidents. Unlike finding 1’s administrative replies, their visible evidence supplies substantive outcomes for closely matching symptoms. Those outcomes can be mistaken for the current incident’s outcome. They satisfy the owner’s narrow confusion test more directly than the administrative replies currently penalized by resolve.

Calling them methodological background does not neutralize their competing incident-specific conclusions.

**Required change:** move both search labels to `must_not`, unless an identity check establishes that they are actually part of the current incident. In that event, correct the reasons instead. With the proposed labels, this replay’s search harmful count becomes 2 rather than 0. Precision remains 0 and recall remains n/a.

## Positive-label audit

No definite post-request `must` was found. The necessity checks below distinguish support from uncertainty rather than treating every related item as required.

| Case | Existing `must` nodes | Assessment |
|---|---|---|
| case-05 | `jira:PAY-2439`; `gh_pr:wego/payments#2421`; `slack:CUV9EAYGY:1791464133.279469` | Direct review target and dependency evidence; defensible narrow positives. The Slack node predates the request; the request and earlier discussion indirectly establish the non-Slack items’ existence. |
| case-06 | `slack:C05RNSE8TBR:1790834828.596929` | Required comparison criteria, predating the request. Keep. Do not promote `slack:C0C6RMYH5U6:1791428761.127289`; its explicit owner-approved `may` remains binding. |
| case-08 | `slack:CUV9EAYGY:1790579932.705169` | Earlier evidence for the same target according to the draft. Timestamp passes. Keep subject to owner verification of the masked identity match. |
| case-09 | `slack:C05RNSE8TBR:1791202861.280829` | Necessary antecedent for the request, predating it. Keep. |
| case-10 | `slack:C0B78BH6810:1791154938.197259` | Direct antecedent for the requested investigation, predating it. Keep. |
| case-13 | `jira:PAY-2409`; `slack:CUV9EAYGY:1790760806.607149`; `gh_pr:wego/payments#2386` | Direct review target and requirement provenance. Earlier discussion supports existence. Check whether both contextual nodes are independently required rather than interchangeable evidence. |
| probe-d | `slack:C05RNSE8TBR:1788444667.445439` | Predates the request, but necessity is less certain: the excerpt establishes related prior analysis, not that the new answer needs this particular artifact. Recommend `may` unless the owner identifies an indispensable fact unavailable in the request or primary data. |
| probe-e | `slack:CUV9EAYGY:1791440402.647959`; `jira:PAY-2434` | Direct decision and requirement provenance. Earlier discussion supports existence. Defensible; confirm they contribute distinct required context. |

Non-Slack timing is supported indirectly, not verified from authoritative creation fields. Do not describe this review as proving historical versions: an older thread can contain later replies or a later-generated summary.

## Harmful-label audit outside case-01

- **case-04:** `slack:C05RNSE8TBR:1789434997.643049` is a plausible competing incident explanation; retaining `must_not` is defensible.
- **case-08:** `slack:CUV9EAYGY:1782304774.725899` is a close historical lookalike with a different cause; retain `must_not`.
- **case-10:** `slack:C0B78BH6810:1789599868.421479` and `slack:C0B78BH6810:1790204595.859309` are competing historical windows; retain `must_not`.
- **probe-c:** `slack:C05RNSE8TBR:1711438647.937599` plausibly supplies a misleading analogous explanation from another incident. Retaining it is defensible, subject to the owner’s same-symptom check.
- **case-03 / case-14:** `slack:C08S954G2LX:1779709917.613979` needs a stronger same-symptom justification than shared channel or answer-shaped details. The supplied request context is abbreviated; I cannot certify the current incident’s symptom from it. Ask the owner to verify, rather than assume all older alerts are harmful.
- **probe-d:** `slack:C0C6RMYH5U6:1791515190.996339` concerns a different mechanism, but a shared broad subject is not sufficient. Recommend checking whether it could actually answer the original question incorrectly; otherwise leave it unlabelled. Do not turn it into `must`.

These qualifications are not additional confirmed blockers. They identify where the available excerpt or request context is insufficient to validate a harmful label confidently.

## Full spot-check outcomes and obvious misses

- **case-01:** findings 1 and 2. No supported new `must` in the supplied candidates.
- **probe-a:** keep the empty `must` sets. Later artifacts, including `jira:PAY-2436`, `gh_pr:wego/payments#2417`, and `slack:C0597404MS6:1791513032.902789`, must not be promoted. No clear overlooked pre-request required artifact in the candidate sheet.
- **probe-b:** keep the empty `must` sets. `jira:PAY-491`, `jira:PAY-482`, and `jira:PAY-473` establish related background, not an independently sufficient required answer. Seed replies correctly remain `may`.
- **case-06:** keep the existing positive and the narrow background labels. No returned search candidate visibly supplies the missing required context; irrelevant retrieval is not automatically harmful retrieval.
- **case-10:** the search positive and two harmful labels are supported. Later resolve artifacts remain `may`. Some reply reasons still describe their parent as `must` even though it is now `may`; update those stale reasons without promoting the replies.
- **case-12:** keep the empty `must` sets. `slack:C05RNSE8TBR:1791207943.700459` and later output nodes cannot become historical requirements. `gh_pr:wego/payments#2380` is useful background, not demonstrably indispensable. No confirmed overlooked `must_not` in the reviewed candidates.

The remaining cases were sampled, not exhaustively cleared for every possible omission. No further high-confidence returned-item promotion emerged from those samples.

## What Part B reports with empty `must`

The 12 cases with no positive on either endpoint are **case-01, case-02, case-03, case-04, case-07, case-11, case-12, case-14, probe-a, probe-b, probe-c, and probe-f**.

For each successful variant of those cases:

- **Recall is n/a**, not 0 or 1, and is excluded from the recall mean.
- **Precision is 0** if any non-`may` item remains in the non-seed top ten; it is **n/a** if that denominator is empty. All-`may` results and successful empty results do not receive perfect precision.
- **Unlabelled@10** still counts unlabelled non-seed items.
- **Harmful** still counts returned `must_not` nodes across the entire result list.
- **Duplicate replies** still counts returned non-seed replies, including replies labelled `may`; `may` does not exempt them from duplication scoring.
- **Unknown metadata** remains independently measurable. Failure is not an empty successful result.

For those 12 cases specifically, each resolve variant has **11 precision n/a cases and one precision 0 case**. Search has **one precision n/a case, probe-a, and 11 precision 0 cases**. All 36 recalls are n/a.

### Offline arithmetic for the supplied replay and unchanged draft labels

Seeds were removed before taking the first ten items. These are calculations over the supplied lists, **not the official first-successful-measured-run baseline**. All 60 stored statuses are 200; the replay does not supply the full measured-run history, instability, latency, or hybrid degradation evidence.

| Metric | resolve-asker | resolve-asker0 | search-hybrid |
|---|---:|---:|---:|
| Recall mean | 0.750 | 0.500 | 1.000 |
| Recall value / n/a cases | 6 / 14 | 6 / 14 | 7 / 13 |
| Precision@10 mean | 0.750 | 0.500 | 0.0938 |
| Precision value / n/a cases | 4 / 16 | 2 / 18 | 19 / 1 |
| Mean harmful count | 0.400 | 0.150 | 0.400 |
| Mean unlabelled@10 | 0.050 | 0.050 | 5.700 |
| Mean duplicate replies | 22.700 | 14.550 | 0.000 |
| Unknown metadata | 0 | 0 | 0 |

The low positive coverage is greater than the headline 12/20 suggests: case-08 and case-10 additionally lack resolve positives, while case-06 lacks search positives.

## Is this still a meaningful eval?

**Yes as a draft diagnostic for harmful retrieval, duplication, unlabelled results, and retention of a small positive set. No as broad evidence of answer-context recall or overall context quality.** Keep the owner’s strict labels; do not inflate `must` to improve coverage.

Recommendations, not changes made by this review:

1. Publish every mean with its value/n/a/failed counts and an explicit **draft labels** banner. In particular, do not present resolve precision as representative of 20 cases when only two or four contribute.
2. Keep no-positive cases as a separate diagnostic stratum. They test over-retrieval and confusion but cannot establish successful recovery of necessary history. `needs_context: yes` does not change the metric denominator or prove an indexed answer exists.
3. Audit the gold pool independently of each endpoint’s current output. Every current `must` appears in its endpoint’s replay pool, and search recall is 1 on all seven contributing cases. That is useful for later regressions but not proof of corpus recall. For example, case-06’s `slack:C05RNSE8TBR:1790834828.596929` is required by resolve but absent from search’s gold; case-10’s `slack:C0B78BH6810:1791154938.197259` has the opposite asymmetry. Document a genuine endpoint-specific reason or include the independently required node even when retrieval misses it. Endpoint-specific label sets are permitted by Part B; the asymmetry is a coverage concern, not automatically a schema violation.
4. Add or substitute independently adjudicated cases with genuinely necessary pre-request non-seed evidence. Preserve the negative/no-history stratum rather than retroactively labelling later solutions as required.
5. Preserve a minimal temporal evidence trail for each positive, including the actual request timestamp when it is a reply. Node creation time alone does not freeze historical thread content.
6. Resolve the owner-confirmation questions above during the required human check. This review does not set `checked_by` or replace that check.

**Approval gate:** correct the two case-01 inconsistencies, retain the coverage caveats, and rerun the arithmetic after owner adjudication. No formula change is required to fix those labels.
