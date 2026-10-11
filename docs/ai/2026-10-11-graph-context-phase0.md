# Graph context quality, Phase 0a: trace and measure resolve + search

Date: 2026-10-11. Owner: Enzo. Status: r4, APPROVED by pi (docs/reviews/2026-10-11-graph-context-phase0-r4.md); awaiting Enzo's approval, needs pi review and Enzo's approval.
r3 answers `docs/reviews/2026-10-11-graph-context-phase0-r2.md` findings 1 to 8; see
"r3 changes" at the end.
REVIEWER: pi (Enzo, 2026-10-11)
STATUS: approved (Enzo, 2026-10-11 13:39 SGT)
PAGE: https://claude.ai/artifact/Ay1BuvRTuC1rqgDmHxHTzk
Parent plan: `docs/ai/2026-10-11-graph-context-quality.md`; reviews in `docs/reviews/`.
Probe manifest: `docs/ai/2026-10-11-graph-context-probes.yaml`.

## What changed from r1

pi's r1 review (`docs/reviews/2026-10-11-graph-context-phase0.md`) showed r1 tried to
cover seven routes with one trace shape and one safety harness. r2 cuts the scope to
the two routes an answering model uses today (resolve and hybrid / ordinary search)
and leaves the synthetic safety fixtures for all routes to Phase 0b, a later round.
Each r1 must-fix is either answered below or moved out of scope by name (see
"Out of scope, named").

## Goal

1. A **context trace** for every resolve and search call: what was asked, who asked,
   what came back in what order with what score, so a bad answer can be traced to the
   items agent-mem returned. Metadata only; it records what was returned, it does not
   re-create old content.
2. A **relevance eval** for resolve and search over real requests, with exact metric
   definitions, run read-only against the hub, giving a baseline for later phases.
3. **Proof that nothing the routes return changed**, apart from the new id.

## Out of scope, named

- Node, neighbors, person, epic, cluster_summary: no trace, no eval (Phase 0b).
- Synthetic safety fixtures S1 to S9 and their self-test (Phase 0b). r1 findings 6
  and 7 belong there.
- Any change to ACL, ranking, bodies, summaries, bot handling, rejection.
- p-agent (its graph injection is off since 2026-10-11, `GRAPH_CONTEXT_ENABLED=false`).
- A dashboard page for traces.

## Decisions already made (Enzo, 2026-10-11, recorded for later phases)

- D1 Org-wide scopes: every active employee with an eeid may see stored Jira, GitHub
  and Confluence content; GWS per file; Slack per channel.
- D2 PII: mask emails, phones, card-like numbers for non-admin callers; keep payment
  and booking refs.
- D3 No second API key; unfiltered access only by explicit admin header; no asker is
  an error; MCP fails closed. The ACL protects against mistakes, not a hostile local
  process.
- D4 Graph items are leads, not facts (p-agent plan).
- D5 Tests before p-agent goes live: wrong link, stale fact, bot echo, empty graph,
  access, A/B on 10 real requests.
- D6 Deploy target is the payments Mac mini as in `CLAUDE.md` "Deployment" (Enzo
  confirmed it permanent 2026-08-15). `AGENTS.md:48-66` and `make deploy` describe the
  retired VPS and are stale; do not use them. Fixing those docs is not this round.

## Part A: context trace (resolve, search)

### Correlation id

- Every request to `POST /api/graph/resolve` and `GET /api/graph/search` (both match
  modes) gets an id `ctx_` + 20 random base32 chars, generated at handler entry.
- The id is sent as response header `X-Graph-Request-Id` on **every** response of
  these two routes, success or error. Error bodies, status codes and content types
  stay exactly as today (plain-text `http.Error`).
- On JSON success responses only, a top-level `request_id` field is added. Nothing
  else changes.
- Requests rejected before the handler (auth middleware, routing) are not traced.

### Table `graph.context_traces` (one migration file)

| column | type | meaning |
|---|---|---|
| `id` | text pk | the request id |
| `created_at` | timestamptz, indexed | |
| `route` | text | `resolve` / `search` |
| `build_rev` | text | git revision compiled into the binary (see "Build revision") |
| `caller` | text | `X-Graph-Caller` header if sent, else `unknown`; max 64 chars |
| `status` | int | HTTP status returned |
| `error` | text | first 500 chars of the error text, null on success |
| `asker_input` | jsonb | the identity inputs **as received**, both kept if both sent: `{"body_asker_eeid": ..., "header_asker_user": ...}` |
| `acl_mode` | text | the decision the existing handler made, read from its own variables, not recomputed: resolve: `unfiltered` when `noFilter` (`handlers/resolve.go:282`) is true, else `filtered`; search: `unfiltered` / `filtered` / `public-only` from `askerScopeSet` (`handlers/acl_guard.go:18-35`); `not-evaluated` when the request failed before that point. Search `public-only` means the header was present but did not resolve to an eeid; a resolved eeid whose membership build failed (`acl_guard.go:28-31`) is recorded as `filtered-membership-error`. `askerScopeSet` cannot show this today (it returns `(eeid, scopeSet, noFilter)` for both cases), so add `askerScopeSetDiag` returning the same values plus the membership error from its existing error branch; `askerScopeSet` becomes a wrapper with unchanged results for other callers. Never infer the error from scope-set size or by re-running the lookup. Tests cover a zero-membership asker and an injected builder failure |
| `request` | jsonb | resolve: seeds as sent, canonical seed ids, query, depth, budget_tokens, include_bodies; search: q, match, limit, other query params; strings capped at 2,000 chars, arrays at 50 entries |
| `items` | jsonb | one entry per returned item, in returned order (see mapping); capped at 200 entries, `items_truncated` true when cut |
| `items_truncated` | bool | |
| `response_sha256` | text | sha256 of the exact response body bytes sent |
| `latency_ms` | int | handler entry to response written |

Item mapping (no other fields are invented; a field the route does not return is
`null`, never 0):

| route | JSON path of the item list | per item |
|---|---|---|
| resolve | `artifacts[]` (`handlers/resolve.go:42-57`) | `node_id`, `rank` (0-based position), `score`, `score_breakdown`, `hop`, `links`, `is_seed` (node id in canonical seeds), `title_sha256`, `title_source` (`node` or `thread_summary`, per `hydrate/budget.go:49-60`), `body_included` (bool), `body_truncated` (bool, per `hydrate/budget.go:121-124`), `body_bytes` (UTF-8 bytes of the returned body, null when not included), `body_sha256` (null when not included; sha256 of the empty string only when an empty body was returned) |
| search ordinary | the result array serialised at `handlers/search.go:375-406` | `node_id`, `rank`, `score`, `score_breakdown` if present, `title_sha256`, `summary_sha256` (ordinary search uses node title and `artifact_index.summary`, `search.go:431-439,489-492`) |
| search hybrid | the hybrid result array (same file) | as ordinary, plus `arm_scores` if present, and `title_source` (`node_title` / `body_first_line` / `thread_summary`) and `summary_source` (`artifact_index` / `overview`) as decided in `search_hybrid_display.go:138-139,167-173`; the response-level `arm_errors` (`search.go:175-195,398-400`) is stored in `request` |

Digests let us tell later whether the content now shown differs from what was
returned; they do not let us re-create it, and the CLI says so.

### Writer

- In-process bounded queue: 1,000 entries and 8 MB total serialised size; when full,
  the trace is dropped and a counter goes up. Writes use their own context with a 2 s
  timeout, never the request context. On shutdown the queue drains for at most 5 s,
  then the rest is counted as dropped.
- Counters `attempted`, `written`, `dropped`, `failed`, exposed on the existing
  metrics or status endpoint the worker finds (else a new `GET /api/graph/traces/stats`
  behind the API key).
- Overhead bound: measured in-process on the scratch fixture (Part C), handler time
  from entry to response written, paired runs with the recorder on and off (Go
  benchmark, 200 iterations per request kind, resolve with and without query, both
  search modes): p95 overhead <= 5 ms. Hub timings in Part B are round-trip and
  observational only.
- Coverage is best effort: "every call" means every call the queue accepted; `trace
  stats` prints the writer counters beside every figure, and a row with
  `items_truncated` is excluded from item-count statistics and counted separately.
- Retention 30 days: a prune in the existing scheduler deletes rows with
  `created_at < now() - 30 days`.

### CLI

- `agent-mem trace show <id>`: prints the stored row; for each item it also renders
  the node's **current** representation with the renderer of the original route and
  mode (resolve, ordinary search, or hybrid search; same title and summary sources, same body inclusion, same truncation prefix length) and marks each
  line `current, may differ from what was returned`. It prints `same as returned`
  when the like-for-like digest matches, `differs from returned` when it does not,
  `not comparable (body not returned)` when the stored item had no body, and
  `node no longer exists` for a deleted node. It never claims when or why a change
  happened.
  Unknown id: prints `not found (never written, dropped, or older than 30 days)` and
  the drop counters.
- `agent-mem trace list --since 24h [--route] [--caller]`.
- `agent-mem trace stats --since 7d`: calls per route / caller / acl_mode / status,
  latency p50 / p95, items per call, share of resolve calls with 0 non-seed items,
  and the writer counters next to them.

### MCP

`internal/graphmcp/client.go` sends `X-Graph-Caller: mcp` on every call. No other MCP
change.

## Part B: relevance eval (resolve, search)

### Cases and request variants

- `testdata/eval/graph_context/cases/*.yaml`, 20 files: the 6 probes
  (`probe-a` .. `probe-f`) and 14 more cases so every use case UC1 to UC10 of the
  parent plan has at least one, taken from EnzoBot threads of the last 30 days
  (source: p-agent `src/data/slack-sessions.db` table `bot_mentions`, read-only;
  thread text via the Slack token the hub already uses).
- Every case file holds the same three named request variants, so every case is
  replayed the same way and probes are not weighted twice:

  | variant | request |
  |---|---|
  | `resolve-asker0` | `POST /api/graph/resolve` with `asker_eeid: 0` |
  | `resolve-asker` | the same body with the asker's real eeid |
  | `search-hybrid` | `GET /api/graph/search` with `match=hybrid`, `limit=10`, `types=<p-agent SEARCH_TYPES>`, and **no** `X-Asker-User` header |

- Probe conversion is mechanical (`agent-mem eval import-probes`): from
  `docs/ai/2026-10-11-graph-context-probes.yaml` take `resolve_request` and emit one
  body per value in its `asker_eeid` list (`0` gives `resolve-asker0`, the other
  gives `resolve-asker`); a field whose manifest value is `null` is omitted from the
  body (for example `budget_tokens`, meaning server default 4000); `hybrid_request`
  becomes `search-hybrid` exactly. The manifest's prose `variants` are not replayed;
  they stay in the case file as `notes`.
- `agent-mem eval relevance --dry-run` prints every HTTP request (method, path,
  query, headers except the key, body) for all cases and variants; for the six
  probes it must match the manifest's primary requests field by field.
- Case schema:

  ```yaml
  id: probe-a
  use_case: UC2
  source_thread: https://wego.slack.com/archives/<C>/p<ts>
  asker_slack_id: U...
  asker_eeid: 651
  needs_context: yes          # does answering this request need graph history? yes / no; scores the later p-agent context gate
  canonical_seeds: [slack:..., jira:PAY-1731]   # written by `eval prepare`, never by hand
  requests:
    resolve-asker0: {body: {...}}
    resolve-asker:  {body: {...}}
    search-hybrid:  {params: {q: "...", match: hybrid, limit: 10, types: "..."}, headers: {}}
  labels:                      # relevance labels, shared by both resolve variants
    resolve: {must: [...], may: [...], must_not: [...]}
    search:  {must: [...], may: [...], must_not: [...]}
  label_reasons: {"<node id>": "one line, no customer PII"}
  labelled_by: fable-draft
  checked_by: null             # set only by Enzo; never by the worker
  notes: "..."
  ```

  `must`, `may`, `must_not` are disjoint and contain no canonical seed ids. Labels
  judge relevance only; access is not a label (Phase 0b).
- Labels: the worker drafts them (it may use a Fable sub-agent), sets
  `labelled_by: fable-draft`, leaves `checked_by: null`. Enzo checks at least 5
  cases afterwards; until then every baseline number is reported as **draft labels**.

### Oracles: canonical seeds and node metadata

Both are produced once by `agent-mem eval prepare`, which runs **on the hub** with
the hub's `.env` `DATABASE_URL` in a read-only transaction
(`SET TRANSACTION READ ONLY`), and are stored in the repo so `eval relevance` needs
only the base URL and works with tracing off.

- `canonical_seeds`: computed by the same seed canonicalisation resolve uses
  (`handlers/resolve.go:433-535`: URL resolution, reply-to-root, epic expansion).
  Today it is a method on `Resolve` that queries `h.db` (`*pgxpool.Pool`,
  `resolve.go:77-80`; calls at `:474-495`, `:519-524`). Extract it, behaviour
  unchanged, into a function taking a small interface with `Query` and `QueryRow`
  that both `*pgxpool.Pool` and `pgx.Tx` satisfy. Resolve passes its pool; `prepare`
  passes its read-only transaction. A test shows resolve's canonical seeds are
  unchanged for the Part C requests, and that a write attempted through the prepare
  transaction is rejected. Any lookup or transaction error fails `prepare`; it never
  saves a partial oracle.
- The `prepare` HTTP replays (used to collect returned node ids) do not run in that
  transaction; they go through the API with the no-side-effects header like any
  eval call.
- `testdata/eval/graph_context/node_meta.json`: for every node id in any label, any
  canonical seed, and any item returned by a `prepare` replay of all variants:
  `{type, exists, is_reply, thread_root}`. Reply predicate: a `slack` node whose
  `metadata.thread_ts` (`handlers/ingest_content.go:45,417`; read by resolve at
  `resolve.go:321`) is present and differs from the node's own ts (the ts part of its
  id). Missing `thread_ts` means not a reply; non-Slack nodes are never replies;
  a node that does not exist is `exists: false`.
- In `eval relevance`, a returned node absent from `node_meta.json` is counted in an
  `unknown_meta` column and excluded from the reply count; it is never silently
  treated as non-reply. `eval prepare --refresh` re-runs to add them.
- Seed exclusion uses the case's `canonical_seeds` for all three variants,
  including `search-hybrid`.

### Metrics

Per case and variant, over non-seed items only:

- precision@10 = |top10 ∩ must| / |top10 \ may|; `n/a` when the denominator is 0.
- unlabelled@10 = |top10 \ (must ∪ may ∪ must_not)|.
- recall = |returned ∩ must| / |must|; `n/a` when `must` is empty.
- harmful = |returned ∩ must_not|, over all returned items.
- duplicate replies = returned items with `is_reply: true` in `node_meta.json`.
- unknown_meta as above.
- Aggregate per variant: unweighted mean over cases with a value, plus the count of
  `n/a` cases; a variant with no case is printed `no coverage`. Each case counts
  once per variant.
- Ranking metrics use the first **successful** measured run (HTTP 2xx with a
  parseable body). A case with no successful run is `failed`, listed by id, and
  counted in its own column; it is never scored as an empty result. An empty result
  from a successful run is a real empty result. Aggregates print `scored`, `n/a`
  and `failed` counts side by side. Hybrid `arm_errors` are kept per run in the
  `--json` output and a case with any arm error is flagged `degraded`.
- `unstable`: among successful runs, more than one distinct ordered id list (count
  printed).

### Latency on the hub (observational)

One warm-up run, then 10 sequential runs per case and variant. Client round-trip
time, labelled as such. p50 and p95 (nearest rank) per variant, with sample count;
failed or timed-out runs (timeout 10 s) are counted and listed; a group with fewer
than 30 successful samples is marked `low sample`. No pass/fail threshold: the
overhead gate is the in-process benchmark in Part A.

### No side effects

Resolve enqueues body fetches on a cache miss (`handlers/resolve.go:424-427`). New
header `X-Graph-No-Side-Effects: 1` (and body field `no_side_effects: true` on
resolve; header wins if both are sent) skips that enqueue and every other database
write and job enqueue in the resolve and search request paths, except the trace row.
`eval` always sends it. Default behaviour unchanged. Header values `1` and `true`
(case-insensitive) turn it on; any other non-empty value turns it off and still wins
over the body field; an absent or empty header defers to the body field.

- Scope: database writes and job enqueues. **Process-local in-memory caches are
  excluded** (for example the ACL scope cache filled by `acl.Builder.For`,
  `acl/scopes.go:37-53`); they do not change the graph and are lost on restart.
- The worker lists every dependency of the two paths it inspected (function names)
  in the PR description.
- Proof (in `make test-db`, scratch DB), by a read-only request pool instead of
  counters:
  - The test builds the resolve and search handlers on a dedicated `pgxpool.Pool`
    whose `AfterConnect` runs `SET default_transaction_read_only = on`. Any write
    the handlers attempt through it fails with Postgres error 25006, which the
    test treats as a failure. The trace writer and fixture setup use a separate
    normal pool, so their writes are outside the measured path.
  - Job enqueues: the worker finds the enqueue entry point(s) used by the two paths
    and injects a counting fake in the test (same mechanism for every enqueue the
    inventory lists); the test asserts zero calls with the flag.
  - Run every Part C request with the flag: all succeed, zero write errors, zero
    enqueues.
  - Positive controls, through the same read-only request pool and the same fake:
    an UPDATE of an existing row and an INSERT both fail with 25006; one resolve
    cache-miss request **without** the flag records at least one enqueue. The test
    fails if a control does not trigger.
  - If an enqueue writes to the database rather than an in-process queue, the
    read-only pool already catches it; the fake is still used so the count is
    visible.

### Commands

- `agent-mem eval import-probes` (writes the 6 probe case files from the manifest).
- `agent-mem eval prepare [--refresh]` (hub only; read-only transaction; writes
  `canonical_seeds` and `node_meta.json`).
- `agent-mem eval relevance --base-url <url> [--json <file>] [--dry-run]`: API key
  from `.env` (`AGENT_MEM_API_KEY`), never argv; always sends the no-side-effects
  header and `X-Graph-Caller: eval`. `--json` writes `{build_rev, cases_sha256,
  node_meta_sha256, started_at, per_case[...], aggregate{...}, request_ids[...]}`.
  Exit code non-zero only for harness errors (bad case file, unreachable server,
  missing oracle file), not for low scores.

## Part C: proof that responses did not change

Deterministic compatibility test on the scratch DB:

- **Clock seam.** Resolve reads `time.Now()` directly (`handlers/resolve.go:546`,
  used for recency at `:565-575`). Add a package-level `var nowFunc = time.Now` in
  the handlers package used by resolve and by search's default clock
  (`search.go:53`); production behaviour is identical. The test sets it to a fixed
  instant.
- **Commit order.** Commit 1 of the branch adds only the clock seam, the fixture
  graph, the compatibility test and its goldens, recorded from that code (no other
  behaviour change). The goldens are recorded twice in separate processes started
  at least 2 s apart; the normalised response bytes must be identical before they
  are committed. Commit 2 adds `golden/PROVENANCE` holding commit 1's sha and the
  fixture sha256; golden files themselves never change afterwards. All later
  commits must keep them.
- **No ties.** The fixture uses distinct timestamps and edges so no two returned
  scores are equal; the test fails if any two artifacts in a golden response have
  equal scores (resolve's sort at `:299-303` is not stable on ties).
- **Requests:** resolve (normal hit, reply seed, empty result, cache miss, invalid
  request 400) and both search modes (normal hit, empty, unknown asker header);
  search embedding calls replaced by a fixed fake embedder.
- **Comparison:** same status, same content type, same body after removing the
  top-level `request_id` key and replacing `graph_trace.took_ms`
  (`resolve.go:67-73,348`) and any search timing field with a fixed value; arrays in
  order; null and absent kept distinct. Any other difference fails. The PR shows
  one deliberate mutation (body, order, status, field presence) failing the test.

## Build revision

`Dockerfile:6` runs `go build` and `.dockerignore:1` excludes `.git`, so Go VCS
stamping cannot work. Add `ARG BUILD_REV` to the Dockerfile and build with
`-ldflags "-X main.buildRev=${BUILD_REV}"` (string var in `package main`, `cmd/agent-mem/main.go`); pass it through
`docker-compose.yml` `build.args`. Deploy command (replaces the one in `CLAUDE.md`
"Deployment", which the worker updates):

```bash
ssh enzo@payments 'cd ~/go/src/github.com/agent-mem && git pull --ff-only \
  && PATH=/opt/homebrew/bin:$PATH BUILD_REV=$(git rev-parse HEAD) \
     docker compose build --build-arg BUILD_REV=$(git rev-parse HEAD) worker \
  && PATH=/opt/homebrew/bin:$PATH docker compose up -d worker'
```

A binary with an empty revision reports `unknown`; acceptance requires the exact
merged sha.

## Test database

Use the existing `make test-db` target (`Makefile:42-44`, sets `DATABASE_URL` and
`AGENT_MEM_TEST_DATABASE_URL` from `TEST_DATABASE_URL`, clears `AGENT_MEM_EVAL`, runs
`-p=1 -count=1 -json`). Use database name `agentmem_test` (accepted by
`handlers/helpers_test.go:18`); do not change any name guard. Extend the target only:

- if `TEST_DATABASE_URL` is unset, start a `pgvector/pgvector:pg16` container named
  `agentmem-test-<random>`, wait for readiness, run migrations, and remove the
  container on exit (success or failure, via `trap`);
- pipe `go test -json` through a small checker (`scripts/check-db-skips.go` or
  similar) that groups `output` records by package and test, marks a test whose
  output mentions `DATABASE_URL` or `AGENT_MEM_TEST_DATABASE_URL` and whose final
  action is `skip`, fails if any is found, and preserves `go test`'s own exit code
  otherwise; other skips stay allowed;
- fault injection: `AGENT_MEM_TEST_DROP_VAR=<name>` makes the target unset that one
  variable for the `go test` child after setup; used only to prove the checker.

## Files expected to change

- new migration: `graph.context_traces`
- `internal/graph/handlers/resolve.go`, `search.go` (id, header, trace hook,
  no-side-effects, clock seam, exported seed canonicalisation)
- new `internal/graph/trace/` (recorder, queue, writer, prune, counters)
- `internal/graphmcp/client.go` (`X-Graph-Caller: mcp`)
- `cmd/agent-mem/trace.go`, `cmd/agent-mem/eval.go`, main package (`buildRev`)
- `Dockerfile`, `docker-compose.yml` (build arg), `CLAUDE.md` deploy snippet
- `testdata/eval/graph_context/` (cases, node_meta.json, golden, fixture graph)
- `Makefile` (`test-db` extension), skip checker
- this file (results)

## Acceptance

1. `make test-db` green; `AGENT_MEM_TEST_DROP_VAR=DATABASE_URL` and
   `AGENT_MEM_TEST_DROP_VAR=AGENT_MEM_TEST_DATABASE_URL` each make it fail through
   the skip checker.
2. Part C compatibility test green; goldens recorded twice identical; the deliberate
   mutation fails it.
3. Trace rows exist for resolve and both search modes, for success and one error
   each; error responses byte-identical apart from the new header; `trace show`
   prints them with the like-for-like labels; unknown id prints the not-found line.
4. Writer tests: slow DB, full queue, canceled request, shutdown do not change the
   response or block the handler past 2 s; counters move as expected; prune deletes
   only rows older than 30 days. Overhead benchmark p95 <= 5 ms.
5. No-side-effects proof passes: zero write errors and zero enqueues with the flag;
   every positive control triggers.
6. `eval import-probes` + `--dry-run` match the manifest's primary requests;
   `eval prepare` and `eval relevance` run on the hub; results (draft labels) and
   observational latency pasted into the parent plan's "Results".
7. 20 case files, every UC covered, `checked_by` null on all.
8. Deployed with the new command; a controlled `curl` resolve on the hub writes a
   trace row whose `build_rev` equals the merged sha.

## Stop and report (do not invent) if

- a probe in the manifest cannot be converted by the rules above;
- `eval prepare` cannot run in a read-only transaction on the hub;
- a DB write or job enqueue in resolve / search cannot be skipped under the flag
  without changing the normal path;
- an enqueue entry point cannot be replaced by a fake in tests without changing the
  normal path.

## r3 changes (answers to r2 review)

1. Clock seam, commit-1 goldens recorded twice, no-tie fixture (Part C).
2. Three named variants per case, mechanical probe import, dry-run check (Part B).
3. `eval prepare` on the hub writes canonical seeds and node metadata; unknown
   metadata counted, never assumed (Part B).
4. Scope says DB writes and job enqueues; in-memory caches excluded by name;
   `pg_stat_user_tables` delta proof with positive controls (Part B).
5. Build revision via build arg and ldflags; deploy command updated (Build revision).
6. Skip checker correlates records; drop-variable fault injection; `agentmem_test`
   only; container cleanup (Test database).
7. Overhead gate moved to an in-process paired benchmark; hub latency observational
   (Parts A, B).
8. Like-for-like drift labels in `trace show`; representation fields stored (Part A).

## r4 changes (answers to r3 review)

1. No-write proof uses a read-only request pool (`default_transaction_read_only`)
   plus a counting enqueue fake, with positive controls on the same path; the
   `pg_stat` counter approach is dropped.
2. Seed canonicalisation extracted behind a `Query`/`QueryRow` interface so
   `prepare` runs it inside its read-only transaction; HTTP replays separated.
3. `askerScopeSetDiag` exposes the membership error; wrapper keeps old callers.
4. Search: ordinary rows cite `search.go`; hybrid rows store separate title and
   summary sources; `trace show` uses the original route's renderer.
5. First successful run is scored; failed cases counted, never scored as empty;
   `arm_errors` kept; `degraded` flag.
6. Goldens in commit 1, provenance sidecar in commit 2.
7. `-X main.buildRev`.

## Implementation notes from the r4 review (binding)

- The read-only pool proves no write lands; also attach a connection-level query
  tracer / error collector to the request pool so a write attempt is counted even
  where application code discards the error (for example the enqueue `Exec` at
  `resolve.go:602-610`). "Zero write errors" means zero collected 25006 errors.
- Canonicalisation extraction keeps today's no-match semantics: `pgx.ErrNoRows` on
  an optional URL / root lookup is not a failure (resolve skips an unmatched URL and
  keeps an unpromoted Slack id, `resolve.go:473-503`; unknown epics expand to
  nothing, `:508-535`). Return diagnostics beside the best-effort result; `prepare`
  fails only on real database or transaction errors.
- "Every Part C request with the flag: all succeed" means every request matches its
  expected status and body, including the deliberate 400; do not change the invalid
  request's response.
- Case schema gains `needs_context: yes|no` (added after approval, 2026-10-11, at
  Enzo's request): a label only, used later to score a Sonnet context gate in
  p-agent; `eval relevance` prints it per case and does not use it in any metric.
