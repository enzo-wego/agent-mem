# R1a: lead mode for graph reads (agent-mem prerequisites for EnzoBot graph context)

Date: 2026-10-12. Owner: Enzo. Status: draft r3 (r2 after pi r1, flag per Enzo 14:35 SGT;
r3 after pi r2 `docs/reviews/2026-10-12-graph-context-r1a-lead-fields-r2.md`: F1, F5, F8,
F9, F10).
REVIEWER: pi (auto: GPT weekly 59% left)
PAGE: https://claude.ai/artifact/GU71ZW2m97bUKieoSs5vYp
STATUS: approved (Enzo, 2026-10-12 14:35 SGT; after build: merge, deploy, setting on and test in #enzo-private)
Repo: agent-mem only. Base: `main` (origin head `8e97d74`, #98). Branch
`feat/graph-lead-mode`, worktree `/Users/enzo/go/src/github.com/agent-mem-wt-lead`.
Umbrella design: `p-agent/docs/ai/2026-10-12-graph-context-in-bot.md`. Parent quality plan:
`docs/ai/2026-10-11-graph-context-quality.md`. Follow-ups with their own plans: R1b (new
eval labels aimed at wrong items), R1c (p-agent detector test).

## Goal

Add an opt-in **lead mode** to `resolve`, `node` and `search` (default and hybrid) that
returns short, attributable, access-checked items for EnzoBot, and a no-trace switch for
eval/detector runs. With the flag off, or without the opt-in header, every route behaves
exactly as on `main`.

## Flag and opt-in

- Worker env `GRAPH_LEAD_MODE_ENABLED`, default `false`, read at startup into the handler
  config (`internal/worker/server.go` wiring). Documented in `.env.example` and README.
- Lead mode runs for a request only when the env flag is `true` **and** the request has
  header `X-Graph-Lead: 1`. Otherwise the request takes today's code path unchanged.
- **The gate is decided before the body is decoded (pi r2 F9).** The flag reaches the
  handlers through `Deps` (`handlers.go:35`) and the constructors in `router.go` (e.g.
  `NewResolve` at :94). Inactive requests use today's `resolveRequest` decoder, untouched:
  no new fields are added to that struct. Active resolve requests are decoded a second time
  into a separate `leadResolveRequest` that adds `max_items *int` (nil = 30; explicit 0,
  negative, over 100 or wrong type = 400 `bad_max_items`). No lead-only query runs and no
  lead field is emitted on an inactive request.
- With the flag off, a request carrying `X-Graph-Lead: 1` gets today's response plus
  response header `X-Graph-Lead: off` (so a caller can tell).
- Existing callers (p-agent Home tab and identity lookup, dashboard, MCP, eval) send no
  `X-Graph-Lead` header, so they see no change. A test proves this for each route.
- The no-trace switch (A1) is not behind the flag: it only reacts to
  `X-Graph-No-Side-Effects`, which only the eval sends today, and it removes writes.

## Rollout stages (Enzo, 2026-10-12 14:30 SGT: off, some channels, all)

Lead mode is per request (`X-Graph-Lead: 1`) and only p-agent sends that header, so the
agent-mem setting never turns on a channel by itself. Channel stages live in p-agent:

| Stage | agent-mem `GRAPH_LEAD_MODE_ENABLED` | p-agent | Who gets graph context |
|---|---|---|---|
| 1 Off | off at deploy; on later with Enzo's go | `GRAPH_CONTEXT_ENABLED=false` | nobody |
| 2 Some channels | on | flag on, allowlist #enzo-private, then channels Enzo adds | listed channels (and 1:1 DMs if added) |
| 3 All | on | allowlist mode "all internal channels" (not Slack Connect, not group DMs) | everyone |

Each step needs Enzo's go. Rollback: p-agent flag off (no restart) plus the revoke script;
the agent-mem setting off makes every lead request answer as today with `X-Graph-Lead: off`.
Stage 3 drops the explicit allowlist (Q2); whether guest-bearing channels are acceptable is
decided by Enzo at that point.

## Non-goals

- Trust tiers, `min_trust`, abstain, semantic `Sem`, SAME_TOPIC hygiene, edge rejection.
- Marking bot messages inside generated summaries with `[bot answer, unverified]` (umbrella
  P3 text label). Deferred: summaries are generated elsewhere and cached. Lead mode gives
  `bot_in_thread` instead; the umbrella P3 stays partly open and the p-agent rounds must
  treat `bot_in_thread=yes` as "summary may quote a bot".
- Changing `people.is_bot`, alert policy, MCP input types (`max_items`, `body=0` are not
  exposed via MCP in this round).
- Any p-agent change; any label change.

## Facts (main `8e97d74`; pi r1 verified the line references)

- `resolve.go`: request :26-39; artifact :48-62 (`Summary`, `Via` never set, literal
  :376-382); visited ids authorized :271-299; candidates sorted :305-309 (equal scores keep
  map order); `hydrate.Greedy` :316 returns `missed`, which feeds `cache_misses` and the
  `fetch_body` enqueue :356, :432-443; author join :320-345 (display name, hydrated rows
  only); score/hop lookup by original id :359-369; bodies blanked :372-375; seed fallback
  :397-430 (thread summary used as Title); `seedLinks` :695-802 queries edges between final
  ids :717-721, keeps best per kind (`linkPick.better` :681), synthetic THREAD :774-776;
  `CanonicalizeSeeds` :503-558 maps reply seeds to an existing non-deleted root :544-553.
- `hydrate/budget.go` `Greedy` :37-123 (drops non-file NULL bodies :72-91, skips oversize
  :94-102, truncates the top oversize when nothing fits :114-120). `TruncateRunes`
  :126-140 is a byte cap despite its name.
- Search: default path does not canonicalize threads; hybrid canonicalizes in
  `canonicalizeThreads` (`search_hybrid_arms.go:134-178`, promotes to root only when the
  root passes `searchFilter.sql`, else keeps the member). Hybrid output is built in
  `search_hybrid_display.go:14-44,60-219` and replaces Slack `summary` with
  `thread_summaries.overview` at :184-186. Default results use `searchResult`
  (`search.go:65-82`).
- `node.go`: response :27-38 (`Summary` = `artifact_index.summary`, `Body`, `AuthorName`,
  edges); header asker :48; not trace-wrapped (router.go:86).
- Masking: `maskGraphResponse` (`mask.go:26`) wraps node, search, resolve, epic, neighbors;
  `redact.Mask(s string) string` (`internal/redact/redact.go:67`) is the string masker.
- ACL: `aclVisibleSQL(alias, scopeParam, resolvedParam)` (`acl_guard.go:292`).
- Tracing: `withContextTrace(maskGraphResponse(...))` on search (router.go:92) and resolve
  (:95); `traceHTTP.ServeHTTP` (`context_trace.go:50-93`) sets `X-Graph-Request-Id`, inserts
  `request_id` into the JSON body, and always calls `h.recorder.Enqueue` (:92).
- Eval: `eval relevance` prints the request URL and body (`cmd/agent-mem/eval_relevance.go:177-178`);
  all eval requests send `X-Graph-No-Side-Effects: 1` (`eval_common.go:95`).
- Thread activity precedent: `channels.go:549-556` (one channel, `max(updated_at)` grouped by
  thread key); no index on the thread-key expression.
- Callers: p-agent Home resolve without `include_bodies` (`p-agent/src/channels/slack/socket.js:8128`)
  and hybrid search (:8146); dashboard resolve (`dashboard/src/api.ts:535`) needs a hop-0
  artifact (`GraphSearchPage.tsx:698-705`); MCP `internal/graphmcp/server.go:115-145`.

## Changes

### A1. No-trace switch (always on)

- Shared helper `noSideEffects(r *http.Request, bodyFlag bool) bool` = today's rule at
  resolve.go:436-444; resolve uses it.
- In `traceHTTP.ServeHTTP`, when the header alone says no-side-effects (`1` or
  case-insensitive `true`), skip only the `Enqueue` call. Request id header and JSON
  `request_id` stay as today. A body-only flag does not stop the trace (the wrapper does
  not parse bodies); README says so.
- `eval relevance` gets `--quiet` (pi r2 F8): no request URL/body echo (:177-178); failed
  runs are recorded and printed as a category only (`transport`, `http_<status>`,
  `decode`) instead of the error string (today `FailedRuns` holds raw errors,
  :208-209, printed at :390-393, and transport errors are `*url.Error` with the full URL);
  the early unreachable-server error (:188) and every error returned to the CLI in quiet
  mode is a fixed message plus category. Non-quiet output is unchanged. The conductor uses
  `--quiet`.

### A2a. One eligibility rule for every thread-derived value (pi r2 F1)

A Slack item's **root** is eligible when: the root node exists, `deleted_at IS NULL`, it
passes `aclVisibleSQL` for this asker, and (resolve only) it passes the request's
epic/business filter. Every thread-derived value comes only from an eligible root:
`lead_summary` from `thread_summaries`, the thread-summary title fallback, root author
fields, `last_activity_at` and `bot_in_thread`. In lead mode, on every route (resolve, node,
default search, hybrid search), a Slack reply whose root is not eligible is **omitted**
(node: 404 as for a hidden node). A root lookup error is 503 `lead_lookup_failed`, kept
separate from a denial. Non-Slack items use only their own node's values.

### A2. Lead pipeline (resolve), in this order

1. **Authorize** original candidates as today (:271-299).
2. **Seeds**: before `CanonicalizeSeeds`, remember each reply seed's own ts as its
   `focus_ts`.
3. **Map** every Slack candidate to its root `slack:<channel>:<thread_ts>` (thread_ts from
   metadata, else own ts).
4. **Authorize roots**: one query for all new root ids: the root row exists, is not deleted,
   passes `aclVisibleSQL` for this asker and the request's epic/business filter. A root that
   fails: its whole group is dropped (no fallback to the reply, no borrowed author). Query
   error: 503 `lead_lookup_failed` (fail closed).
5. **Group** per root: members = authorized candidates mapped to it; score/breakdown/hop =
   the highest-scoring member (tie: lowest node id); a seed root is hop 0 and always kept;
   `focus_ts` = the reply seed's ts if any, else the best-scoring reply member's ts, else
   empty.
6. **Select**: order by score desc, node id asc. Seeds first and always included; then
   others up to `max_items` (request field; default 30; values 1-100; 0, negative or over
   100 gives 400 `bad_max_items`). If seeds alone exceed `max_items`, only seeds return.
   `include_bodies` is ignored in lead mode: no bodies are ever returned.
7. **Misses (pi r2 F5)**: one query over every selected item (Slack roots and non-Slack
   items): `SELECT n.id FROM unnest($1::text[]) AS n(id) WHERE NOT EXISTS (SELECT 1 FROM
   graph.artifact_bodies b WHERE b.node_id = n.id)` (the body cache is
   `graph.artifact_bodies`, not a `graph.nodes` column). Result goes to `cache_misses` and is
   enqueued as today unless `noSideEffects`. Query error: 503 `lead_lookup_failed`.
8. **Links**: query stored edges between authorized seed members and authorized members of
   the selected groups (both original endpoints authorized), map both endpoints to roots,
   drop self-links (from == to), keep the best tuple per kind with `linkPick.better`, and
   emit `from`, `to` (roots), plus today's kind/tag/topic/why/confidence. No THREAD links in
   lead mode (replies are folded into roots).
9. **Enrich** (A4) after selection, then return. `context_tokens` in lead mode =
   ceil((len(title)+len(lead_summary)) / 4) summed, documented as a lead-mode meaning.

### A3. Lead mode on node and search

- `node`: lead mode never returns `body` and returns no `edges_in`/`edges_out`; adds A4
  fields for the requested id. A reply id stays a reply id (no remapping); thread-derived
  values follow A2a (ineligible root: 404).
- `search` default and hybrid: enrichment runs on the final page after the limit, in both
  output paths (default `searchResult` and `search_hybrid_display.go`). Slack rows whose
  root fails A2a are dropped in both modes (this includes hybrid's member-fallback rows). The existing `summary`
  and `overview` keep their meaning but are masked and capped like A4; the new
  `lead_summary` field is added.

### A4. Lead fields (resolve, node, search in lead mode)

| Field | Type / values | Value |
|---|---|---|
| `lead_summary` | string ≤ 600 runes incl. `…` | `artifact_index.summary` for every node type (Slack roots: the root's own, topic plus overview); empty if none |
| `last_activity_at` | RFC3339 UTC, always present | Slack: max(`updated_at`) over the thread's message nodes (A5); others: `nodes.updated_at` |
| `author_slack_id` | string, `""` when unknown | root (or node) author `people.slack_user_id` |
| `authored_by_bot` | `bot` / `human` / `unknown` | `bot` if author is in `graph.answer_bots` or `people.is_bot`; `unknown` if no author person |
| `bot_in_thread` | `yes` / `no` / `unknown` (Slack only) | any live node in the thread (root or reply) authored by `answer_bots` or `is_bot` |
| `channel_id` | string or empty | Slack and Slack-file: `REPLACE(scope,'slack:','')` |
| `focus_ts` | string or empty | resolve only, A2.5 |

Masking and caps: for non-admin callers the handler runs `redact.Mask` on the full selected
summary text first, then cuts to at most 600 runes including a trailing `…` (new
`capRunes` helper, rune-based). Admin callers get the same cut without masking. The outer
`maskGraphResponse` stays. Enrichment errors in lead mode fail the request with 503
`lead_lookup_failed`.

### A5. Thread aggregate query and index

- One query per response, after selection: input = the selected eligible Slack roots as
  `(scope, thread_ts)` pairs via `unnest($1::text[], $2::text[])`, joined to `graph.nodes`
  on `scope` and the thread-key expression, `deleted_at IS NULL`, **Slack message node
  types only** (`type = 'slack'`; the worker checks whether any other message type such as
  `'slack_thread'` exists in migrations/ingest and lists the final set in Findings), and
  `aclVisibleSQL` for the asker; returns per pair `max(updated_at)` and
  `bool_or(author is bot)`. Files and other node types never contribute. Uses the request context; rows are closed
  before the next query.
- New expression index in its own migration file (one index per file):
  `CREATE INDEX CONCURRENTLY IF NOT EXISTS graph_nodes_thread_key_idx ON graph.nodes (scope,
  (COALESCE(NULLIF(metadata->>'thread_ts',''), split_part(id,':',3)))) WHERE deleted_at IS NULL`,
  `-- +goose NO TRANSACTION`, down drops it concurrently. Created with
  `make migrate-create name=graph_nodes_thread_key_idx`.

### A6. Bot list

- Separate migration `make migrate-create name=graph_answer_bots`: table
  `graph.answer_bots(slack_user_id TEXT PRIMARY KEY, note TEXT, created_at TIMESTAMPTZ DEFAULT now())`,
  seeded `('U0AG1DJP9K9','EnzoBot')`. Read only by lead mode.

## Implementation notes from pi r3 (APPROVED)

- Read the request body once into bytes; decode the legacy `resolveRequest` from those bytes
  as today, and on the active path decode `leadResolveRequest` from the same bytes. Never
  decode twice from the consumed `r.Body`. Inactive decoding stays untouched.
- Compatibility normalisation applies only to traced routes (resolve, search): remove the
  generated `request_id` and normalise `took_ms`. Node, epic and neighbors are not traced:
  compare their bodies byte-exact and assert they carry no request id. Do not add tracing to
  routes that have none.

## Files expected to change

`internal/graph/handlers/{resolve.go,node.go,search.go,search_hybrid_display.go,context_trace.go}`,
new `internal/graph/handlers/lead.go` (pipeline helpers, enrichment, `capRunes`),
`internal/worker/server.go` (flag wiring), `cmd/agent-mem/eval_relevance.go` (`--quiet`),
two migrations, tests under `internal/graph/handlers/` and `cmd/agent-mem/`,
`README.md`, `.env.example`, parent quality plan Results.

## Acceptance (worker, scratch DB via `make test-db`)

1. **Flag off / no header (pi r2 F9, F10)**: for resolve (with and without
   `include_bodies`), node, default search, hybrid search, epic and neighbors, admin and
   filtered callers, env false and env true without the header: status, relevant headers
   and raw JSON equal `main`'s output after exactly the normalisation in
   `compatibility_test.go:135-175` (drop `request_id`, normalise `took_ms`), on fixtures
   with no tied scores; request-id presence and header/body agreement asserted separately.
   No other normalisation (no field-set, order or null changes). Env × header matrix
   (false/true × absent/`1`/other) with `max_items` omitted/0/out of range/wrong type:
   inactive requests behave exactly as `main` (including accepting junk `max_items`); only
   env=true + header `1` rejects bad values; no lead-only query runs on inactive requests
   (query counter). With env false and header `1`, `X-Graph-Lead: off` is the only
   difference. The A1 trace change is the one intended difference for requests carrying
   `X-Graph-No-Side-Effects`.
2. **No-trace**: mounted resolve and search (masked + trace wrapped) with a recorder/store
   spy, drained after the request: header `1`, `TRUE`, `true` write nothing and enqueue no
   `fetch_body`; header absent writes one record; body-only flag still writes the trace and
   skips `fetch_body`; header `0` overrides body `true`; request id header and JSON
   `request_id` present in all cases. `eval relevance --quiet`: a fake transport returning a
   `*url.Error` with a sentinel in the query (during warm-up and during a measured run), an
   HTTP 500 and a decode failure; neither stdout, stderr nor the returned error contains the
   URL or sentinel, and failure counts are still reported.
3. **Root eligibility (pi F1, r2 F1)**: on resolve, node-by-reply, default search and hybrid
   search, with a sentinel in the hidden root's cached thread summary and title: visible reply + hidden root, missing root, deleted root,
   root outside the epic filter, mismatched scope, root lookup error: forbidden ids, author
   ids, link prose and aggregate values never appear; lookup error gives 503.
4. **Masking before cap (pi F2)**: email, phone, card crossing rune 600; Unicode at the cut;
   lengths 599/600/601; admin vs filtered; on the mounted routes. No partial sensitive token;
   length ≤ 600 runes.
5. **Grouping and links (pi F3)**: cross-thread edge to a reply; edge from a seed-thread
   reply; two members with different kinds; equal scores; seed root plus higher-scoring
   member; reply seed with a different best reply; root absent from candidates. Check root
   ids, scores, hop, `focus_ts`, `from`/`to`, no self-links, winning metadata intact, no reply
   ids in artifacts or links.
6. **Search (pi F4)**: default and hybrid in lead mode: fields present, hybrid member-only
   rows dropped, `summary`/`overview` capped and masked, `lead_summary` filled with the
   documented fallback.
7. **Selection and misses (pi F5, r2 F5)**: bodyless and oversize candidates selected;
   cache rows present / absent for Slack and non-Slack items give the exact miss list,
   enqueued without the header, not with it; miss-query failure gives 503; seeds at and above `max_items`;
   invalid `max_items` 400; repeated runs give the same order.
8. **Aggregate (pi F6, r2 F1)**: a matching-thread non-message row (file, attachment)
   and a hidden row do not change activity or bot values; synthetic 200k nodes over 50 channels with shared timestamps and
   one hot thread; `EXPLAIN (ANALYZE, BUFFERS)` of the aggregate shows the new index; one
   aggregate query per response (query counter); cancelled context returns promptly.
9. **Bot fields (pi F7)**: answer-list bot, generic `is_bot`, human, missing person, loader
   error, root-only bot, human root with bot reply, non-Slack node.
10. Migrations: apply all, roll back only the last, the earlier one stays applied in
    `goose_db_version`, re-apply.
11. `make test-db` green, `go vet ./...` clean, MCP and non-DB tests green.

## Verify (conductor, after Enzo's merge and deploy go)

1. Deploy on the hub (as used for #93-#96, run on the hub itself in
   `~/go/src/github.com/agent-mem`):
   `PATH=/opt/homebrew/bin:$PATH BUILD_REV=$(git rev-parse HEAD) docker compose build --build-arg BUILD_REV=$(git rev-parse HEAD) worker && docker compose up -d worker`.
   Flag stays **off**. Confirm the running build the way #93-#96 did: the newest
   `graph.context_traces.build_rev` from a non-eval request equals the merged HEAD.
2. Flag-off check: `eval relevance --quiet` ranking and count metrics (precision, recall,
   harmful, duplicates, rows) equal the pre-deploy run on the same data revision; latency is
   reported, not compared for equality; 0 failed requests. `--quiet` output is checked for
   absence of URLs before it is pasted anywhere.
3. Trace check: note time T before the eval run, wait 60 s after it, then
   `SELECT count(*) FROM graph.context_traces WHERE caller='eval' AND created_at > T` in a
   read-only session: 0, and the eval reported 0 failed requests.
4. Turning the flag on (`GRAPH_LEAD_MODE_ENABLED=true` in the worker env, restart) needs
   Enzo's go. Then one real-asker resolve with `X-Graph-Lead: 1` and
   `X-Graph-No-Side-Effects: 1`: report field names, counts, latency only. Measure lead-mode
   resolve p50/p95 over the eval cases with a small script that prints numbers only.

## Findings

Built on `feat/graph-lead-mode` (base `8e97d74`). Not merged, not deployed, no PR.

### What was done

- A1: `noSideEffects` / `headerNoSideEffects` in `context_trace.go`; `traceHTTP` skips only
  `Enqueue` on header `1`/`true`; resolve uses the helper. `eval relevance --quiet`
  (`quietEvalError`, categories `transport`, `http_<status>`, `decode`, `setup`).
- Flag: `Deps.LeadModeEnabled` from env `GRAPH_LEAD_MODE_ENABLED` (`true`, case-insensitive)
  in `internal/worker/server.go`; handlers take it through `WithLeadMode(bool)` so existing
  constructor signatures are unchanged. Gate (`leadGate`) runs before body decoding; inactive
  resolve still decodes `resolveRequest` untouched; the active path reads the body once into
  bytes and decodes `leadResolveRequest` from the same bytes.
- A2/A2a/A3/A4/A5: `lead.go` (gate, `leadRoots`, `leadEnrich`, `capRunes`, `leadText`),
  `lead_resolve.go` (pipeline), `lead_search.go`, node lead path in `node.go`.
  `CanonicalizeSeeds` now wraps `canonicalizeSeedsNotify` (same behavior) to learn each
  reply seed's `focus_ts`. `sortedLinks` was extracted from `seedLinks` (same behavior).
- A6 and A5 index: migrations `20261012063721_graph_answer_bots.sql` and
  `20261012063729_graph_nodes_thread_key_idx.sql` (created with `make migrate-create`, one
  index per file, the index one is `NO TRANSACTION` / `CONCURRENTLY`).
- Docs: README (Lead mode section, env list), `.env.example`, parent quality plan Results.

### Differences / decisions where the plan was silent

- **Final Slack message node type set for A5: `{'slack'}`.** `ids.TypeSlackThread` is the
  string `"slack"` and ingest writes only that; `'slack_thread'` appears in read-side
  `IN (...)` lists and a schema comment but is never written (grep of `internal/`, `migrations/`).
- Seed roots are exempt from the epic/business filter (as seeds are on `main`); ACL, existence,
  deleted and scope checks still apply. Roots that are themselves authorized candidates are not
  re-checked against the epic filter.
- Root eligibility also requires `root.scope == member.scope` ("mismatched scope" case).
- Slack items take author fields (`author_slack_id`, `authored_by_bot`) from the thread root,
  per A2a, also on node-by-reply and search rows that are replies.
- `bot_in_thread`: `yes` if any visible live message is by a bot; else `unknown` if some visible
  message has no author person; else `no`. `unknown` also if the aggregate returns no row.
- Links query failure in lead mode is 503 `lead_lookup_failed` (main logs and drops links).
  Lead links that tie on confidence and endpoints after folding to roots get a deterministic
  tie-break on tag/topic/why (`leadLinkBetter`); `linkPick.better` is untouched.
- `context_tokens` in lead mode counts runes (not bytes): sum of ceil((runes(title)+runes(lead_summary))/4).
- Node lead response is a separate struct (`leadNodeResponse`) so the main `nodeResponse` JSON
  stays byte-identical; its `summary` is masked and capped like A4.
- Epic and neighbors have no lead gate: they ignore `X-Graph-Lead` and never emit `X-Graph-Lead: off`.
- Test migration check uses goose `ApplyVersion` on the newest version rather than `Down`
  (`Down` picks by insertion order and other tests re-apply older migrations).
- 30 new golden files `testdata/compatibility/lead_a_*.golden.json` were recorded from main
  `8e97d74`; the 8 existing goldens still pass.

### Tests and results

New tests: `lead_a_test.go` (acceptance 1, 2), `cmd/agent-mem/eval_quiet_test.go` (2, `--quiet`),
`lead_b_test.go` (3, 4, 5), `lead_c_test.go` (6, 7, 8, 9), `lead_c_migration_test.go` (10).
During development tests ran against throwaway pgvector containers (same image and database
name as `make test-db`, removed afterwards); the final runs are `make test-db`.

| Command | Result |
|---|---|
| `PATH=/opt/homebrew/bin:$PATH make test-db` | all lead and handler tests pass (266 `TestLead*` pass events); only failure is `internal/graph/jobs` `TestPeriodicTickerReclaimInterleavings` ("no original claim"), which fails identically on a clean `8e97d74` checkout (pre-existing, untouched package). It passed once in an earlier plain `go test -p=1 ./internal/graph/...` run, so it is timing-dependent. |
| `go vet ./...` | clean |
| `go test -count=1 ./...` with `DATABASE_URL` unset (non-DB and MCP tests) | green except `internal/hooks` and `internal/skills` (4 tests), which fail identically on a clean `8e97d74` checkout |
| skips under `make test-db` | 4, none caused by a missing DB: `TestDescribeAttachmentHandler_UnsupportedMime`, `TestParseDocument_RichPDF`, `TestTopicJudgeGolden`, `TestTopicJudgeFixture` (pre-existing, environment-gated) |

Perf check (acceptance 8): at 200k nodes / 50 channels / one 20k-reply hot thread the aggregate
uses `graph_nodes_thread_key_idx` under default planner settings (hot-thread plans about 20 ms).

### Open items

- `make test-db` exits 2 because of the pre-existing `jobs` failure; Enzo to decide whether to fix it separately.
- The `[bot answer, unverified]` summary label (umbrella P3) stays open; `bot_in_thread` is the interim signal.

## Fix round 1 (after deep review code-r1 APPROVE, `docs/reviews/2026-10-12-graph-context-r1a-lead-fields-code-r1.md`)

Enzo decided F1 on 2026-10-12 15:40 SGT. Fix all five on `feat/graph-lead-mode`, new commit:

1. **F1** `lead_summary` = `artifact_index.summary` (topic plus overview) for every node type,
   Slack included, masked then cut to 600 runes as before (`lead.go:299-304`). Title keeps the
   topic. Update A4's table row and the tests that assert the Slack source.
2. **F2** the migration test (`lead_c_migration_test.go:78-80`) must not fail when a newer
   migration is added: target this round's two versions by number, not "the newest".
3. **F3** `router.go:95-96`: a failed `NewResolve` must not panic the worker at startup; keep
   main's behaviour (route fails at request time) with a nil guard, plus a test.
4. **F4** lead search applies the epic filter only where the plan says (resolve); remove it from
   `lead_search.go:18-25`, or if the reply-root check needs it, record why in Findings.
5. **F5** the acceptance-1 query counter (`lead_a_test.go:50`) must see every lead-only query
   (all five the review lists), so "no lead query on inactive requests" is fully checked.

Then `make test-db` (only the known jobs flake may fail), `go vet`, update Findings, push.

### Fix round 1 results

- **F1** `lead.go`: `lead_summary` = `artifact_index.summary` for every node type (Slack roots
  included; thread-summary `summary`/topic no longer read). The unused `thread_summaries`
  join and `tsSummary` field were removed from the enrich query. Title fallback in resolve is
  unchanged (topic). A4 table row updated. Tests changed: `lead_b_test.go` (B3 fixture,
  mixed threads, B4 masking/cap, B5) and `lead_c_test.go` (thread A expects the index summary).
- **F2** `lead_c_migration_test.go`: dropped the "newest migration" guard; the test only
  requires the two versions to be found and `thread_key_idx` after `answer_bots`. Rollback
  and re-apply already addressed the versions by number (`ApplyVersion`).
- **F3** `router.go`: `WithLeadMode` is called only when `NewResolve` returned non-nil; the
  nil handler is mounted as on main. New test `lead_router_test.go`
  (`TestMountSurvivesNewResolveFailure`, closed pool, lead on/off).
- **F4** `lead_search.go`: `leadPage` no longer takes epic keys and passes nil to
  `leadRoots`, so lead search does not apply the epic filter to roots (A2a: resolve only).
- **F5** every lead-only SQL carries a `/* lead:<name> */` tag (`roots`, `roots_eligible`,
  `epic`, `enrich`, `aggregate`, `misses`, `display`, `links`); `epicMembers` became a wrapper
  over `epicMembersTagged` (tag empty on main's callers, so main's SQL text is unchanged).
  The acceptance-1 counter counts every `lead:` tag plus the two old markers; the positive
  control asserts `roots`, `enrich`, and on resolve `misses`/`display` run when active.
- **F4 test side effect:** `TestLeadB3RootEligibility` `root_outside_epic` now expects search and
  hybrid to keep the reply (node already did); the mismatched-scope hybrid case seeds a distinct
  own index summary for the public root and expects it as `lead_summary`.
- **Run:** `make test-db` all green (the `jobs` `TestPeriodicTickerReclaimInterleavings` flake
  passed this run); `go vet ./...` clean.
