# Semantic floor — Revision 5

Metadata-only baseline passed the revised calibration gate. Human-selected **F = 0.65**.

- Manifest SHA256: `c591518361ac831d22e956890d7c2d627e728e7020ff60d5441f743f362d4011`.
- Baseline: `eval-semantic-floor/baseline-20261005T103359Z.json`.
- Capture timestamp: `2026-10-05T10:33:59.127788+00:00`; default mode, all 14 manifest queries.
- E rows: 19; N rows: 15.
- E_min: `0.6672237548798137`; N_max: `0.6245059801450124`.
- Good-hit inequality: `0.65 <= 0.6672237548798137 - 0.01 = 0.6572237548798137`.
- Noise inequality: `0.65 > 0.6245059801450124`.
- Thin margins: `0.017224` below weakest good hit; `0.025494` above strongest noise.

## Expected-good E

Original response ranks 1–10, folded to frozen expected IDs, with semantic rank.

| Node | Query | Rank | Sem |
|---|---|---:|---:|
| `slack:C08SVNFA30R:1787125337.685409` | Q1 | 2 | 0.6672237548798137 |
| `jira:PAY-2307` | Q1 | 4 | 0.7447724460132072 |
| `slack:CUV9EAYGY:1786442428.281449` | Q1 | 8 | 0.7021952014397438 |
| `jira:DES-1167` | Q4 | 1 | 0.7348316863046065 |
| `slack:C012A121AQJ:1786541785.540699` | Q4 | 4 | 0.712953205625953 |
| `slack:C012A121AQJ:1786703390.895489` | Q4 | 5 | 0.7068664793889847 |
| `slack:C0597404MS6:1787647035.383409` | Q4 | 8 | 0.7220559858736355 |
| `slack:C08S954G2LX:1788421850.770589` | Q5 | 4 | 0.7372619480031262 |
| `slack:C08S954G2LX:1784206256.540219` | Q5 | 5 | 0.7643135585322017 |
| `slack:C08S954G2LX:1784236855.699269` | Q5 | 10 | 0.7231102288548286 |
| `slack:C0BRJGC92KA:1788682872.344359` | Q6 | 2 | 0.8320658723487663 |
| `slack:C0BRJGC92KA:1790495331.206809` | Q6 | 6 | 0.727363392690618 |
| `gh_pr:wego/payments#2310` | Q9 | 2 | 0.8215949905601827 |
| `gh_pr:wego/payments#2282` | Q9 | 3 | 0.8181212652157189 |
| `gh_pr:wego/payments#2281` | Q9 | 5 | 0.7579221930092609 |
| `slack:C09H1QMK882:1790214660.013799` | Q10 | 3 | 0.7332455666315892 |
| `slack:C09H1QMK882:1790663419.346109` | Q10 | 4 | 0.7141794654155381 |
| `slack:C09H1QMK882:1787113625.698419` | Q10 | 6 | 0.741587502034443 |
| `slack:C09H1QMK882:1790646394.316749` | Q10 | 7 | 0.7158071511948183 |

## Corrected noise N

Noise-probe rows qualify only when ranks are semantic-only, body length is under
20 characters, and `contains_query_word` is false (case-insensitive substring).
Problem-table nodes obey the same test. Query-containing short replies are relevant,
not noise. No message text is persisted in the baseline or reproduced here.

| Node | Query | Rank | Sem |
|---|---|---:|---:|
| `slack:C05RNSE8TBR:1790774008.666339` | tamara | 3 | 0.6209506943634212 |
| `slack:C04HZ6JB83V:1790907716.124109` | tamara | 5 | 0.6203307846069611 |
| `slack:C09H1QMK882:1790058121.649719` | tamara | 7 | 0.6245059801450124 |
| `slack:C0BRJGC92KA:1790246293.272499` | tamara | 13 | 0.6148713732262092 |
| `slack:C09TRPZRE4B:1783594503.900139` | tamara | 15 | 0.6241666640581096 |
| `slack:C0BBJAHV4G1:1790145586.350519` | tamara | 18 | 0.6106007748882336 |
| `slack:CUV9EAYGY:1790645812.381419` | tamara | 20 | 0.606037634619819 |
| `slack:C0736FUE03W:1778666953.193559` | tamara | 31 | 0.6072699854536777 |
| `slack:C019B36KGNR:1783535677.368799` | tamara | 33 | 0.608163914649227 |
| `slack:C019B36KGNR:1784119062.826319` | tamara | 38 | 0.6065409301657675 |
| `slack:C05RNSE8TBR:1778229931.020679` | tamara | 39 | 0.6065034810945276 |
| `slack:C04E53S74E8:1776307570.406859` | tamara | 40 | 0.6033471237403323 |
| `slack:C01SVR5DY9J:1789013088.824239` | tamara | 43 | 0.6044178423885556 |
| `slack:C019B36KGNR:1785758788.504159` | tamara | 48 | 0.6050452250100817 |
| `slack:CDP50BYVD:1789705286.423449` | tamara | 49 | 0.6022356282233501 |

## Problem nodes not observed

None: all five Problem-table nodes were observed. Each qualifying occurrence is
included above; query-containing occurrences are excluded.

## Capture and history safety

Capture stores only node ID, type, response rank, semantic score and arm ranks,
thread key, body length and query-containment flag. Bodies are processed in memory
and discarded; summary, title, body and arbitrary response free text are never stored.
Every returned Slack node resolved through the permitted read-only graph.nodes SELECT.
673 captured rows passed an exact metadata-field allowlist check.

The old rejected baseline was deleted without opening it. `git reset --soft
origin/main` removed a6cfb7a from branch ancestry before recommitting. No push
protection bypass. Frozen eval/manifest remain unchanged, including Q3 expected IDs.

History marker scan after sanitized recommit used `git log -p origin/main..HEAD`
and the required case-insensitive regex through the search tool. Matching lines
(all documentation, no credential values):

```text
+  token or settings row. All hub access is read-only, through the two paths in
+`graph_node` bodies. One contained a Checkout.com test secret key, and GitHub
+`git log -p origin/main..HEAD | grep -iE 'sk_(test|live)_|secret|password|token' || true`.
+keys, tokens, settings rows, or dev database on 5433 were accessed.
```

## Implementation

The floor filters semantic candidates in place, preserving order and admitting the
exact boundary. Default search filters after scanning. Hybrid filters after scanning,
before thread deduplication and budget truncation. Keyword, temporal, graph logic,
SQL/HNSW query, and request/config contracts are unchanged.

`search.go:226–235` passes returned semantic hits directly to `graphArm`, so dropped
hits cannot seed graph expansion. Hybrid selects only semantic and keyword arms.

## Verification

Scratch: `pgvector/pgvector:pg16`, container `agentmem-sfl-pg-5450`, trust auth,
database `agentmem_test`, bound only to `127.0.0.1:5450`. Migrations passed.

The HTTP smoke scenario reproduced below-floor noise before arm cutover. New
integration cases failed before cutover for below-floor inclusion, semantic
contribution on keyword matches, graph seeding, and folded reply admission.
After cutover:

```text
$ go test -v -run SemanticFloor ./internal/graph/handlers/
=== RUN   TestSemanticFloorBoundaryAndOrder
--- PASS: TestSemanticFloorBoundaryAndOrder (0.00s)
=== RUN   TestSemanticFloorEmpty
--- PASS: TestSemanticFloorEmpty (0.00s)
=== RUN   TestSearch_SemanticFloorFiltersBothModes
=== RUN   TestSearch_SemanticFloorFiltersBothModes/default
=== RUN   TestSearch_SemanticFloorFiltersBothModes/hybrid
--- PASS: TestSearch_SemanticFloorFiltersBothModes (0.09s)
    --- PASS: TestSearch_SemanticFloorFiltersBothModes/default (0.06s)
    --- PASS: TestSearch_SemanticFloorFiltersBothModes/hybrid (0.02s)
=== RUN   TestSearch_SemanticFloorKeywordSurvivesBothModes
=== RUN   TestSearch_SemanticFloorKeywordSurvivesBothModes/default
=== RUN   TestSearch_SemanticFloorKeywordSurvivesBothModes/hybrid
--- PASS: TestSearch_SemanticFloorKeywordSurvivesBothModes (0.05s)
    --- PASS: TestSearch_SemanticFloorKeywordSurvivesBothModes/default (0.02s)
    --- PASS: TestSearch_SemanticFloorKeywordSurvivesBothModes/hybrid (0.03s)
=== RUN   TestSearch_SemanticFloorGraphSeeds
=== RUN   TestSearch_SemanticFloorGraphSeeds/below_floor
=== RUN   TestSearch_SemanticFloorGraphSeeds/cosine_one_control
--- PASS: TestSearch_SemanticFloorGraphSeeds (0.05s)
    --- PASS: TestSearch_SemanticFloorGraphSeeds/below_floor (0.02s)
    --- PASS: TestSearch_SemanticFloorGraphSeeds/cosine_one_control (0.02s)
=== RUN   TestSearch_SemanticFloorFoldedReplies
--- PASS: TestSearch_SemanticFloorFoldedReplies (0.02s)
PASS
ok github.com/agent-mem/agent-mem/internal/graph/handlers 0.911s
PASS HTTP default above-floor survives; noise absent; keyword-only sem=0
PASS HTTP hybrid above-floor survives; noise absent; keyword-only sem=0
```

The throwaway smoke program started an HTTP server with the real search handler,
used a fixed embedder against scratch data, and checked actual HTTP responses.
It was removed afterward.

Fixture update: `TestSearch_HybridKeywordTitleBoostOutranksSemantic` now uses
`SemanticMinCosine + 0.1`; assertions unchanged. No other below-floor search
fixtures found. Eligibility-gate vectors test ingestion, not search, and remain
intentionally unchanged.

```text
$ python3 -m unittest compare_test
............
----------------------------------------------------------------------
Ran 12 tests in 0.028s

OK
$ python3 compare.py baseline-20261005T103359Z.json baseline-20261005T103359Z.json
Q1–Q10: lost=[] (condensed output summary, not literal stdout)
exit 0; noise-node presence and rank arms printed for all four probes
$ go build ./...
exit 0; no output
$ go vet ./...
package github.com/agent-mem/agent-mem/internal/llmgateway
 imports github.com/agent-mem/agent-mem/internal/graph/handlers from client_test.go
 imports github.com/agent-mem/agent-mem/internal/llmgateway from subject_queries.go: import cycle not allowed in test
exit 1
```

Vet blocker is pre-existing: both cycle files are identical to `origin/main`
(`git diff --exit-code origin/main -- <both files>` exited 0). Removing the
test-only interface assertion and its handlers import would resolve that cycle,
but touches a file outside the plan's allowed list. Awaiting explicit scope
authorization rather than suppressing vet or silently widening the change.

The unrestricted graph-suite run also failed. Package tests mutate the same
scratch tables, so a serialized run was attempted, then the container was
recreated and migrated again to exclude leftover migration state. Final evidence:

```text
$ GOFLAGS='-p=1' DATABASE_URL=<scratch-5450> go test -count=1 ./internal/graph/...
--- FAIL: TestE2E_IngestContent_TRYThread (0.03s)
    e2e_test.go:159: POST 1 outcome = skipped_non_incident, want created
    e2e_test.go:165: graph.nodes row not found: no rows in result set
FAIL
FAIL github.com/agent-mem/agent-mem/internal/graph 0.319s
ok github.com/agent-mem/agent-mem/internal/graph/acl 0.516s
ok github.com/agent-mem/agent-mem/internal/graph/bfs 0.523s
ok github.com/agent-mem/agent-mem/internal/graph/entities 0.539s
ok github.com/agent-mem/agent-mem/internal/graph/extractor 0.528s
ok github.com/agent-mem/agent-mem/internal/graph/fetchers 0.923s
--- FAIL: TestImportBambooHR_CSVBytes_ParsesAndUpserts (0.02s)
    import_bamboohr_enqueue_test.go:50: expected 3 people rows, got 1
--- FAIL: TestIngestURL_AlreadyFresh (0.01s)
    ingest_url_test.go:142: outcome = "queued_for_fetch", want already_fresh
    ingest_url_test.go:145: expected 0 jobs for already_fresh, got 2
{"level":"warn","error":"unknown time zone Mars/Base","timezone":"Mars/Base","time":"2026-10-05T17:45:39+07:00","message":"invalid graph.temporal.timezone; using UTC"}
FAIL
FAIL github.com/agent-mem/agent-mem/internal/graph/handlers 47.617s
ok github.com/agent-mem/agent-mem/internal/graph/hydrate 0.816s
ok github.com/agent-mem/agent-mem/internal/graph/identity 0.545s
ok github.com/agent-mem/agent-mem/internal/graph/ids 0.400s
--- FAIL: TestE2E_StuckJobRecoveredAndCompleted (10.08s)
    e2e_test.go:60: janitor did not recover stuck job; calls=1
--- FAIL: TestHeartbeat_ExtendsLease (2.52s)
    heartbeat_test.go:29: no rows in result set
FAIL
FAIL github.com/agent-mem/agent-mem/internal/graph/jobs 60.781s
ok github.com/agent-mem/agent-mem/internal/graph/normalizer 0.452s
ok github.com/agent-mem/agent-mem/internal/graph/scoring 0.451s
ok github.com/agent-mem/agent-mem/internal/graph/temporal 0.377s
FAIL
```

These failures are outside the changed floor scenarios. Relevant E2E, ingestion,
and handler fixture files are unchanged from `origin/main`; job files were not
modified. The ingest E2E is rejected by the incident-only author guard before
search. No broad-gate failure was skipped, suppressed, or silently patched.
The scratch container was removed after verification. Work remains **blocked**
on permission to repair out-of-scope gates; no push yet.

## Tracking

`bd prime` succeeded, but `bd create` failed because the shared database lacks
issue_prefix. `bd init --prefix agent-mem` refused because the shared database is
already initialized. No forced initialization or shared tracker reconfiguration.

## Rollout gate

**pending, conductor**. No PR, merge or deployment. Live hybrid recall remains the
plan's documented gap; integration cases cover hybrid floor behavior. No real API
keys, tokens, settings rows, or dev database on 5433 were accessed.
