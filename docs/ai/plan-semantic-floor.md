# Plan: cosine floor on the search semantic arm

REVIEWER: pi (auto: GPT weekly 57% left)

Revision 4 (2026-10-05). It answers pi reviews 1-3 (6, 7 and 4 findings). See "Review history" at the end.

## Problem

`graph_search q=tamara` returns five semantic-only hits that have nothing to do
with Tamara, mixed in with the 10 real keyword hits. Observed on the hub on
2026-10-05 (MCP, default mode):

| node | body | sem (cosine) | arm rank |
|---|---|---|---|
| slack:C09H1QMK882:1790058121.649719 | "exactly" | 0.6245 | semantic 1 |
| slack:C09TRPZRE4B:1783594503.900139 | "perfect" | 0.6242 | semantic 2 |
| slack:C05RNSE8TBR:1790774008.666339 | "Exactly" | 0.6210 | semantic 3 |
| slack:C04HZ6JB83V:1790907716.124109 | "Roxana approved!" | 0.6203 | semantic 4 |
| slack:C0BRJGC92KA:1790246293.272499 | "Exactly." | 0.6149 | semantic 5 |

All of them sit around 0.62, the baseline cosine between two short strings in
this embedding model. RRF uses rank only, so semantic rank 4 (0.0156) scores
about the same as a real keyword hit at rank 5 (0.0154). The dashboard search
page (`dashboard/src/api.ts:1292`, `match=hybrid`) folds each reply into its
thread root, so the user lands on an unrelated thread. The fold is correct. The
match is the bug.

**Full picture for `tamara`** (MCP default mode, `limit=50`, each Slack hit
folded to its thread root, and each thread checked for any message whose body
ILIKE '%tamara%'):

| arm(s) that found the thread | threads | threads that mention Tamara |
|---|---|---|
| semantic only | 19 | 0 |
| keyword only | 8 | 8 |
| keyword + semantic | 1 | 1 (the semantic part is the junk reply "Exactly") |

Every semantic hit in the top 50 is a short reply ("Okay", "hii", "@Supriya
yes", "got it. bye") with sem 0.602-0.625. No real Tamara content made it into
the semantic arm's top 28. The semantic arm adds nothing for this query and
causes 19 wrong threads.

The repo already uses a 0.65 floor for the same reason in `link_topics.go:22`
and `bfs/expand.go:102`.

## Goal

The semantic arm drops every hit whose cosine is below a fixed floor. A dropped
hit contributes no semantic rank or `sem` score. In default mode it also does
not seed the graph arm. This holds in default and hybrid mode.

What this does not promise: a below-floor node can still appear when another arm
returns it on its own merits. The guarantee covers semantic contribution and
seed eligibility only.

## Non-goals

- No change to keyword, temporal or graph arm logic.
- No new config key, env var, request parameter or dashboard setting. The floor
  is a Go constant.
- No adaptive mean+2σ cutoff.
- No change to embedding or indexing of short Slack replies.
- No change to the SQL / HNSW query. Filter in Go after the scan.
- The worker never deploys or merges, and never reads or looks for any API key,
  token or settings row. All hub access is read-only, through the two paths in
  "Inputs".

## Facts the worker can rely on (verified 2026-10-05)

- MCP `graph_search` (`internal/graphmcp/server.go`) calls search in **default
  mode**. It never sends `match=hybrid`. Default mode runs semantic, keyword,
  graph and (with a window) temporal arms.
- `match=hybrid` sets `wanted = {semantic, keyword}` only (`search.go:134-139`).
  Hybrid has no graph arm.
- A node's cosine to a query is the same in both modes, since both run
  `semanticRows`.
- `internal/graph/handlers/export_test.go` is package `handlers` and already
  exports test seams (`SetSearchNow`).
- Default-mode response rows carry `node_id`, `summary`, `score_breakdown.sem`
  and `score_breakdown.ranks`. For a short Slack reply the summary is its body.

## Files expected to change

- `internal/graph/handlers/search_arms.go`: floor constant, helper
  `aboveSemanticFloor`, used in `semanticArm`.
- `internal/graph/handlers/search_hybrid_arms.go`: use the helper in
  `semanticArmFolded` after the scan and **before** thread dedup and truncation.
- `internal/graph/handlers/export_test.go`: add
  `const SemanticMinCosine = semanticMinCosine` (the test seam; tests in
  `handlers_test` derive fixture cosines from it, so they never drift from F).
- New `internal/graph/handlers/search_semantic_floor_test.go`, package `handlers`,
  DB-free.
- New `internal/graph/handlers/search_semantic_floor_db_test.go`, package
  `handlers_test`, integration.
- New `docs/ai/eval-semantic-floor/`: `eval-set-10q.frozen.md`,
  `manifest.json`, `capture.py`, `compare.py`, `baseline-<ts>.json`.
- `internal/graph/handlers/search_hybrid_test.go`: only the fixture of
  `TestSearch_HybridKeywordTitleBoostOutranksSemantic` (line ~420 builds its
  semantic hit at cosine 0.5, which any F >= 0.65 drops). Raise that vector to an
  above-floor cosine derived from `handlers.SemanticMinCosine` (e.g. floor + 0.1,
  same `[c, sqrt(1-c^2)]` construction) so the test keeps asserting what it
  asserts today: keyword + semantic outranks semantic only. Do not change its
  assertions. Grep `internal/graph/handlers/*_test.go` for any other fixture whose
  semantic hit sits below F, and fix each one the same way, listing them in the
  report.
- `docs/ai/eval-semantic-floor/compare_test.py`.
- New `docs/ai/report-semantic-floor.md`.

## Inputs

- **Eval set, committed.** Copy
  `/Users/neocapitelo/go/src/github.com/agent-mem/docs/ai/eval-set-10q.md` (untracked
  on `main`) to `docs/ai/eval-semantic-floor/eval-set-10q.frozen.md`, unchanged,
  and commit it. From it write `manifest.json`: for each of Q1-Q10 the exact `q`
  text, any `since`/`until`, the expected ids exactly as listed (do not
  re-derive any, Q3 included), and the thread-fold rule
  `channel:(metadata.thread_ts or ts)`. Add the noise probes `tamara`, `tabby`,
  `juspay`, `roxana` with no expected ids. Its header says DRAFT. That doesn't
  matter: this is a regression check, and pre and post read the same committed
  manifest.
- **Hub search (default mode), no credentials:** the MCP stdio server inside the
  worker container,
  `ssh enzo@payments '/opt/homebrew/bin/docker exec -i agent-mem-worker-1 agent-mem mcp'`.
  Send JSON-RPC `initialize`, `notifications/initialized`, then one `tools/call`
  `graph_search` per query with `limit: 50` and the manifest's params. Use
  `graph_node` on the same connection for node bodies.
- **Thread keys for folding**, read-only SQL on the hub, nothing else:
  `ssh enzo@payments "/opt/homebrew/bin/docker exec agent-mem-postgres-1 psql -U agentmem -d agentmem -At -c \"SELECT id, scope, metadata->>'thread_ts' FROM graph.nodes WHERE id = ANY('{...}')\""`.
  Only `SELECT` on `graph.nodes`. No other table.
- No hybrid capture. Hybrid needs the HTTP API, which needs a credential the
  worker must not use. Hybrid is covered by integration tests. See "Known gap".

## Approach

1. **capture.py** (stdlib only). For every manifest query, runs `graph_search`
   over MCP and writes `{query, params, response, error}` per query, plus a UTC
   timestamp, the manifest sha256, and `--label` (pre/baseline/post). Thread keys
   for every returned Slack node come from the SQL above and go into the same
   file. Exit non-zero, and write no output file, if: any call errors; any eval
   query returns zero results; the thread-key lookup fails or misses any returned
   Slack node; or any response has an `arm_errors` entry other than the one
   allowed inactive condition, `temporal: "no time window in query or
   since/until"` on a query with no window. A failed semantic, keyword or graph
   arm always fails the capture.
2. **compare.py a.json b.json**. First validates both files: same manifest
   sha256, every manifest query present in each, no recorded error. If not, it
   exits 2. Then it prints, per eval query, expected ids found in the top 10
   (thread-folded) in a and in b, and lists any lost. For each noise node:
   present or absent in each file, and with which `ranks` arms. Exit 1 if any
   expected id is lost, else 0.
   **compare_test.py** (stdlib `unittest`, small hand-written JSON fixtures, no
   hub access) proves that: an unchanged pair exits 0; a reply id in b that
   folds to the same expected thread root exits 0; a lost expected id exits 1;
   a missing query, a manifest-sha mismatch, or a recorded error exits 2.
3. **Baseline.** Run `capture.py --label baseline` before any code change.
   Commit the output.
4. **Calibrate** from the baseline:
   - *E* (expected-good): rows at rank 1-10 of an eval query that fold to an
     expected id and whose `ranks` has `semantic`. Record node, query, rank, sem.
   - *N* (noise): every row of a noise-probe query whose `ranks` has only
     `semantic` and whose `summary` is under 20 characters. Plus any Problem-table
     node that appears in the baseline. A Problem node absent from the top 50 is
     listed as "not observed" and left out of N.
   - `E_min` = min sem over E. `N_max` = max sem over N.
   - **F** = the largest value in {0.65, 0.66, ..., 0.80} with
     `F <= E_min - 0.01` and `F > N_max`.
   - **STOP and report, no code, when:** E is empty; N has fewer than 3 rows; no F
     satisfies both inequalities; the baseline capture failed.
5. **Implement.** `const semanticMinCosine = F` in `search_arms.go`, with a
   comment quoting `E_min`, `N_max` and pointing to the report.
   `aboveSemanticFloor(hits []armHit) []armHit` keeps `Score >= semanticMinCosine`
   in order. Call it in `semanticArm` and in `semanticArmFolded` before dedup.
   Confirm in code that the graph arm's seeds are the semantic arm's returned hits
   (`search.go`, semantic goroutine), and say so in the report.
6. **Tests.** Fixture cosines come from `handlers.SemanticMinCosine`. Build a
   below-floor vector as `[c, sqrt(1-c^2), 0, ...]` against `spUnitVec()` with
   `c = SemanticMinCosine - 0.02`. Positive fixtures use cosine 1.
   - DB-free (package `handlers`): keeps a hit at exactly the floor, drops one at
     floor - 0.0001, keeps order, handles empty input.
   - Integration (package `handlers_test`), each case its own test:
     a. **both modes**: a node at cosine 1 is returned. A below-floor node with
        no keyword match is not.
     b. **both modes**: a below-floor node whose title keyword-matches the query is
        returned with `ranks.keyword` present, `ranks.semantic` absent, and
        `sem == 0`. The thread has no above-floor member.
     c. **default mode only**: a below-floor node with a REFERENCES edge to a
        neighbour does not bring that neighbour in. Control: the same shape with
        the seed at cosine 1 does return the neighbour. In hybrid, assert that
        the response's `arms` has no `graph`.
     d. **hybrid only**: a thread whose only semantic match is a below-floor reply
        is not returned. A thread with an above-floor reply is returned as its
        root id.
   - **They must run, not skip.** Start a throwaway `pgvector/pgvector:pg16`
     container on 127.0.0.1:5450 (5448 and 5449 belong to other workers), database
     `agentmem_test`, run `go run ./cmd/agent-mem migrate` against it, set
     `DATABASE_URL`. Never use the dev DB on 5433: tests truncate graph tables
     that sync to prod. Remove the container when done.
7. **Commit and push** `fix/semantic-floor`. No PR, no merge, no deploy.

## Acceptance criteria

1. `docs/ai/report-semantic-floor.md` has: manifest sha256, baseline file name,
   tables E and N (node, query, rank, sem), the "not observed" list, E_min,
   N_max, and F with both inequalities shown as numbers.
2. `manifest.json`, the frozen eval file, both scripts, `compare_test.py` and
   the baseline are committed. `python3 -m unittest compare_test` passes (output
   pasted), and `python3 compare.py baseline.json baseline.json` exits 0.
3. `go test -v -run SemanticFloor ./internal/graph/handlers/` output pasted: every
   test PASS, none SKIP.
4. `go build ./...`, `go vet ./...`, `go test ./internal/graph/...` (with the
   scratch `DATABASE_URL`) pass. Output pasted.
5. Diff touches only the files listed. No TODOs, no new `t.Skip`, no `.only`.
6. Report lists the rollout gate below as **pending, conductor**.

## Rollout gate (conductor, human-gated)

This is a **conservative live regression gate**, not an isolated causal
comparison. The corpus keeps ingesting between runs.

1. Record the previous identity: hub `git -C ~/go/src/github.com/agent-mem
   rev-parse HEAD` and `docker inspect -f '{{.Image}}' agent-mem-worker-1`.
2. `capture.py --label pre`.
3. Deploy the branch per CLAUDE.md (human approval first). Record the candidate
   commit and image id. Confirm the candidate is serving: the running
   container's image id equals the image just built
   (`docker inspect -f '{{.Image}}' agent-mem-worker-1` vs
   `docker compose images -q worker`), and it differs from the previous id. Also
   record `docker exec agent-mem-worker-1 sha256sum /usr/local/bin/agent-mem`,
   both before and after. The two sums must differ.
4. `capture.py --label post`. If it fails or is partial, rerun once. Two failures
   mean roll back.
5. `compare.py pre.json post.json`.
   - No expected id lost → keep.
   - Expected id lost → roll back to the recorded previous commit (`git checkout
     <sha>` + `docker compose up -d --build worker`), then `capture.py --label
     rollback`. If the id is still missing on the old build, it is corpus drift:
     report it and redeploy the candidate. If it is back, the floor caused it:
     stay rolled back and report.
   - For `tamara`, report each Problem node as absent, or present via which arm.

## Known gap

Hybrid recall (the dashboard search page) is checked by integration tests a, b
and d only, not against live data. A live hybrid capture needs the HTTP API key.
Running one is the human's call.

## Review history

pi review 1 (openai-codex gpt-6-astra), CHANGES REQUIRED, 6 findings:
response-table recall proof → pre/post capture; untracked DRAFT eval → frozen
copy; undefined calibration → E/N/F rule + STOP cases; thin skippable tests →
DB-free + integration cases that must run; overclaimed goal → narrowed; curl
without auth → MCP stdio.

pi review 2, CHANGES REQUIRED, 7 findings (applied in revision 3):
1. MCP is default mode, not hybrid → captures labelled default; hybrid is a
   documented gap.
2. Responses lack bodies/thread metadata → `summary` (= body for short replies),
   `graph_node`, and a read-only `graph.nodes` SELECT for thread keys; absent
   noise nodes listed "not observed"; N < 3 is a STOP.
3. Graph control can't pass in hybrid → case c default-only, hybrid asserts no
   graph arm; case d hybrid-only.
4. Keyword-survival test too weak → asserts keyword rank present, semantic rank
   absent, sem == 0, no above-floor thread member.
5. Unexported constant unreachable from `handlers_test` → exported in
   `export_test.go`.
6. Eval not delivered with the branch → frozen copy + manifest committed; both
   scripts read the manifest.
7. No deploy identity / rollback target → previous and candidate commit + image
   recorded, serving check, rollback to the recorded sha, drift vs regression
   rule, labelled a live regression gate.

pi review 3, CHANGES REQUIRED, 4 findings. Three review loops without approval, so
the conductor took it to the human, who chose to apply these and skip a 4th review:
1. HIGH: `search_hybrid_test.go` fixture at cosine 0.5 would break → that fixture
   (and any other below-F one) is raised above the floor, assertions unchanged.
2. Serving check relied on comment text → image id + binary sha256 before/after.
3. Degraded captures could pass → any arm error fails a capture, except the
   documented no-window temporal skip. Thread-key lookup gaps fail it too.
4. Comparator untested → `compare_test.py` fixtures for pass, fold-equivalent,
   lost, and invalid inputs.

## Revision 5: round 1 STOP resolved (human decision, 2026-10-05)

Round 1 (commit a6cfb7a, local only) stopped correctly: no F fit. The cause was
this plan's N rule. N_max came from "Roxana approved!" (q=roxana, 0.8466),
"I raised to Juspay" (q=juspay, 0.7840) and "Cc @Roxana @sourav" (q=roxana,
0.7493). All three contain the query word, so they are relevant, not noise.

**Corrected N rule:** a noise-probe row whose `ranks` has only `semantic`, whose
body is under 20 characters, **and whose body does not contain the probe word**
(case-insensitive). Problem-table nodes count only under the same test.

With the round 1 baseline: N_max = 0.6245 (tamara), E_min = 0.6672 (Q1). **F =
0.65**, chosen by the human. Margins are thin (0.025 over noise, 0.017 under the
weakest good hit). Say so in the floor's code comment.

**Captures must never store message text.** Round 1's baseline held full
`graph_node` bodies. One contained a Checkout.com test secret key, and GitHub
push protection rejected the push. Change `capture.py` so it stores only
`node_id`, `type`, rank, `score_breakdown` (sem + ranks), thread key, body
length, and `contains_query_word` (bool). It never stores `summary`, `title`,
`body` or any other free text. Compute length and the flag in memory, then
discard the text. compare.py and compare_test.py must work from those fields.

**Rewrite the unpushed history.** Commit a6cfb7a was never pushed. Remove the
old baseline from history (`git reset --soft origin/main` and recommit, or
amend), re-run `capture.py --label baseline` with the fixed script, and before
any push run
`git log -p origin/main..HEAD | grep -iE 'sk_(test|live)_|secret|password|token' || true`.
Paste the result, which must show no credential values. Never bypass push
protection.

Then continue the original Approach from step 4 with F = 0.65, recomputing
E/N from the new baseline with the corrected N rule. If the new baseline gives
E_min < 0.66 or N_max >= 0.65, STOP again and report.
