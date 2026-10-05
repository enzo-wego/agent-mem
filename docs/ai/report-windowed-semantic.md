# Windowed semantic search: bounded HNSW refill

## Design

Implements `plan-windowed-semantic.md`, including revisions 2–4, for
`agent-mem-82eb`. Only explicit arm windows (`searchFilter.win != nil`) use a
read-only transaction. Both `semanticArm` and `semanticArmFolded` execute:

```sql
SET LOCAL hnsw.iterative_scan = strict_order;
SET LOCAL hnsw.max_scan_tuples = 20000;
```

The transaction owns the query and row scanning. Scanners close the rows and
return `rows.Err()` before commit. Begin, SET, query, scan, row-stream, and commit
errors propagate; deferred `Rollback(context.Background())` covers every failure
and is a no-op after commit. The test-only `afterSetLocalHook` cancels inside the
transaction, after both SETs and before querying.

No-window calls retain the same query, arguments, scan order, floor, and folding
behaviour, and query the pool directly, without BEGIN or SET. The folded scanner
was extracted without changing its scan logic; its existing 3× fetch, first-row
per thread folding, and budget truncation remain unchanged. The cosine floor
remains 0.65. Fusion and API contracts are unchanged.

## pgvector documentation and limits

Primary source: [pgvector iterative index scans](https://github.com/pgvector/pgvector#iterative-index-scans)
and [iterative scan options](https://github.com/pgvector/pgvector#iterative-scan-options).
Iterative scanning is available starting in 0.8.0. Filters are applied after the
approximate index scan; iterative scanning continues until enough results are
found or a scan bound is reached. `strict_order` preserves exact distance order;
`relaxed_order` does not. The 20,000 tuple bound is approximate and does not bound
the initial scan. Recall remains approximate, not a guarantee of all eligible
rows in arbitrarily selective windows.

`hnsw.scan_mem_multiplier` limits memory as a multiple of `work_mem` (default 1).
The docs suggest increasing it if raising the tuple bound does not improve recall.
This change deliberately leaves it unchanged; it does not raise the memory budget.
`hnsw.ef_search` also remains at its default 40.

## Temporal arm

`temporalArm` computes cosine through a CASE expression over a LEFT JOIN, then
orders by `cosine DESC, at DESC`. It is not the index-compatible
`ORDER BY ai.embedding <=> $1` pattern and does not use the capped HNSW scan.
No temporal-arm change is required. Handler distance-expression search found the
semantic query and this temporal scoring expression as the retrieval-arm uses.

## Isolated database

Container: `fix-windowed-semantic`, image `pgvector/pgvector:pg16`, bound only to
`127.0.0.1:55482`, database `agentmem_test`. `docker ps` was checked immediately
before container creation. Both `DATABASE_URL` and `AGENT_MEM_TEST_DATABASE_URL`
were explicitly exported and equality-checked against the scratch DSN before
migrations and test commands. No dev database or hub access.

```text
CREATE EXTENSION
 extversion
------------
 0.8.2
(1 row)

INF Running migrations dir=./migrations
INF Migrations applied
```

Tests use a fresh one-connection pool with connection-local `enable_seqscan=off`
and `enable_sort=off`, rather than changing database defaults. The latter also
discourages a filtered node-first plan plus exact sort. EXPLAIN of the actual
query and bound arguments recorded by a pgx tracer asserts
`idx_artifact_index_embedding` for both arms. The fixture asserts
`hnsw.ef_search=40` and `hnsw.iterative_scan=off` before each run; it seeds 60
closer outsiders (cosines 0.99 down to 0.931) and three insiders (0.84, 0.82,
0.80). The planner controls die with the test pool and cannot affect other tests.

## Red evidence on origin/main

Before production edits, both HEAD and origin/main were
`e2b1b276906369b35159460463c8fd5d8e5b7038`.
Command: `go test -count=1 -v ./internal/graph/handlers -run '^TestWindowedSemanticRefill$'`.
Selected literal output (EXPLAIN excerpts omit only the lengthy vector and
eligibility subplans):

```text
=== RUN   TestWindowedSemanticRefill/semanticArm
        Limit  (cost=256.61..470.54 rows=3 width=43)
          ->  Nested Loop  (cost=256.61..3679.44 rows=48 width=43)
                ->  Index Scan using idx_artifact_index_embedding on artifact_index ai  (cost=256.47..265.10 rows=63 width=37)
    search_windowed_semantic_test.go:94: windowed hits: []
    search_windowed_semantic_test.go:95: windowed semantic hits=0, want 3
=== RUN   TestWindowedSemanticRefill/semanticArmFolded
        Limit  (cost=472.61..1116.15 rows=9 width=75)
          ->  Nested Loop  (cost=472.61..3904.82 rows=48 width=75)
                ->  Index Scan using idx_artifact_index_embedding on artifact_index ai  (cost=472.47..485.10 rows=63 width=37)
    search_windowed_semantic_test.go:94: windowed hits: []
    search_windowed_semantic_test.go:95: windowed semantic hits=0, want 3
--- FAIL: TestWindowedSemanticRefill (0.42s)
    --- FAIL: TestWindowedSemanticRefill/semanticArm (0.22s)
    --- FAIL: TestWindowedSemanticRefill/semanticArmFolded (0.19s)
FAIL
FAIL github.com/agent-mem/agent-mem/internal/graph/handlers 0.924s
```

## Green refill, order, and cleanup evidence

Command: `go test -count=1 -v ./internal/graph/handlers -run '^TestWindowedSemantic'`.
Both actual query plans still use HNSW:

```text
semanticArm:
        Limit  (cost=472.61..686.54 rows=3 width=43)
          ->  Nested Loop  (cost=472.61..3895.44 rows=48 width=43)
                ->  Index Scan using idx_artifact_index_embedding on artifact_index ai  (cost=472.47..481.10 rows=63 width=37)
semanticArmFolded:
        Limit  (cost=472.61..1116.15 rows=9 width=75)
          ->  Nested Loop  (cost=472.61..3904.82 rows=48 width=75)
                ->  Index Scan using idx_artifact_index_embedding on artifact_index ai  (cost=472.47..485.10 rows=63 width=37)
--- PASS: TestWindowedSemanticRefill (0.40s)
    --- PASS: TestWindowedSemanticRefill/semanticArm (0.22s)
    --- PASS: TestWindowedSemanticRefill/semanticArmFolded (0.18s)
--- PASS: TestWindowedSemanticCleanup (0.51s)
    --- PASS: TestWindowedSemanticCleanup/semanticArm (0.25s)
        --- PASS: TestWindowedSemanticCleanup/semanticArm/success (0.01s)
        --- PASS: TestWindowedSemanticCleanup/semanticArm/wrong_dimension (0.00s)
        --- PASS: TestWindowedSemanticCleanup/semanticArm/cancel_inside_transaction (0.00s)
    --- PASS: TestWindowedSemanticCleanup/semanticArmFolded (0.27s)
        --- PASS: TestWindowedSemanticCleanup/semanticArmFolded/success (0.01s)
        --- PASS: TestWindowedSemanticCleanup/semanticArmFolded/wrong_dimension (0.00s)
        --- PASS: TestWindowedSemanticCleanup/semanticArmFolded/cancel_inside_transaction (0.00s)
PASS
ok github.com/agent-mem/agent-mem/internal/graph/handlers 1.630s
```

Both arms returned insider IDs `ordinary:inside:00`, `:01`, `:02` with observed
cosines `0.8400022803662356`, `0.8199883578135139`, `0.7998827857952571`, in that
order. The small differences from seeded values are halfvec quantization.
Unwindowed calls returned `ordinary:outside:00`, `:01`, `:02` with cosines
`0.9899982511655265`, `0.9889900228411358`, `0.9880003990369137`; the query tracer
recorded `transactions=0` for both arms.

Cleanup tests cover success, wrong-dimension query errors, and cancellation by
the hook inside the transaction for each arm. Cancellation is checked with
`errors.Is(err, context.Canceled)`. After every scenario, fresh deadline-bound
contexts on the same MaxConns=1 pool observe `SHOW hnsw.iterative_scan = off`
and a successful unwindowed search returning the closest outsider. Calls and
recovery checks each have a five-second deadline.

## Conductor-only eval

Explicitly windowed manifest queries are **Q7** (`since=2026-09-28`,
`until=2026-10-05`) and **Q8** (`since=2026-09-01`, `until=2026-09-08`). In the
manifest's default mode, Q1 ("August 2026") and Q5 ("since July") also contain
recognized natural-language windows; these do not set `searchFilter.win`, which
is reserved for explicit windows. No other manifest query carries since/until.

The worker does not capture against the hub, create a PR, merge, or deploy.
The conductor's human-gated pre/post capture and `compare.py` exit 0 (no expected
ID lost) remain the deployment gate. Q7 semantic-hit counts are the numbers of
final results whose `score_breakdown.ranks` contains `semantic`, counted in the
pre/post JSON captures; any increase is informational, not a deployment gate.

## Initial verification STOP and resolution

`go build ./... && go vet ./...` exited 0.
The isolated regression run above passed, but the subsequent full
`make test-db TEST_DATABASE_URL=<scratch>` run exited 2. The new tests failed
after earlier handler tests ran:

```text
TestWindowedSemanticRefill/semanticArm:
    windowed semantic hits=0, want 3
TestWindowedSemanticRefill/semanticArmFolded:
    windowed semantic hits=0, want 3
TestWindowedSemanticCleanup/semanticArm/success:
    success hits=[] err=<nil>
TestWindowedSemanticCleanup/semanticArmFolded/success:
    success hits=[] err=<nil>
```

Wrong-dimension and cancellation recovery checks also returned no unwindowed
hits in the full-suite run. Both actual query EXPLAINs still showed HNSW.
The only skips were `TestDescribeAttachmentHandler_UnsupportedMime`,
`TestParseDocument_RichPDF`, and `TestTopicJudgeGolden`.

Implementation execution stopped on this verification failure, as required.
The conductor authorized diagnosis and a test-only repair before resuming.

### Root cause: physical HNSW state survives DELETE-based fixture resets

The fixture already used `winReset` / `truncateGraphHandlerTables`. The standard
external-package `testDB` / `truncateHandlerTables` used by hybrid and floor
tests has the same relevant behaviour: both reset helpers issue `DELETE`, not
`TRUNCATE`, against `graph.artifact_index`. They remove logical rows but leave
dead entries in the approximate index until index maintenance.

The instrumented full-suite run compared raw unwindowed semantic rows before
the floor against an exact count using the same joined tables and `f.sql`.
All 63 fixture embeddings were SQL-visible, but HNSW returned zero rows:

```text
diagnostic raw unwindowed HNSW rows=0; exact eligible=63;
predicate inputs=[]interface {}{interface {}(nil), interface {}(nil), interface {}(nil), false}
```

This rules out the cosine floor, window predicate, ACL, type, and epic
visibility as the cause of the zero-row result. NULL type/scope/epic inputs
disable those optional filters. Search boost and timezone settings do not
participate in this direct arm query; no settings values were read. Tests
verified `hnsw.ef_search=40` and `hnsw.iterative_scan=off` on fresh connections.
The handler tests contain no persistent ALTER DATABASE / ALTER ROLE HNSW
overrides.

In a second diagnostic full-suite run, rebuilding only the HNSW index, with
the same live rows, vectors, and predicate inputs, changed the raw result:

```text
diagnostic raw unwindowed HNSW rows=0; exact eligible=63;
predicate inputs=[]interface {}{interface {}(nil), interface {}(nil), interface {}(nil), false}
diagnostic after REINDEX: raw unwindowed HNSW rows=3;
predicate inputs unchanged=[]interface {}{interface {}(nil), interface {}(nil), interface {}(nil), false}
```

That diagnostic full-suite run passed. The index-state dependency, rather than
filter state, caused the isolated/full-suite difference. pgvector documents
[dead tuples as another cause of reduced HNSW result counts](https://github.com/pgvector/pgvector#why-are-there-less-results-for-a-query-after-adding-an-hnsw-index).
An unwindowed query returning zero therefore does not exclude HNSW starvation.

The permanent repair is only in `search_windowed_semantic_test.go`: immediately
after `winReset`, rebuild the empty `graph.idx_artifact_index_embedding` before
seeding. This makes recall independent of earlier fixture churn and autovacuum
timing. No global reset helper, product code, HNSW search setting, visibility
predicate, or floor was changed to solve this test failure. Temporary diagnostic
queries were removed before final verification.

### Standalone runtime smoke

A throwaway Go executable called the actual `semanticArm` and
`semanticArmFolded` against the scratch database, with a deterministic 3072-dim
query vector and 60 closer outsiders plus three insiders. It verified both the
windowed and unwindowed paths on a one-connection pool:

```text
semanticArm runtime insiders: [{smoke:windowed:60 0.8400022803662356 2026-09-30 07:00:00 +0700 +07 } {smoke:windowed:61 0.8199883578135139 2026-09-30 07:00:00 +0700 +07 } {smoke:windowed:62 0.7998827857952571 2026-09-30 07:00:00 +0700 +07 }]
semanticArm runtime unwindowed closest=smoke:windowed:00; pooled iterative_scan=off
semanticArmFolded runtime insiders: [{smoke:windowed:60 0.8400022803662356 2026-09-30 07:00:00 +0700 +07 smoke:windowed:60} {smoke:windowed:61 0.8199883578135139 2026-09-30 07:00:00 +0700 +07 smoke:windowed:61} {smoke:windowed:62 0.7998827857952571 2026-09-30 07:00:00 +0700 +07 smoke:windowed:62}]
semanticArmFolded runtime unwindowed closest=smoke:windowed:00; pooled iterative_scan=off
```

The executable also confirmed that the existing temporal parser recognizes Q1
and Q5's natural-language windows. No network embedder, API token, settings
read, or hub query was involved. Its fixture rows and executable seam were
removed before final verification and are not committed.

## Quality gates

Both successive `make test-db TEST_DATABASE_URL=<scratch>` runs passed with
exactly the three documented skips. Counts below are computed from the
runner's JSON events; passing test events include subtests:

```text
run 1: 14 packages passed; 1470 test/subtest pass events; 0 failures
run 2: 14 packages passed; 1470 test/subtest pass events; 0 failures
skips in each run:
  TestDescribeAttachmentHandler_UnsupportedMime
  TestParseDocument_RichPDF
  TestTopicJudgeGolden
```

After replacing per-query formatting of the fixed scan-bound statement with
an identical SQL string constant, a fresh targeted run exercised the final
sources:

```sh
go test -count=1 -v ./internal/graph/handlers \
  -run 'SemanticFloor|WindowedSemantic|WindowArms|WindowFilter|Temporal'
go build ./...
go vet ./...
```

Selected literal output:

```text
--- PASS: TestSemanticFloorBoundaryAndOrder (0.00s)
--- PASS: TestSemanticFloorEmpty (0.00s)
--- PASS: TestWindowArms_SemanticRefill (0.07s)
--- PASS: TestWindowArms_SemanticFoldedRefill (0.07s)
--- PASS: TestWindowArms_HandlerDefault (0.26s)
--- PASS: TestWindowArms_HandlerHybrid (0.25s)
--- PASS: TestWindowFilter_Arms (0.12s)
--- PASS: TestWindowedSemanticRefill (0.30s)
--- PASS: TestWindowedSemanticCleanup (0.30s)
--- PASS: TestTemporalArm_EpicInheritanceEquivalence (0.04s)
--- PASS: TestTemporalArm_ActiveEpicPlanOnce (2.53s)
--- PASS: TestSearch_SemanticFloorFiltersBothModes (0.06s)
--- PASS: TestSearch_SemanticFloorKeywordSurvivesBothModes (0.05s)
--- PASS: TestSearch_SemanticFloorGraphSeeds (0.06s)
--- PASS: TestSearch_SemanticFloorFoldedReplies (0.03s)
PASS
ok github.com/agent-mem/agent-mem/internal/graph/handlers 5.962s
```

Build and vet both exited 0 with no output. The scratch-only runtime smoke,
baseline red run, strict-order green run, and cleanup scenarios above provide
the behavioural evidence; no hub eval or deployment is claimed.

The two-run full-suite gate was repeated against the final SQL-constant
sources after the targeted run. Both runs again exited 0 with the same
14 passing packages, 1470 test/subtest pass events, zero failures, and exactly
the three skips listed above.
