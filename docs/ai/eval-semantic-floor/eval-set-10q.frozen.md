# Retrieval eval set: 10 questions

> **DRAFT. The expected ids need owner confirmation.** An agent drafted them on
> 2026-10-04/05 from read-only SELECTs against the hub DB (payments Mac mini,
> `agent-mem-postgres-1`). Nobody has signed them off yet. Once they are
> confirmed, freeze this file and change it only through a new dated version.

Source of the questions: `docs/ai/hindsight-vs-agent-mem-payments-entity.md`,
section "Eval set". The query text below is what the client sends as `q`. The
backticks on `tax_country`/`site_code` are dropped, and the parenthetical
hints for Q6, Q7, Q9 and Q10 are not part of the query.

## Scoring rules

- **hit@k**: a question scores a hit at k when any expected id appears in the
  top k. Slack ids are compared at thread level: a result counts when it is the
  expected thread root or any reply in that thread. Thread key =
  `channel:(metadata.thread_ts or ts)`. Every expected Slack id in this file is
  a thread root.
- **recall@10**: the fraction of expected ids found in the top 10, using the
  same thread folding.
- **Alternates**: ids that also answer the question but are not in the core set.
  They count toward the separate `alt` hit columns only. They never count toward
  recall.
- Do not send an `X-Asker-User` header (it filters results).

## Hub state that affects the endpoint choice (observed 2026-10-04)

- `graph.epic_membership` and `graph.epic_briefs` are **empty**, so
  `GET /api/graph/epic/{key}` returns `404 unknown epic` for every key,
  `business:payments` included. Epic and child status was therefore grounded
  from `graph.jira_epic_map` (refreshed 2026-10-04 13:37 UTC).
- Jira `nodes.metadata` is `{}`, so it holds no assignee, status or dates. Status
  comes from `jira_epic_map.issue_status`.
- `artifact_index.identifiers` on a Jira node lists the keys it *references*,
  never its own key. 935 of 2241 Jira nodes have an embedding. None of
  `jira:PAY-2307`, `PAY-2333`, `PAY-2318`, `PAY-2399` or `DECLAW-197` has one.
- Datadog and PagerDuty nodes are stubs with no title and no body. Their content
  lives in the Slack alert messages that `REFERENCES` them.

---

## Q1. What happened on the India GST epic in August 2026?

- **Query**: `What happened on the India GST epic in August 2026?`
- **Fair client**: `/api/graph/epic/PAY-2307` with the brief and the August
  members. That endpoint 404s today, so search (the temporal arm parses
  "August 2026") is the realistic fallback.
- **Expected (8)**:
  - `jira:PAY-2307`: the epic, created 2026-08-11
  - `cf:4115693570`: RFC-58, IN GST on Wego Revenue (Agent Model), 2026-08-15
  - `slack:CUV9EAYGY:1786442428.281449`: epic creation notice, 2026-08-11
  - `slack:C0BBJAHV4G1:1786434184.778929`: Finance tax requirements (state, PAN), 2026-08-11
  - `slack:C08SVNFA30R:1787125337.685409`: determination API announced, 2026-08-19
  - `slack:CUV9EAYGY:1787286012.340719`: pre-grooming check of 7 IN GST tickets, 2026-08-21
  - `jira:PAY-2308`: eligibility and country determination API (DONE, last edited 2026-08-31)
  - `jira:PAY-2315`: write the RFC (DONE in August)
- **Evidence**:
  `SELECT … FROM graph.jira_epic_map WHERE epic_key='PAY-2307'` (31 children
  with created dates). Nodes that `REFERENCES` `jira:PAY-2307` (12 rows). 36
  `graph.thread_summaries` rows matching `GST` with a thread_ts in August.
- **Confidence**: medium. The question is broad: about 36 August GST threads
  qualify, and I picked the milestones (epic, RFC, requirements, API contract,
  grooming).

## Q2. Which PRs closed PAY-2333 and what did the review discuss?

- **Query**: `Which PRs closed PAY-2333 and what did the review discuss?`
- **Fair client**: search, or `/api/graph/node?id=jira:PAY-2333` followed by its
  neighbours.
- **Expected (7)**:
  - `jira:PAY-2333`
  - `gh_pr:wego/payments#2280`: PAY-2344, add tax_country + dual-write (body: "First of five steps in PAY-2333's expand-and-contract rename")
  - `gh_pr:wego/payments#2281`: PAY-2345, backfill + index (step 2)
  - `gh_pr:wego/payments#2282`: PAY-2346, switch reads
  - `gh_pr:wego/payments#2293`: PAY-2349, remaining pkg/tax2 reads
  - `gh_pr:wego/payments#2310`: PAY-2347, stop writing site_code / rename response key ("decided in PAY-2333 grooming")
  - `gh_pr:wego/payments#2314`: PAY-2348, drop site_code ("contract step … under PAY-2333")
- **Alternates** (review-thread Slack posts that reference PAY-2333):
  `slack:C0597404MS6:1787989269.327309`, `…:1788011276.396119`,
  `…:1788738765.729369`, `…:1788774732.210859`.
- **Evidence**: edges into `jira:PAY-2333` (28 rows: 8 PRs, 4 Jira tickets, 15
  Slack posts, 1 CF page). `regexp_matches(body, '…PAY-2333…')` on `gh_pr`
  bodies. `#2276` and `#2359` only cite PAY-2333 for rollout order, so they are
  excluded.
- **Confidence**: medium. Jira has no "closed by" link. "Closed" is read here
  as the TaxCountry PR sequence the ticket split into. `#2357` (PAY-2331,
  contracting-entity V3) may be what actually delivered the determination, but
  its body never names PAY-2333.

## Q3. What is open on PAY-2307 right now?

- **Query**: `What is open on PAY-2307 right now?`
- **Fair client**: `/api/graph/epic/PAY-2307` (open items). It 404s today.
- **Expected (5)**: `jira:PAY-2307` (epic, To Do), `jira:PAY-2331` (In
  Progress), `jira:PAY-2360` (To Do), `jira:PAY-2393` (To Do), `jira:PAY-2400`
  (Review & Testing).
- **Evidence**: `jira_epic_map WHERE epic_key='PAY-2307' AND issue_status NOT IN
  ('DONE','Not Required')`. The map was refreshed 2026-10-04.
- **Confidence**: high, as of 2026-10-04. "Right now" goes stale, so
  re-derive the set from `jira_epic_map` at each run.

## Q4. Who owns the Razorpay checkout UI change and what did they decide?

- **Query**: `Who owns the Razorpay checkout UI change and what did they decide?`
- **Fair client**: search.
- **Expected (7)**:
  - `jira:PAY-2318`: [Razorpay] Update payment UI on Checkout (DONE)
  - `gh_pr:wego/payments-react-component#480`: the implementation PR
  - `slack:C0597404MS6:1787647035.383409`: PR review thread (the reviewer asked for before/after screenshots)
  - `slack:CUV9EAYGY:1786948867.596489`: story created by the PM (removes the method text label)
  - `cf:4107206660`: Design Brief, Razorpay on Checkout
  - `slack:C012A121AQJ:1678158107.892769`: design review thread (thread of reply `…1786703390.895489`, DES-1167 designs sent to the PM)
  - `jira:DES-1167`: the design ticket
- **Evidence**: edges on `jira:PAY-2318` (17 rows) and `thread_summaries` for
  those threads. The ticket was created by the payments PM and implemented by a
  payments engineer (PRs #480 and #491, released in v5.7.7 on 2026-08-31).
- **Confidence**: medium. "Owner" is ambiguous (PM vs engineer), and Jira
  assignee data is empty in the hub.

## Q5. Which incidents (Datadog/PagerDuty) touched the tax path since July?

- **Query**: `Which incidents (Datadog/PagerDuty) touched the tax path since July?` (owner, 2026-10-05: reworded from "tax determination path"; the 7 expected items stand)
- **Fair client**: search (the temporal arm parses "since July").
- **Expected (7)**:
  - `pagerduty:Q00IFF0Q0BGPIV`, `pagerduty:Q2EE3TH7G7IWMR`: Payments - Tax, spike in post-invoice data submission failures (2026-07-16)
  - `pagerduty:Q0NELS47JE95N0`: tax-entity status error (2026-09-03)
  - `datadog:monitor:252312593`: the Payments - Tax monitor
  - `slack:C08S954G2LX:1784236855.699269`, `slack:C08S954G2LX:1784206256.540219`, `slack:C08S954G2LX:1788421850.770589`: the #payments-alerts posts for those incidents
- **Evidence**: Slack nodes with `REFERENCES` edges to `datadog:%`/`pagerduty:%`
  whose body matches `tax|determination|gst|vat` (128 rows, mostly daily digest
  noise). `C08S954G2LX` messages since 2026-07-01 matching `tax`.
- **Confidence**: **low**. No Datadog or PagerDuty incident touches the
  *determination* path itself. All the tax incidents since July are on the
  post-invoice/invoicing path. The set above is the "tax path" reading. The
  only determination-path failure on record is a regression reported by the
  flights team in a Slack thread on 2026-09-08 (`C08SVNFA30R`), and no alert
  fired for it. The owner should decide whether the question should say "tax
  path" or whether the right answer is "none".

## Q6. Show the thread where the refund for a customer was confirmed from the gateway side.

- **Query**: `Show the thread where the refund with scheme reference 74300216267920006001532 was confirmed from the gateway side.` (owner, 2026-10-05: the scheme reference makes this an identifier-recall test; it is a transaction reference, no customer name or contact)
- **Fair client**: search.
- **Expected (1)**: `slack:C0BRJGC92KA:1788682872.344359`. In this
  #payments-support-ops thread, payment ops confirmed the refund "from the
  payment gateway side" and gave the scheme reference.
- **Alternates**: `slack:C0BRJGC92KA:1789606021.066609` (refund shown as processed
  by the gateway, RRN given), `slack:C0BRJGC92KA:1788942795.769759` (two refunds
  confirmed as processed).
- **Evidence**: `thread_summaries` where the overview matches `refund` + `confirm` +
  a gateway name (29 rows). I picked the thread whose wording matches the question.
- **Confidence**: **low**. The question names no identifier, so "identifier
  recall" cannot be tested as written. To make it a real identifier-recall
  test, the owner should add the scheme reference (ARN) or the payment id to
  the query. Customer data is deliberately left out of this file.

## Q7. What did payments discuss last week that has no ticket?

- **Query**: `What did payments discuss last week that has no ticket?`
- **"Last week"** is pinned to **2026-09-28 to 2026-10-04** (owner confirmed 2026-10-05; the scorer passes `since=2026-09-28&until=2026-10-05` instead of relying on the phrase) (the hub's temporal arm
  parsed the same window, starting 2026-09-28 00:00 +07:00).
- **Fair client**: search with the temporal arm. No endpoint today expresses "no ticket".
- **Expected (8, business-level and human-asked)**:
  - `slack:C05RNSE8TBR:1790939932.438779`: Sift 90-92.99 band analysis after the threshold change
  - `slack:C05RNSE8TBR:1790574910.456809`: Mada acceptance rate per partner
  - `slack:C05RNSE8TBR:1790765130.837879`: Tabby KSA presentation, flights vs hotels
  - `slack:CUV9EAYGY:1790593195.088149`: MADA decline reasons via MyFatoorah
  - `slack:C05RNSE8TBR:1790668280.158439`: Checkout transaction count for September
  - `slack:C05RNSE8TBR:1790590105.368949`: Payfort SSL certificate renewal notice
  - `slack:CUV9EAYGY:1790675479.191359`: Tabby UAE Pay-in-3 vs Pay-in-4
  - `slack:C02NA2MA5K5:1790594671.039869`: blank payment-partner field in Backoffice
- **Alternates**: all 49 payments-channel threads that week with no Jira link from
  the root or any reply. The list is regenerated by the query below.
- **Evidence**: `thread_summaries` with thread_ts in the window, in channels named
  `%payment%`, filtered by `NOT EXISTS` on a `REFERENCES` edge from any thread
  node to a `jira` node.
- **Confidence**: **low**. Any of about 49 threads qualifies, so the core 8
  reflect my judgement of what counts as business-level.

## Q8. Which epics were active in the first week of September?

- **Query**: `Which epics were active in the first week of September?`
- **Window**: 2026-09-01 to 2026-09-07.
- **Fair client**: an epic list with activity windows (`epic_membership.last_at`).
  None exists, and `/epic/{key}` 404s. Search is the only path today.
- **Expected (6)**: `jira:PAY-2307` (India Taxation & Invoicing, 44 sources),
  `jira:PAY-1581` (Taxation & Invoicing Engine, 20), `jira:PAY-1983` (ad hoc
  Requests & live issues, 18), `jira:PAY-2203` (Unified ApplePay, 10),
  `jira:PAY-2273` (BackOffice refunds visibility, 7), `jira:PAY-2065` (Tech
  Debts, 6).
- **Evidence**: count of distinct slack/gh_pr/jira/cf/gws_doc nodes created in the
  window that `REFERENCES` a child of each epic in `jira_epic_map`. Cut at 6 or
  more sources. `PAY-2333` (27) is excluded: it shows up as an `epic_key` only
  because it is the parent of the TaxCountry sub-tasks, not an epic.
- **Confidence**: medium. The threshold is a judgement call. `PAY-701`,
  `PAY-2363` and `PAY-2200` had 2 to 4 sources.

## Q9. Was tax_country renamed from site_code, and where?

- **Query**: `Was tax_country renamed from site_code, and where?`
- **Fair client**: search (keyword arm).
- **Expected (8)**: `jira:PAY-2333` (where the rename was decided),
  `gh_pr:wego/payments#2280`, `#2281`, `#2282`, `#2293`, `#2310`, `#2314` (the
  expand-and-contract PRs), `jira:DECLAW-215` (data team's BigQuery
  site_code to tax_country migration).
- **Alternates**: sub-tasks `jira:PAY-2344` to `jira:PAY-2349`,
  `gh_pr:wego/payments#2298` (metrics dimension move).
- **Evidence**: PR titles and bodies. There are 122 `SAME_TOPIC` edges among
  these nodes, including `jira:DECLAW-215 -> jira:PAY-2345/2346/2347/2348` and
  `gh_pr#2280 -> #2281/#2282/#2293`.
- **Confidence**: high.

## Q10. What did the PK VAT CSV work depend on in another team's ticket?

- **Query**: `What did the PK VAT CSV work depend on in another team's ticket?`
- **Fair client**: search, or node + neighbours on `jira:PAY-2399`.
- **Expected (4)**:
  - `jira:DECLAW-197`: data team's ticket, generate the CSV for PK tax test cases (EG/KSA format)
  - `slack:C09H1QMK882:1786709371.372099`: PK VAT go-live thread. The reply that created DECLAW-197 sits in this thread, and it is the only edge into DECLAW-197.
  - `jira:PAY-2399`: payments' PK VAT report CSV query ticket
  - `jira:DECLAW-230`: data team's follow-up, fix the PK VAT CSV from validation feedback
- **Evidence**: edges on `jira:DECLAW-197` (1 row: a reply in the go-live
  thread) and on `jira:PAY-2399` (37 rows, including `REFERENCES` to the
  go-live thread root and `DECLAW-242 -> PAY-2399`).
- **Confidence**: medium. There is **no direct Jira-to-Jira edge** between
  PAY-2399 and DECLAW-197. The cross-project link only exists through the
  Slack reply, so "cross-project edge" in the original note is not literally
  true today.

---

## Re-derivation SQL (read-only)

```sql
-- Q3 open children
SELECT issue_key, issue_status FROM graph.jira_epic_map
 WHERE epic_key='PAY-2307' AND issue_status NOT IN ('DONE','Not Required');

-- Q8 epic activity in a window
SELECT m.epic_key, count(DISTINCT s.id)
  FROM graph.jira_epic_map m
  JOIN graph.edges e ON e.to_node_id='jira:'||m.issue_key
  JOIN graph.nodes s ON s.id=e.from_node_id AND s.type IN ('slack','gh_pr','jira','cf','gws_doc')
 WHERE coalesce(s.created_at,s.first_seen_at) >= '2026-09-01'
   AND coalesce(s.created_at,s.first_seen_at) <  '2026-09-08' AND m.epic_key<>''
 GROUP BY 1 ORDER BY 2 DESC;

-- Q7 no-ticket threads in a window
WITH t AS (
  SELECT ts.channel_id, ts.thread_ts FROM graph.thread_summaries ts
  LEFT JOIN graph.slack_channels c ON c.slack_channel_id=ts.channel_id
  WHERE to_timestamp(ts.thread_ts::float) >= '2026-09-28'
    AND to_timestamp(ts.thread_ts::float) <  '2026-10-05'
    AND c.name ILIKE '%payment%')
SELECT 'slack:'||channel_id||':'||thread_ts FROM t
 WHERE NOT EXISTS (
   SELECT 1 FROM graph.nodes r JOIN graph.edges e ON e.from_node_id=r.id
   JOIN graph.nodes j ON j.id=e.to_node_id AND j.type='jira'
   WHERE r.id LIKE 'slack:'||t.channel_id||':%'
     AND (r.id='slack:'||t.channel_id||':'||t.thread_ts OR r.metadata->>'thread_ts'=t.thread_ts));
```
