# Plan: windowed semantic search refills past the HNSW cap (agent-mem-82eb)

REVIEWER: pi (auto: GPT weekly 22% left)

Read `bd show agent-mem-82eb` in full first.

## Problem

`semanticRows` (`internal/graph/handlers/search_arms.go:93-109`) adds the time
window as a WHERE predicate (`windowEligibleSQLAt`) on an HNSW-ordered query.
pgvector's HNSW scan returns at most `hnsw.ef_search` candidates (default 40)
**before** the filter. When the window is narrow, most of those 40 fall outside
it, so the semantic arm returns very few rows or none. Nothing in `internal/`
sets `hnsw.iterative_scan` or `hnsw.ef_search` today. The hub runs **pgvector
0.8.6** (checked 2026-10-05), which supports iterative index scans.

## Goal

When a time window is set, the semantic arm keeps scanning the index until it
has `limit` rows that pass the filter, or reaches a bounded scan size. Results
stay strictly ordered by distance, and the semantic floor (`semanticMinCosine`,
0.65) still applies. Queries without a window are unchanged.

## Approach

- Only when `f.win != nil`: run the query in a short read-only transaction and
  set, inside it:
  `SET LOCAL hnsw.iterative_scan = strict_order;` and
  `SET LOCAL hnsw.max_scan_tuples = <const, 20000>;`.
  Use `strict_order`, not `relaxed_order`, so the order stays correct and the
  floor's early-stop logic is unaffected. Read the pgvector 0.8 docs for these
  GUCs (`hnsw.iterative_scan`, `hnsw.max_scan_tuples`, `hnsw.scan_mem_multiplier`)
  and cite them in the report.
- `semanticRows` returns `pgx.Rows`, and its callers scan and close them
  (`semanticArm`, `semanticArmFolded`). A transaction has to outlive the rows.
  Restructure so the windowed path scans the rows inside the transaction and
  returns `[]armHit` (or the folded hits) after commit/rollback. Keep the
  no-window path exactly as it is today, with no transaction and no SET. Keep
  the change local to `search_arms.go` / `search_hybrid_arms.go`.
- Check whether `temporalArm` uses the same windowed-HNSW pattern (grep for
  `<=>` under `internal/graph/handlers/`). If it does, give it the same
  treatment. If not, say so.
- No change to the floor, fusion, or the API.

## Tests (scratch DB, must PASS, not SKIP)

1. Seed 60 nodes whose embeddings are closer to the query than one in-window
   node, all outside the window, plus 3 in-window nodes above the floor. A
   windowed semantic search returns all 3 in-window nodes. Red on
   `origin/main`: paste it showing fewer than 3.
2. The same search with no window returns the closest nodes overall, and the
   SQL runs without a transaction (assert via behaviour, or a test seam that
   records whether the windowed path ran).
3. Ordering: windowed results come back in non-increasing cosine order.
4. Run the existing `SemanticFloor*` and window/temporal handler tests;
   they must still pass.

Scratch DB: throwaway `pgvector/pgvector:pg16` on a free port. Check that its
pgvector version is ≥ 0.8 (`SELECT extversion FROM pg_extension WHERE
extname='vector'`) and paste it. Database `agentmem_test`, `go run
./cmd/agent-mem migrate`, then `make test-db TEST_DATABASE_URL=<scratch>`.
Never the dev DB on 5433 or the hub.

## Eval (conductor, after deploy, human-gated)

`docs/ai/eval-semantic-floor/` on main has `capture.py`, `compare.py` and the
manifest. Q7 is windowed. Run `capture.py --label pre` before deploy and
`--label post` after, then `compare.py`. No expected id may be lost, and Q7's
semantic hit count should rise. The worker adds a line to the report stating
which manifest queries are windowed.

## Rules

- Branch `fix/windowed-semantic` off `origin/main`. Commit and push. No PR,
  merge or deploy.
- No API keys, tokens or settings reads.
- **No full-vector GROUP BY / DISTINCT / ORDER BY on the hub, ever.** One OOM-
  killed the hub Postgres on 2026-10-05. The worker doesn't touch the hub at all.

## Acceptance criteria

1. Tests 1-4 pass (outputs pasted), and test 1's red run is pasted.
2. `go build ./...` and `go vet ./...` are clean. `make test-db` passes, with
   only the 3 documented skips.
3. The no-window path is byte-for-byte unchanged in behaviour (test 2).
4. `docs/ai/report-windowed-semantic.md`: the design, the pgvector doc
   citations, the temporalArm answer, and the windowed manifest queries.
5. Push `fix/windowed-semantic`.

## Revision 2 (answers pi review 1, 3 findings). Overrides the sections above where they differ.

1. **Scratch binding.** Before any migrate or test command, export
   `DATABASE_URL` and `AGENT_MEM_TEST_DATABASE_URL` set to the scratch DSN, and
   check it: host `127.0.0.1`, the port you picked, database `agentmem_test`.
   Abort otherwise. Run migrations as
   `DATABASE_URL=<scratch> go run ./cmd/agent-mem migrate`, never relying on
   config.
2. **The fixture must really use HNSW.** At 63 rows the planner may pick an
   exact scan, which passes before the fix. In the new test file:
   - make the planner choose the index: `ALTER DATABASE agentmem_test SET
     enable_seqscan = off` in setup, with `RESET` in `t.Cleanup`, opening the
     test's pool **after** the ALTER so new connections inherit it. Or use
     another documented way that you verify;
   - leave `hnsw.ef_search` at its default (40) and keep iterative scan off on
     the no-fix path;
   - paste `EXPLAIN` output for the arm's actual windowed query showing
     `idx_artifact_index_embedding`;
   - seed more than 40 out-of-window nodes closer to the query than the in-window
     ones (60 is fine), so the default scan truly misses.
   Do the red/green refill and ordering checks for **both** `semanticArm` and
   `semanticArmFolded`. Don't reuse `search_window_arms_test.go:65-69`, which
   disables index scans.
3. **Eval measurement.** "Semantic hit count" means the number of Q7 final
   results whose `score_breakdown.ranks` contains `semantic`, counted from
   `pre-*.json` and `post-*.json`. The conductor reports it as information only.
   It doesn't gate the deploy, because live data may legitimately show no
   increase. The gate stays `compare.py` exiting 0 (no expected id lost). The
   deterministic proof of the refill is test 1.

## Revision 3 (answers pi review 2, 1 finding)

**Transaction cleanup.** The windowed path is `tx, err := db.BeginTx(ctx,
pgx.TxOptions{AccessMode: pgx.ReadOnly})`, then `defer tx.Rollback(context.Background())`
(a no-op after commit), then `SET LOCAL ...`, query, scan all rows, `rows.Err()`,
`tx.Commit(ctx)`. Every error (begin, set, query, scan, rows.Err, commit) is
returned to the caller, never swallowed. `SET LOCAL` ends with the transaction,
so nothing leaks to the next use of the connection. Tests, on a pool with
`MaxConns = 1`, for **both** `semanticArm` and `semanticArmFolded`:
(a) success; (b) error, using a vector of the wrong dimension so the query
fails, and asserting the error is returned; (c) cancellation, with a ctx
cancelled before the call, asserting the error. After each one, on the same
1-connection pool: `SHOW hnsw.iterative_scan` returns `off`, and an
**unwindowed** semantic search succeeds. A leaked connection or a leaked setting
fails the test, and with one connection a leak would hang, so give the test a
5 s timeout.

## Revision 4: final (answers pi review 3; approved by Enzo 2026-10-05 without a 4th review). Overrides R3 test (c).

**Cancellation inside the transaction.** Add a package-level test hook,
`var afterSetLocalHook func()` (nil in production, set only by tests), called
right after the `SET LOCAL` statements and before the query. Test (c), for both
arms: the hook cancels the call's ctx. Assert the arm returns an error wrapping
`context.Canceled`. Then, with **fresh** contexts that each have a deadline,
on the same 1-connection pool: `SHOW hnsw.iterative_scan` returns `off`, and an
unwindowed search succeeds. The pre-cancelled-ctx case can stay as an extra
test, but it doesn't count as the cleanup proof.
