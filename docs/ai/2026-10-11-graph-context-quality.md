# Graph context quality: make agent-mem safe to feed an answering model

Date: 2026-10-11. Owner: Enzo. Status: draft r2 (revised after Fable review r1), needs pi review and Enzo's approval.
REVIEWER: fable (r1, named by Enzo for this round); pi from r2 on (Enzo, 2026-10-11)
Repo: `agent-mem` only. **No p-agent change in this plan.** p-agent work (pass the
real asker, seed every link, frame the context, verify-before-state rules) is a
later plan that starts only after this one meets its gates.

## Goal

When a caller asks agent-mem "what context goes with this request", every artifact
it returns must be (1) one the asker may see, (2) actually related, (3) labelled
with how it is related, how sure we are, and how old it is, (4) not a duplicate,
and (5) marked when it is cut short or written by a bot. When agent-mem is not
sure, it returns less, not filler. A wrong link fed to Claude Code is worse than no
link.

## How this fits the larger safety design

agent-mem is layers 1, 2 and 6 of six. Layers 3 to 5 belong to the later p-agent
plan and are listed only so a reader sees why the fields below exist.

1. Fewer wrong links at creation (Phases 3, 4).
2. Every returned item labelled with method, trust tier, why, age, truncation, bot
   authorship; weak guesses withheld by default; abstain when nothing is strong
   (Phases 1, 2, 3).
3. (p-agent) Prompt has two blocks: "Linked (from real references)" and "Possibly
   related (AI guess, check before use)", framed as leads, never facts.
4. (p-agent) Session rule: any status, number or owner is checked against the live
   source before it is stated; live source wins on conflict.
5. (p-agent) Answer marks graph-only claims as "related, not verified".
6. Feedback: a bad link reported by a person moves to `graph.edges_rejected` and the
   case joins the eval set (Phase 0 harness, Phase 4 table).

## Why now (evidence, measured 2026-10-11 on the hub)

Live A/B in #enzo-private: the same teammate request answered with a graph context
pack gave the right owner, precedent tickets and a paste-ready reply; without it, a
log dump. The context is worth having. What agent-mem returns today:

| # | Finding | Evidence | Harm |
|---|---|---|---|
| F1 | A real asker loses every Jira / GitHub / Confluence / GWS node. `graph.member_scopes` holds only `slack:*` (2,962 rows, 348 eeids); its only writer is `refresh_slack_members.go:245`; `aclVisibleSQL` (`handlers/acl_guard.go:78-86`) admits only NULL / '' / `public` / listed scopes | 2,274 `jira`, 2,471 `github`, 209 `confluence`, 27 `gws`, 54 `jira:PAY` nodes invisible to every eeid. Resolve on `jira:PAY-1731`: 7 artifacts for asker 0, 2 for Enzo's real eeid | Highest: the ticket asked about is silently missing |
| F2 | Three inputs mean "trusted, unfiltered" or "public only" today: absent `X-Asker-User` (`acl_guard.go:21`), `asker_eeid: 0` in a resolve body (`resolve.go:282`), present-but-unknown header (public only, `acl_guard.go:24`). MCP sends no asker (`graphmcp/server.go:144-184`). p-agent sends `asker_eeid: 0` whenever its lookup fails (`p-agent/src/graph-context/index.js:19`), which is every call, because `p-agent/src/graph-context/asker.js:20` uses `/node?id=person:slack:<uid>` and there are 0 `person:*` nodes | 78 people have a Slack id and no eeid | Security: private content can reach a requester who may not see it |
| F3 | Resolve returns reply nodes as separate artifacts; the root's `body_full` already holds the thread | 88% of slack nodes are replies. Probe C: 22 of 27 artifacts were replies of the seed; probe E: 62 of 65 | Budget on duplicates; real neighbours cut |
| F4 | `Sem` hard-coded 0 (`resolve.go:566`); `query` ignored; edge kind and confidence not scored; no score threshold | weights in `scoring/combine.go:50` (Sem 0.50) | A wrong recent neighbour outranks a right older one |
| F5 | EnzoBot replies are graph content with no label. `people.id=10`, `is_bot=false`, 249 live authored nodes (`deleted_at IS NULL`, all types); 62 REFERS_TO edges come from its replies; 1,379 SAME_TOPIC edges touch its nodes | probe A seed body: 12 bot references in 16 replies | A wrong earlier bot answer returns as "context" |
| F6 | SAME_TOPIC is a Gemini judgement (`deps.Gemini.Generate`, `link_topics.go:783`). Human-judged strict precision 76% (19/25). Wrong ones carry confidence 0.82-0.92 and a `why` that invents shared tickets. No floor at insert; stored min 0.40 | 828 edges < 0.8; 1,103 (9%) span 30+ days; 57 alert-to-alert edges tagged "same Sentry error" in one alert channel | Plausible but false history |
| F7 | `summary` and `via` empty on every resolve artifact; `artifact_index` has summaries for 4,338 roots; 869 of 5,207 roots have no `artifact_index` row | 6 of 6 probes | Raw bodies only; more tokens, more noise |
| F8 | Provenance gaps: REFERENCES carries no `metadata.method` (0 of 33,081); REFERS_TO carries `method='slack-permalink'` on all 531 (`link_topics.go:685-686`) but `source_msg_id` is NULL on 531 of 531, so the message that made it cannot be found. REFERENCES `source_msg_id` is set (it is the from-node id, used by `pruneStaleEdges`, `fetch_body.go:440-456`). "Why linked" only on hop-1 artifacts; hop-2 get `links: []` | | A structural link cannot be told from a guess, or traced when wrong |
| F9 | `lookupAsker` (`search.go:728-745`) matches Slack id / email / GitHub / Jira only; a numeric eeid header returns 0 results | reproduced | Callers give up and use asker 0 |
| F10 | Truncation is silent: `hydrate.Greedy` cuts the best candidate when nothing fits (`hydrate/budget.go:40-45`) | | A cut thread looks complete |
| F11 | Slack scopes older than 24 h are dropped (`acl/scopes.go:60-62`); a stalled member refresh empties every asker's Slack view with no signal | | Context silently disappears |
| F12 | Bodies are raw Slack / Jira / GitHub text, unsanitised (no redact/mask outside `postgres.go`, `settings_handlers.go`) | | Prompt injection and PII reach the model |

Precision by edge kind (human-judged random samples): REFERENCES ~100% (12/12
judgeable), REFERS_TO ~100% mechanically (7/7), PART_OF ~100% (9/9), SAME_TOPIC 76%.
Structural links are trustworthy; the LLM link is the risk.

Facts the worker needs: embedding model is `gemini-embedding-001` via the LLM
gateway (`internal/llmgateway/client.go:381`); `artifact_index.embedding` is
`halfvec(3072)`. `GET /api/graph/slack-user?id=<uid>` already returns `eeid`
(`handlers/slack_user_api.go:43-56`) and p-agent's client already wraps it
(`p-agent/src/graph-context/client.js:71-74`), so the p-agent fix later is one line
in `asker.js`; no third lookup is to be built.

## Non-goals

- Any p-agent change.
- New sources or fetchers.
- Changing who may see a Slack channel (Slack membership stays the rule for Slack).
- Rewriting stored bodies to strip instruction-like text (lossy; the risk is
  handled by marking content untrusted here and framing it in p-agent).
- Public blockchain lookups or any other external call from agent-mem.

## Use cases and expected behaviour

Request types from 150 EnzoBot mentions over the last 30 days (7 users, 14
channels). Each is a row type in the Phase 0 eval set.

| UC | Request (masked) | Seeds the caller sends | Must return | Must NOT return |
|---|---|---|---|---|
| UC1 | Forwarded / linked thread: "check DB and Juspay logs for the refs in this thread" | thread URL + linked thread URL | linked thread root (summary + body), its REFERS_TO / REFERENCES targets, earlier bot answers labelled as bot | reply nodes as items; threads that share only a symptom |
| UC2 | Jira question: "trace PAY-1731 translated message" | `jira:PAY-1731` + thread URL | the ticket (title, status, summary), its PRs, epic via PART_OF, threads that reference it | the ticket missing for scope reasons |
| UC3 | PR review request: "review payments#2424" | PR URL | the PR, its Jira key, threads that discuss it | 60 reply nodes of the review thread |
| UC4 | Payment / order ref lookup | thread URL; refs in `query` | threads mentioning the same ref (exact identifier), dated | same partner, other refs, unless `inferred-low` and asked for |
| UC5 | How does X work: "Tabby pay-in-3 vs 4" | thread URL; text in `query` | Jira / PR / Confluence on the topic ranked by `Sem` | recent unrelated threads ranked by recency |
| UC6 | Incident follow-up: Mada PagerDuty, Sentry spike | alert thread URL | earlier incidents with the same incident id or error fingerprint, dated, with outcome | same-symptom alerts from another incident |
| UC7 | Data / report question | thread URL; text in `query` | earlier analyses, dated | old numbers without a date |
| UC8 | Ownership / go-live | thread URL; text in `query` | Jira epic, assignees, recent threads | |
| UC9 | Question with no link and no match | thread URL | the seed only, `abstained: true`, reason | filler from recency |
| UC10 | Asker who may not see a source channel | any | nothing from that channel, not even a title | private content via a trusted fallback |

## Response contract (new fields, all additive)

Per response: `abstained` (bool), `abstain_reason`, `ranking` (`full` | `degraded`),
`content_is_untrusted: true` (always), `scopes_stale` (bool, true when the asker's
Slack scopes were dropped by the 24 h rule).

Per artifact:
- `relation`: list of hops from a seed; each hop `kind`, `method`
  (`url-extract` | `attachment` | `gh-pr-key` | `slack-permalink` | `jira-epic-link` |
  `llm-same-topic` | `thread`), and for LLM hops `confidence`, `tag`, `why`.
- `trust`: `structural` (all hops deterministic) | `inferred-high` (one LLM hop,
  confidence >= 0.9 and a shared identifier verified in both nodes'
  `artifact_index.identifiers`) | `inferred-low` (anything else, and anything
  authored by a bot).
- `summary`, `created_at`, `last_activity_at`, `age_days`.
- `truncated` (bool) and `body_chars_full` when the body was cut.
- `authored_by_bot` (bool); bot messages inside the body prefixed
  `[bot answer, unverified]`.
- `links` stays for one release beside `relation`.

## Phases

One worker round per phase, dispatched only after the previous phase meets its gate.
Phase 0 is the gate for all others. Phase 1 is **blocked on Q1**.

### Phase 0: eval set and harness

- `testdata/eval/graph_context/*.yaml`: 20 cases covering UC1 to UC10 (at least one
  per UC), from real EnzoBot threads of the last 30 days. Each case: asker Slack id,
  seeds, query, `must` node ids, `may` node ids, `must_not` node ids, a one-line
  reason per label. No customer PII in notes; node ids are fine.
- Labelling is human-judged: a Fable agent drafts labels, Enzo checks at least 5
  cases. The link judge is Gemini, so a Fable labeller is not grading itself.
- `cmd/agent-mem/eval.go`, subcommand `agent-mem eval context --base-url <url>`;
  the API key is read from `.env` (`AGENT_MEM_API_KEY`), never from argv.
  Per case and in total it prints:
  - **precision@10** = |returned top 10 ∩ must| / |returned top 10 \ may|
    (`may` items are neither right nor wrong: excluded from the denominator);
  - **recall** = |returned ∩ must| / |must|, for resolve and, separately, for
    `/search?match=hybrid` on the case query;
  - **harmful** = |returned ∩ must_not|;
  - duplicate-reply count; ACL-dropped seeds; `scopes_stale` cases (own counter,
    not counted as ACL drops); bot-authored artifacts without the label;
    truncated artifacts without `truncated: true`;
  - latency p50 / p95 from **10 runs per case** (200 samples).
- `agent-mem eval edges --kind SAME_TOPIC --n 50 --seed <n>`: prints pairs with
  both bodies for human judging; results go into this file per round.
- Record today's baseline in "Results" before Phase 1 starts.

Gate: harness runs on the hub in under 5 min; baseline recorded; Enzo has checked
5 labelled cases.

### Phase 1: access control that is correct for real askers (blocked on Q1)

- `graph.org_scopes` table (migration, config not code): source scopes every active
  employee with an eeid may see. `aclVisibleSQL` admits a node when its scope is in
  the asker's member scopes **or** in `org_scopes`. Slack stays per channel.
- Asker inputs, each with one defined behaviour:

  | Input | Today | After |
  |---|---|---|
  | `X-Asker-User` absent, no `trusted` | unfiltered | `403 asker_required` |
  | resolve body `asker_eeid: 0` or missing, no `asker_slack_id`, no `trusted` | unfiltered | `403 asker_required` |
  | `X-Asker-User` present but unknown | public only | `403 asker_unknown` |
  | `X-Asker-User` = Slack id / email / GitHub / Jira / **numeric eeid** | filtered (eeid: 0 results) | filtered |
  | body `asker_slack_id` | n/a | resolved like the header |
  | `trusted: true` (body) or `X-Asker-Trusted: 1` | n/a | unfiltered, logged with route and caller |

- Alias `GET /api/graph/node?id=person:slack:<uid>` to the slack-user lookup, so
  p-agent's current call resolves without a p-agent change.
- MCP (`internal/graphmcp/client.go`): read `AGENT_MEM_ASKER` (Slack id) from env and
  send it; with none set send `X-Asker-Trusted: 1` and log a warning once, so Enzo's
  own sessions keep working.
- Dashboard (`dashboard/src/api.ts:467,528,553,845,1244,1315`, `authFetch`
  `api.ts:11-19`): send `X-Asker-Trusted: 1`. Rebuild the embedded dashboard per
  CLAUDE.md ("Dashboard changes still require the embed rebuild").
- `/neighbors`, `/node`, `/search`, cards use the same filter path as resolve.
- Response `scopes_stale` set when the 24 h rule dropped the asker's Slack scopes;
  log a warning when more than 10% of askers in an hour are stale.

Compatibility: for the 78 eeid-less people, p-agent still sends `asker_eeid: 0`
(the alias returns no eeid for them) and now gets a 403. p-agent's `try/catch` turns
that into an empty graph block and logs it. That is accepted: no context beats
unfiltered context. For everyone else the alias gives p-agent a real eeid.

Gate: 0 ACL-dropped seeds for an asker who can see the seed's source; UC10 returns
nothing from the hidden channel; probes A, C, E get their Jira / PR seeds back;
every row of the asker table above has a test; dashboard search, resolve, node
and neighbors pages still render against the hub.

### Phase 2: distinct, ranked, explained artifacts

- Collapse threads: one artifact per thread root; a reply seed maps to its root and
  is kept as `focus_ts`; reply nodes are never emitted. Budget counts each root once.
- `Sem`: when `query` is set, embed it with `gemini-embedding-001` (same as the
  index) and use cosine vs `artifact_index.embedding`. Over 800 ms or an error:
  `Sem` = 0, `ranking: "degraded"`, continue.
- Score the link: add trust tier to the score (`structural` > `inferred-high` >
  `inferred-low`).
- Fill `summary` from `artifact_index` (thread_summary first, then gemini) and
  `relation`, `trust`, ages, `truncated` per the response contract. `relation` is
  filled for every hop, not only hop 1.
- `min_trust` request field, default `inferred-high`; `inferred-low` only when asked.
- Abstain when no non-seed artifact passes `min_trust`.
- PII masking at hydrate for non-trusted callers, as decided in Q2 (default if Q2
  is unanswered: mask emails, phone numbers, card-like numbers; keep payment and
  booking refs). Masking runs on `summary` and body alike.

Expected effect, stated so nobody reads it as a bug: with the default `min_trust`,
most SAME_TOPIC neighbours without a verified shared identifier disappear, so many
requests return the seed plus structural links only. That is intended.

Gate: duplicate-reply count 0; precision@10 >= 0.8; harmful = 0; recall of `must`
>= 0.8 (resolve); UC9 abstains; 0 truncated artifacts without the flag; resolve
p95 <= 500 ms without query and <= 1.2 s with query (10 runs per case).

### Phase 3: bot answers labelled, not hidden

The earlier bot answer was among the most useful lines in the A/B test, so it
stays, marked.

- Do **not** flip `people.is_bot` for EnzoBot: it is read by `alert_policy.go:166-176`
  (an alert thread answered only by `is_bot` people counts as "no human cared"),
  `thread_cards.go:57`, `pins.go:83`, `person.go:164`, and changing it would change
  alert handling. Instead add `graph.answer_bots` (config table: person id, Slack
  id), seeded with EnzoBot (`people.id=10`, `U0AG1DJP9K9`). Only this plan's code
  reads it.
- Labelling is done at read time, no backfill: in `hydrate/budget.go` (around
  `:50`), compose the body of a thread root from its reply nodes ordered by ts, and
  prefix messages whose author is in `answer_bots` with `[bot answer, unverified]`.
  If reply nodes are missing for a root, fall back to `body_full` and set
  `authored_by_bot` from the root author only. The worker confirms in step 1 that
  reply nodes carry their own body and author; if they do not, stop and report
  (do not invent a backfill).
- Artifacts whose root author is in `answer_bots`: `authored_by_bot: true`, trust
  capped at `inferred-low`.
- `materializeThreadReferences` skips source messages authored by `answer_bots`; a
  one-off migration moves the 62 existing such REFERS_TO edges to
  `graph.edges_rejected` with reason `bot-source`.
- New REFERS_TO edges store `source_msg_id` (the message that held the permalink).
- REFERS_TO is pruned with its source: when `pruneStaleEdges` (`fetch_body.go:440`)
  removes a REFERENCES edge that produced a REFERS_TO edge, that edge goes too.
- SAME_TOPIC candidates where either root is authored by `answer_bots` are not created.

Gate: 0 bot-authored artifacts without the label; alert-policy tests unchanged and
green; REFERS_TO human-judged precision on a fresh sample of 20 = 100% of judgeable.

### Phase 4: SAME_TOPIC hygiene

New rules are **post-filters with no LLM call**, applied after the judge and kept
outside the judgement cache hash, so existing cached verdicts stay valid:
- shared-identifier verdicts: every id in `shared_ids` must occur in both nodes'
  `artifact_index.identifiers`;
- skip roots under 200 characters with no identifier (OOO, holiday notices);
- confidence floor 0.8 at insert, every tag;
- alert-to-alert pairs (both roots in an alert channel): same incident id or error
  fingerprint required;
- gap over 30 days: shared identifier required.

Re-check job (`agent-mem relink sweep` or similar, worker names it): runs the post-
filters over the existing 12,084 edges; failures move to `graph.edges_rejected`
(edge row + reason + rule version + timestamp) instead of calling
`deleteSameTopicEdge` (`link_topics.go:205`). No LLM cost, because nothing is
re-judged. Only candidate-generation changes (the short-root skip) affect future
judging. `graph.edges_rejected` also takes manual rejections (a person reports a
bad link: `POST /api/graph/edges/reject` with edge id and reason, trusted callers
only), and resolve never returns a rejected edge.

Gate: human-judged strict SAME_TOPIC precision >= 90% on a fresh sample of 50
(Fable labels, Enzo checks 10); eval precision@10 does not drop; rejected counts
reported per reason.

### Phase 5: index coverage and provenance

- Backfill `artifact_index` for the 869 unindexed roots.
- Remove reply nodes from the semantic and keyword index.
- REFERENCES at ingest: store `metadata.method` (`url-extract` | `attachment` |
  `gh-pr-key`).

Gate: 0 unindexed roots older than 1 h; hybrid-search recall of `must` does not drop.

## Files expected to change

- `internal/.../handlers/acl_guard.go`, `handlers/resolve.go`, `handlers/search.go` (`lookupAsker`), `handlers/search_arms.go`, neighbors / node / cards handlers, `handlers/slack_user_api.go` (alias)
- `internal/graph/acl/scopes.go`; migrations for `graph.org_scopes`, `graph.answer_bots`, `graph.edges_rejected`
- `internal/graph/scoring/combine.go`, `bfs/expand.go`, `hydrate/budget.go`
- `link_topics.go`, `topic_rules.json` (post-filters only), `fetch_body.go`
- `internal/graph/extractor/extractor.go`, `handlers/ingest_content.go`
- `internal/graphmcp/client.go`, `server.go`
- `dashboard/src/api.ts` + embed rebuild
- `cmd/agent-mem/eval.go`, `testdata/eval/graph_context/`
- The worker confirms exact paths; findings above cite file:line.

## Verification per round

1. `go test ./...` green; dashboard build green when touched.
2. `agent-mem eval context` against a local stack: gate met; numbers pasted into
   "Results".
3. Deploy per CLAUDE.md (payments Mac mini, `docker compose up -d --build worker`),
   confirm the running binary carries the change, rerun the eval against the hub.
4. Re-run probes A to F and the A/B thread seed; paste a before / after table.

## Open questions for Enzo

- Q1 (blocks Phase 1). Which source scopes are org-wide, visible to every employee
  with an eeid? Proposal: `jira` and `jira:PAY` (projects the Jira fetcher reads),
  `github` (repos the GitHub fetcher reads), `confluence` (spaces the fetcher
  reads); `gws` stays per file. Every Wego employee, or payments only?
- Q2. PII in bodies: agent-mem masks emails, phone numbers and card-like numbers at
  hydrate for non-trusted callers, and keeps payment / booking refs because they
  are the lookup keys the bot needs; the bot's own masking directive governs what
  reaches Slack. Accept, or mask booking refs too?
- Q3. Keep the trusted mode for the dashboard and your own sessions (proposed), or
  require an asker everywhere?

## Results

### Phase 0 baseline (2026-10-11 21:44-21:49 SGT, hub, main `ddf7a92`)

Draft labels (`fable-draft`, not human-checked), so every relevance number is
provisional. Command: `go run ./cmd/agent-mem eval relevance --base-url http://localhost:34567`,
one warm-up plus 10 measured runs per case and variant; ranking from the first
measured run. All 600 measured calls succeeded; no case was unstable or degraded.

| | resolve, asker 0 | resolve, real asker | hybrid search |
|---|---|---|---|
| precision@10 (n / n/a) | 0.500 (2 / 18) | 0.667 (3 / 17) | 0.087 (19 / 1) |
| recall (n / n/a) | 0.500 (6 / 14) | 0.583 (6 / 14) | 0.857 (7 / 13) |
| unlabelled@10, mean | 0.10 | 0.05 | 5.80 |
| harmful (must_not returned), mean | 0.05 | 0.20 | 0.45 |
| duplicate replies, mean | 14.55 | 22.40 | 0 |
| unknown_meta (nodes) | 1 | 2 | 4 |
| rows (primary / linked) | 372 / 0 | 525 / 0 | 200 / 24 |
| latency p50 / p95 (200 samples) | 24 / 94 ms | 23 / 98 ms | 1,024 / 2,244 ms |

What the numbers say:

- Resolve is mostly reply copies: a mean of 14.6 (asker 0) to 22.4 (real asker)
  returned items are replies of a thread already returned. Phase 2 targets this.
- Search is mostly noise: about 5.8 of its top 10 non-seed items are unlabelled
  (judged irrelevant) and precision@10 is 0.087. Where a needed item exists it
  usually finds it (recall 0.857), but only 7 cases score recall.
- Harmful items appear: search returns a same-symptom, different-incident item
  0.45 times per case on average, resolve with a real asker 0.20. This is the
  wrong-link risk behind this plan.
- Known misses: case-06 (search does not return the spec thread) and case-10
  (resolve does not return the linked Sentry report) score recall 0.
- `unknown_meta` counts nodes returned now but absent from `node_meta.json`
  (ingested after `eval prepare`). Run `eval prepare --refresh` before the next
  comparison.

Coverage caveats (pi, `docs/reviews/2026-10-11-graph-context-labels-r2.md`):

- Labels are judged as of the original request. 13 of 20 cases have no `must` on
  either endpoint, so recall is n/a there and resolve precision rests on 2-3 cases.
  Those 13 cases measure over-retrieval and confusion only.
- Labels come from what the endpoints returned (plus two cross-endpoint `must`),
  so recall is recall of that pool, not of the whole corpus.
- Next eval work: Enzo's human check, then cases with real pre-request evidence.
  Open label questions for that check: case-03/case-14
  `slack:C08S954G2LX:1779709917.613979`, probe-d
  `slack:C0C6RMYH5U6:1791515190.996339`, the identity match in case-08.
