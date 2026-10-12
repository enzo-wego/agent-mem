# Graph context Phase 0a.2: finish the eval harness

Date: 2026-10-11. Owner: Enzo. Status: draft r2 (after pi review r1, docs/reviews/2026-10-11-graph-context-phase0-eval.md), needs pi review and Enzo's approval.
REVIEWER: pi (Enzo, 2026-10-11)
STATUS: approved (Enzo, 2026-10-11 18:06 SGT)
PAGE: https://claude.ai/artifact/2ZBB54G4upoAmRu6Zbdfvv
Parent: `docs/ai/2026-10-11-graph-context-phase0.md` (Phase 0a, merged in #88,
deployed 2026-10-11 as build `3d36fc7`). Repo: agent-mem only. Branch
`feat/graph-context-phase0-eval`, worktree
`/Users/enzo/go/src/github.com/agent-mem-wt-gc0b`.

## Why

Phase 0a shipped the trace and the case files, but three things its plan required are
missing, so the baseline cannot be scored:

1. `agent-mem eval prepare` was not built (parent plan, Part B "Oracles" and
   "Commands"): no `canonical_seeds`, no `testdata/eval/graph_context/node_meta.json`.
2. `eval relevance` sends each variant once (`cmd/agent-mem/eval.go:150-275`); the
   parent plan requires one warm-up plus 10 measured runs, p50 / p95 per variant,
   `unstable`, `failed`, `degraded`, and reading `AGENT_MEM_API_KEY` from `.env`
   (today it reads only the process environment, `eval.go:218`).
3. No labels: all 20 cases have `labelled_by: none`.

First observational numbers from the 1-run baseline on the hub (2026-10-11, trace
rows with `caller='eval'`): resolve mean 19.1 items with asker 0 and 26.6 with the
real asker; about 90% of resolve items are thread replies (approximate predicate);
resolve p50 40 ms / p95 147 ms; hybrid search p50 1,332 ms / p95 2,165 ms. Hybrid
search returned up to 20 items with `limit=10` (check in item 4 below).

## Scope

Everything in the parent plan's Part B that is not built yet, exactly as specified
there (read Part B in full; it is the contract). In short:

1. **`eval prepare [--refresh]`.** Runs on the hub with `DATABASE_URL` read from the
   repo's `.env` file (not from argv), opens one transaction with
   `SET TRANSACTION READ ONLY`, and through the extracted canonicalisation function
   (merged in #88; it takes the `Query`/`QueryRow` interface) writes each case's
   `canonical_seeds`. Then replays all variants over HTTP with the no-side-effects
   header and writes `node_meta.json` for every node id in labels, seeds and returned
   items: `{type, exists, is_reply, thread_root}`. Reply predicate: a `slack` node
   whose `metadata.thread_ts` is present and differs from the node's own ts, which
   is colon component 3 of `slack:<channel>:<own ts>` (`internal/graph/ids/ids.go:46-53`,
   `handlers/ingest_content.go:559-567`). Any database or transaction error fails the command; it never writes a
   partial file. `--refresh` adds ids missing from an existing file.
   It also writes `testdata/eval/graph_context/replay.json`, ids only, no content:
   `{prepared_at, build_rev, cases_sha256, cases: {<case id>: {canonical_seeds:
   [...], variants: {<variant>: {status, request_id, returned: [node ids in
   order]}}}}}`. `prepare` writes `node_meta.json` and `replay.json` together via
   temp files and rename, only after every replay finished; on any error neither file
   is replaced. `--refresh` rewrites `replay.json` from the new replay. Both files
   are committed (ids and flags only).
2. **`eval relevance`, full Part B.** One warm-up, 10 measured runs per case and
   variant; ranking metrics from the first successful run; `unstable`, `failed`,
   `degraded` (hybrid `arm_errors`), `unknown_meta`, duplicate replies from
   `node_meta.json`; p50 / p95 (nearest rank) per variant with sample counts and
   `low sample` under 30; precision@10, unlabelled@10, recall and harmful per the
   parent plan's formulas once labels exist; cases with `labelled_by: none` print
   `unlabelled` and are excluded from relevance means but included in latency and
   duplicate-reply figures. API key read from the repo `.env` (`AGENT_MEM_API_KEY`)
   when the environment does not set it; never from argv. The text output prints one
   summary table per variant at the end.
3. **`eval label-sheet <case-id|--all> --out <dir>`.** For labelling only: one
   markdown file per case listing the case query, the source thread link, and every
   distinct non-seed node in that case's `replay.json` entry (all variants, canonical
   seeds excluded; never a new HTTP replay). It refuses to run when `replay.json` is
   missing, when its `cases_sha256` differs from the current case files, or when a
   requested case is absent from it. Current content is read through the read-only
   database transaction (same `.env` `DATABASE_URL` as `prepare`), one row per node, with
   node id, type, title, the first 300 characters of the current summary or body,
   and `is_reply`. No customer data filtering is attempted by the tool; the sheets
   are written outside the repo (the `--out` directory is required and must not be
   inside the repo) and never committed.
4. **Search `limit` check.** pi's review already traced it: `search.go:370-379`
   truncates to `limit` before `addLinkedThreads`, which may add up to 10 linked rows
   (`search_linked_threads.go:13-14,200`; `TestLinkedOnlyKeptSourcesExpand` expects 2
   results for `limit=1`). So more than `limit` is by design. In the harness, record
   per item whether it is a primary or a linked row (the response marks linked rows;
   the worker finds the field) and report both counts; metrics use all returned
   items. Write this into "Findings". No handler change.
5. Replace the repository-case invariant in `cmd/agent-mem/eval_test.go:39-48`
   (today it requires `checked_by == nil`, `labelled_by == "none"` and empty label
   lists in every real case, which labelling would break) with validation of allowed
   states: `labelled_by` in {`none`, `fable-draft`, `human`}; `checked_by` null or
   `enzo`; label lists disjoint; no canonical seed in any list; `labelled_by: none`
   implies empty lists. Keep the "imported probes start unlabelled" assertions in the
   import test (`eval_test.go:142-175`). Add synthetic fixtures: one valid labelled
   and checked case (passes), one with overlapping lists and one with a seed in
   `must` (each fails).
6. `make test-db` keeps passing; new tests for: prepare uses the read-only
   transaction and fails on a write; prepare never writes a partial `node_meta.json`;
   relevance computes the metrics on a fixed fake server (including a first-run
   failure, an unstable case, a degraded case, an unlabelled case); key read from a
   temp `.env`; `label-sheet` on a fake server: prepare two cases with different
   candidate sets, change the server's later responses, and check each sheet still
   lists its own saved replay ids with canonical seeds excluded; missing or mismatched
   `replay.json` is refused. Ranking uses the first successful **measured** run, never
   the warm-up.

Out of scope: drafting labels (the conductor does it after this merges, with a
Fable agent reading the label sheets, then Enzo checks 5 cases); any handler change;
anything in Phases 0b to 5; p-agent.

## Files expected to change

`cmd/agent-mem/eval.go` (and new files beside it), `cmd/agent-mem/eval_test.go`,
test fixtures under `cmd/agent-mem/testdata/`, this file. On the hub, after merge:
case `canonical_seeds`, `node_meta.json`, `replay.json` (ids only).

## Acceptance

1. `make test-db` green except the known flaky
   `TestPeriodicTickerReclaimInterleavings` (fails about 1 in 5 on unchanged main);
   `go vet ./...` clean.
2. The new tests above exist and fail on db82f1d where they test new behaviour.
3. `eval relevance --dry-run` output unchanged for all 20 cases (same requests).
4. Findings section filled for item 4.
5. Branch pushed; no merge, no deploy, no hub runs in this dispatch.

After merge (conductor, with Enzo's go): no worker redeploy is needed (CLI only);
run `go run ./cmd/agent-mem eval prepare` and `... eval relevance` from the updated
main checkout on the hub, generate label sheets, draft labels, Enzo checks 5, re-run relevance,
paste numbers into the parent plan's "Results".

## Findings

(filled by the worker)

### Item 4: search `limit` (worker, 2026-10-11; code-traced, no hub run)

- Hybrid search truncates the fused list to `limit` (`internal/graph/handlers/search.go:370-372`)
  and only then calls `addLinkedThreads` (`:373-378`), which inserts up to 3 Slack
  threads per kept Jira/Confluence source and 10 overall (`linkedPerSource`,
  `linkedTotal`, `search_linked_threads.go:13-14`). So a response may hold up to
  `limit + 10` rows by design (the 20 rows seen at `limit=10` fit). No handler change.
- The response marks linked rows: hybrid rows carry `linked_via` / `linked_via_title`
  (`search_hybrid_display.go:39-40,138-141`, `match: ["linked"]`).
- The harness classifies each `search-hybrid` row by `linked_via` presence and reports
  `primary` and `linked` counts per case (per-case JSON `primary`, `linked`; text line
  `returned=N (primary=P linked=L)`; per-variant totals in the summary). Ranking metrics
  use all returned non-seed items, as specified; precision@10 and unlabelled@10 take the
  first 10 of them in response order.
- Not measured here: the actual primary/linked split on the hub. It appears in the first
  `eval relevance` run after merge.

### Implementation notes (worker)

- Commands built: `eval prepare [--refresh] --base-url`, `eval relevance` (warm-up + 10
  runs, full metrics), `eval label-sheet <case-id|--all> --out <dir>`.
- `prepare` and `label-sheet` read `DATABASE_URL` from `./.env` (falling back to the
  process environment); `relevance`/`prepare` read `AGENT_MEM_API_KEY` from the
  environment, then `./.env`. Run them from the repo root.
- `prepare` needs `--base-url` for the replays (the plan does not name the flag; it is the
  same as `relevance`). A non-2xx or transport error in any replay fails the command.
- `prepare` rewrites only the `canonical_seeds:` line of each case file (anchors and
  formatting stay); `replay.json` `cases_sha256` is computed over the rewritten files.
- `thread_root` in `node_meta.json` is `slack:<channel>:<thread_ts>` for any Slack node
  with a `thread_ts` (a root points at itself), empty otherwise.
- `relevance` treats an unreachable server (first warm-up gets no HTTP response) and a
  missing `node_meta.json` as harness errors; later transport failures count as failed runs.
- `request_ids` in `--json` include warm-up and measured calls.

### Lead-mode snapshot and relation labels (R1b, 2026-10-12)

Plan: `docs/ai/2026-10-12-graph-context-r1b-wrong-item-labels.md`. Case files and their
`labels` are not touched; the new ground truth lives in its own file.

- **Variant `resolve-lead`.** Fourth entry of the variant list: the `resolve-asker` body
  (deep copy) with `include_bodies: false` and `max_items: 30`, headers `X-Graph-Lead: 1`
  plus the harness headers, never `X-Graph-Admin`. `relevance` scores it against
  `labels.resolve` and adds per case `roots`, `seed_groups`, and `unreachable_must`
  (reply-only `must` ids per `node_meta.json`, excluded from the lead recall denominator
  only). Lead responses are decoded strictly (HTTP 2xx, no `X-Graph-Lead: off`, an
  `artifacts` array, unique ids, integer `hop`, valid `links`, `authored_by_bot` in
  `human|bot|unknown`); anything else is a failed run (`--quiet` category: the same categories as the old
  variants, e.g. `HTTP <n>`, a transport error, or the lead outcome). Precision@10 excludes
  seeds by `canonical_seeds` as for the old variants; for a reply seed the lead hop-0 root has
  a different id, so it still counts toward precision@10 (sheets and `relation-check` treat
  hop 0 as the seed). `--quiet` also prints `warmup_ok/warmup_total` and `runs_ok/runs_total` per
  variant.
- **`eval prepare --snapshot <dir>`** (required). `<dir>` must not exist and its parent,
  resolved through symlinks, must be outside the repo. `replay.json` stores per case the
  lead `outcome` (`ok|lead_off|http_error|transport_error|decode_error`; the three old
  variants stay `ok`), the HTTP `status` (still an int), and `items` (ids, `hop`, `links`,
  `authored_by_bot`, `bot_in_thread`, `has_title`, `summary_runes`, `last_activity_at`; no
  text), plus top-level `eligibility_sha256` and `snapshot_sha256`. Any non-`ok` lead
  outcome fails the whole run with `prepare: lead_<outcome> <case_id>` and writes nothing.
  The exact title and `lead_summary` text, author name and Slack id go to
  `<dir>/snapshot.json` (dir 0700, file 0600, exclusive create, written last, removed if a
  later step fails). It is confidential; never commit it. `build_rev` comes from the
  `X-Graph-Build-Rev` response header if the server sends one (it does not today), else the
  client-side `BUILD_REV` environment variable of the eval host (not the server's), else
  `unknown`; expect `unknown` in practice. `prepare` also fails with `prepare: seed_mismatch
  <case_id>` when a canonical seed (or its thread root) is not a hop-0 lead item.
- **`eval label-sheet --variant all|resolve-lead`.** `all` (default) is the previous
  union-of-variants sheet; its row heading is now `## [r<k>] <node_id>`. `--variant
  resolve-lead --mask --snapshot <dir> --out <dir>` builds masked sheets from the snapshot
  with one `redact.Pseudonymizer` per case (`person-N`, `email-N`, `phone-N`, `card-N`,
  `ref-N`), applied to the full text before the 200/600-rune cut, and writes
  `manifest.json` (`{case_id: [{r, node_id}]}`, ids only). It refuses, with a fixed category
  on stderr, unless the lead replay validates (`validateLeadReplay`: every case has an
  `ok` lead variant with 2xx status and well-formed `items`, and `eligibility_sha256`
  recomputes) and the snapshot, eligibility and case digests all match. Residual exposure:
  names in prose that are not mentions or authors, merchant/partner names, refs without a
  digit. A token after a label word (`booking`, `payment`, `order`, `pnr`, `bid`, `txn`,
  `transaction`, `ref`, `reference`) is always masked, even if it looks like a Jira key,
  `repo#n`, date or time.
- **Relation rubric.** `same_case | related_context | different_case | insufficient`, one
  reason code per row (`same_ref`, `same_ticket_or_pr`, `work_on_case`, `precedent`,
  `same_component`, `runbook`, `different_ref`, `different_incident`, `shared_words_only`,
  `empty_evidence`, `ambiguous`, `reviewers_disagree`). A detector should flag
  `different_case`; it should not flag `same_case` or `related_context` (two separate
  denominators); `insufficient` is excluded from truth denominators and counted as coverage
  loss.
- **`eval relation-assemble --draft <rows> --review <rows> [--owner <rows>] --out <file>`**
  takes model rows `{case_id: {node_id: {relation, reason}}}`, computes the header itself
  (header-named keys in a rows file are ignored), merges (agree, or `insufficient` /
  `reviewers_disagree` / `disputed`, owner rows win, missing rows `insufficient` /
  `empty_evidence` / `missing`) and creates the output exclusively with mode 0600.
  **`eval relation-check [--file testdata/eval/graph_context/relation_labels.json]`**
  validates header, digests against `replay.json` and the case files, exact case and item
  coverage, enums, the disputed/missing rule and seed consistency, and prints counts only.
  R1c must run it (or the same validation) before scoring.
- **Buckets** (each non-seed lead item gets one link bucket and one author bucket):
  `structural` (any PART_OF, REFERS_TO, REFERENCES link), else `same_topic_hi`
  (SAME_TOPIC, confidence >= 0.9), `same_topic_lo`, `other_link`, `no_link`; and
  `authored_by_bot` = `human|bot|unknown`. A bucket with fewer than 5 decisive
  `different_case` items is inconclusive for that bucket.
