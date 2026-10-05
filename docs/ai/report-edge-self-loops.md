# Edge self-loop implementation report

## Status: revision 2 implemented

Steps 2–5 and vm42 implemented on `fix/edge-self-loops`; verification below. The updated plan was copied verbatim. No PR, merge, deploy, dev/hub DB access, credential/settings reads, or push-protection bypass. No bd issues closed; `bd show agent-mem-vm42` returned no matching issue in this worktree.

## Sync safety findings

### Can a client push a deleted edge back to the hub?

**Yes through the hub's supported push payload; not through the current client's automatic push loop.**

- `internal/sync/engine.go:155-173`: the current engine deliberately pushes only flat-memory tables. It does not collect or send graph edges. Current replicas therefore do not automatically resurrect deleted hub edges.
- `internal/sync/engine.go:33-34`: `SyncPushPayload` nevertheless supports `GraphNodes` and `GraphEdges`.
- `internal/worker/sync_handlers.go:63-76`: the hub still imports graph nodes and graph edges supplied in a push. There is no self-loop filter or deletion-history check.
- `internal/database/graph_sync.go:492-506`: `ImportGraphEdge` executes an `INSERT ... ON CONFLICT DO NOTHING` carrying the incoming sync identity. After a physical deletion, the deleted row no longer supplies a conflict, so an otherwise valid incoming copy can be inserted again. This is a code-path inference, not an observed production event.
- `internal/database/graph_sync.go:222-228`: the remaining unsynced-edge collector selects `sync_version = 0`; it is not called by the current push loop.

The deployed versions and behavior of other clients were not inspected. Assuming all clients are current pull-only replicas would be an additional deployment prerequisite, not evidence supplied by this worktree. Under the plan's unqualified condition that a deleted self-loop could return through sync, the still-supported hub import path triggers STOP.

### Does deletion need a tombstone?

- `internal/database/graph_sync.go:42-54`: edge transport has no deletion field.
- `internal/database/graph_sync.go:671-696`: edge pull transports existing rows with `id > afterID`, excluding the requesting machine; a physical deletion is not transported.
- `internal/sync/engine.go:294-295,319-320`: clients import returned edges and advance the edge cursor; no deletion reconciliation occurs here.

A tombstone is not needed merely to prevent the current pull-only engine from uploading its cached edge: that engine does not upload graph data. But a physical DELETE alone neither prevents a graph-capable push client from restoring a row nor tells existing replicas to remove their cached copies. A deletion protocol or removal/rejection of graph-edge pushes would be needed to cover those cases without relying on client deployment assumptions. Neither change is in this plan's allowed files.

### Does a CHECK rejection fail loudly or break a sync batch?

- `internal/database/graph_sync.go:497-506`: a CHECK violation is returned by the database import; `ON CONFLICT DO NOTHING` does not absorb CHECK violations.
- `internal/worker/sync_handlers.go:70-76`: the hub counts a rejected edge and continues with the other rows. Imports are individual pool operations, not one batch transaction.
- `internal/worker/sync_handlers.go:118-124`: the hub logs aggregate received/rejected counts and returns those counts in JSON with the normal successful HTTP response. It does not return the SQL error or rejected row identity.
- `internal/sync/engine.go:181-205`: the current client considers an HTTP 200 push successful, marks its submitted flat-memory rows synced, and logs received count; it does not treat the response's rejected count as an error.

Thus the constraint does not abort the hub's entire push batch. Rejection is visible as an aggregate hub log/response counter, but quiet at the current client's success/error boundary. Graph-capable older/custom client handling is unknown. The constraint would prevent restoration in the hub once installed, but the plan explicitly requires stopping when sync can restore the physically deleted row, rather than silently depending on that constraint to resolve the sync policy.


### Revision 2 resolution

Resolved by the conductor: the CHECK constraint plus migrations at startup cover restoration and replica cleanup. `internal/worker/server.go:92` runs migrations at startup. Delete precedes constraint installation. Incoming self-loops are rejected individually rather than aborting the push batch. The earlier STOP discussion above records round 1, not a remaining blocker.

Follow-up (out of scope): the client ignores hub push `rejected` counts (`internal/sync/engine.go:181-205`).

## Implementation

- `reconcileEdges` skips equal endpoints before either upsert. Extractor and pruning unchanged.
- Guard regression exercises own-plus-other and own-only bodies, drops the CHECK and restores it in cleanup. Separate direct INSERT test checks SQLSTATE 23514 and constraint name.
- Generator: `make migrate-create name=delete_edge_self_loops`, then `make migrate-create name=edges_no_self_loop`. Initial generator invocations landed in the same second; the unused CHECK template was removed and regenerated to obtain distinct ordered versions.
- Delete migration `20261005111634`; CHECK migration `20261005111701`. Delete down is explicitly irreversible/no-op.
- SemanticFloor skips only absent DATABASE_URL; database name must be `agentmem_test`, with no host/port pin.

## Scratch target and checks

`docker ps` showed only dev Postgres on 5433; bound throwaway `pgvector/pgvector:pg16` container `esl-scratch-5452` to `127.0.0.1:5452`, database `agentmem_test`. All DB commands/tests below target it, except the intentional SemanticFloor `/postgres` safety rejection on the same scratch port, before connection. Container removed after verification.

Full handler run failed only `TestImportBambooHR_CSVBytes_ParsesAndUpserts` and `TestIngestURL_AlreadyFresh`; both also failed on `origin/main` (`73b9b64`). TRYThread was not reported as a failure. Baseline main ran on the same disposable database after branch migrations, so its artifact migration tests additionally failed on migration-version/schema assumptions (`TestArtifactTSV_UpLockBounded`, `TestArtifactTSV_PopulatedUpgrade`); these did not fail in the branch run. Main also rejected port 5452 in all four SemanticFloor DB tests, the vm42 bug fixed here. No unrelated failures were fixed.

`go build ./...` exit 0. `go vet ./...` hits pre-existing llmgateway→handlers→llmgateway test import cycle (`agent-mem-hurt`).

## Pasted command outputs

### initial_migrate

```text
[90m6:15PM[0m [32mINF[0m [1mRunning migrations[0m [36mdir=[0m./migrations
[90m6:15PM[0m [32mINF[0m [1mMigrations applied[0m
EXIT 0
```

### guard_red

```text
=== RUN   TestReconcileEdges_NoSelfLoop
=== RUN   TestReconcileEdges_NoSelfLoop/self_and_other
    edge_self_loop_test.go:34: returned 2 edge IDs, want 1
    edge_self_loop_test.go:37: edges total=2 self=1 other=1, want 1/0/1
=== RUN   TestReconcileEdges_NoSelfLoop/only_self
    edge_self_loop_test.go:34: returned 1 edge IDs, want 0
    edge_self_loop_test.go:37: edges total=1 self=1 other=0, want 0/0/0
--- FAIL: TestReconcileEdges_NoSelfLoop (0.06s)
    --- FAIL: TestReconcileEdges_NoSelfLoop/self_and_other (0.01s)
    --- FAIL: TestReconcileEdges_NoSelfLoop/only_self (0.01s)
FAIL
FAIL	github.com/agent-mem/agent-mem/internal/graph/handlers	0.796s
FAIL
EXIT 1
```

### seed

```text
ALTER TABLE
INSERT 0 2
INSERT 0 3
 from_node_id | to_node_id  
--------------+-------------
 jira:SEED-1  | jira:SEED-1
 jira:SEED-2  | jira:SEED-2
 jira:SEED-1  | jira:SEED-2
(3 rows)
EXIT 0
```

### delete_apply

```text
[90m6:17PM[0m [32mINF[0m [1mApplied one migration[0m
EXIT 0
```

### after_delete

```text
 from_node_id | to_node_id  
--------------+-------------
 jira:SEED-1  | jira:SEED-2
(1 row)
EXIT 0
```

### all_apply

```text
[90m6:17PM[0m [32mINF[0m [1mRunning migrations[0m [36mdir=[0m./migrations
[90m6:17PM[0m [32mINF[0m [1mMigrations applied[0m
EXIT 0
```

### constraint_rejection

```text
ERROR:  new row for relation "edges" violates check constraint "edges_no_self_loop"
DETAIL:  Failing row contains (7, jira:SEED-1, jira:SEED-1, REFERENCES, null, 0, 2026-10-05 11:17:59.037377+00, c69e4067-ab58-4442-9753-f5cbd80958e9, 0, test, {}).
EXIT 1
```

### rollback

```text
[90m6:17PM[0m [32mINF[0m [1mMigration rolled back[0m
EXIT 0
```

### versions

```text
   version_id   | is_applied 
----------------+------------
 20261005111634 | t
(1 row)

 from_node_id | to_node_id  
--------------+-------------
 jira:SEED-1  | jira:SEED-2
(1 row)
EXIT 0
```

### reapply

```text
[90m6:17PM[0m [32mINF[0m [1mRunning migrations[0m [36mdir=[0m./migrations
[90m6:17PM[0m [32mINF[0m [1mMigrations applied[0m
EXIT 0
```

### guard_red_all_migrations

```text
=== RUN   TestReconcileEdges_NoSelfLoop
=== RUN   TestReconcileEdges_NoSelfLoop/self_and_other
    edge_self_loop_test.go:48: returned 2 edge IDs, want 1
    edge_self_loop_test.go:55: edges total=2 self=1 other=1, want 1/0/1
=== RUN   TestReconcileEdges_NoSelfLoop/only_self
    edge_self_loop_test.go:48: returned 1 edge IDs, want 0
    edge_self_loop_test.go:55: edges total=1 self=1 other=0, want 0/0/0
--- FAIL: TestReconcileEdges_NoSelfLoop (0.05s)
    --- FAIL: TestReconcileEdges_NoSelfLoop/self_and_other (0.01s)
    --- FAIL: TestReconcileEdges_NoSelfLoop/only_self (0.01s)
FAIL
FAIL	github.com/agent-mem/agent-mem/internal/graph/handlers	0.802s
FAIL
EXIT 1
```

### guard_final

```text
=== RUN   TestReconcileEdges_NoSelfLoop
=== RUN   TestReconcileEdges_NoSelfLoop/self_and_other
=== RUN   TestReconcileEdges_NoSelfLoop/only_self
--- PASS: TestReconcileEdges_NoSelfLoop (0.06s)
    --- PASS: TestReconcileEdges_NoSelfLoop/self_and_other (0.01s)
    --- PASS: TestReconcileEdges_NoSelfLoop/only_self (0.01s)
=== RUN   TestEdges_NoSelfLoopConstraint
--- PASS: TestEdges_NoSelfLoopConstraint (0.02s)
PASS
ok  	github.com/agent-mem/agent-mem/internal/graph/handlers	0.429s
EXIT 0
```

### semantic_skip

```text
=== RUN   TestSemanticFloorBoundaryAndOrder
--- PASS: TestSemanticFloorBoundaryAndOrder (0.00s)
=== RUN   TestSemanticFloorEmpty
--- PASS: TestSemanticFloorEmpty (0.00s)
=== RUN   TestSearch_SemanticFloorFiltersBothModes
=== RUN   TestSearch_SemanticFloorFiltersBothModes/default
    search_semantic_floor_db_test.go:72: DATABASE_URL not set
=== RUN   TestSearch_SemanticFloorFiltersBothModes/hybrid
    search_semantic_floor_db_test.go:72: DATABASE_URL not set
--- PASS: TestSearch_SemanticFloorFiltersBothModes (0.00s)
    --- SKIP: TestSearch_SemanticFloorFiltersBothModes/default (0.00s)
    --- SKIP: TestSearch_SemanticFloorFiltersBothModes/hybrid (0.00s)
=== RUN   TestSearch_SemanticFloorKeywordSurvivesBothModes
=== RUN   TestSearch_SemanticFloorKeywordSurvivesBothModes/default
    search_semantic_floor_db_test.go:102: DATABASE_URL not set
=== RUN   TestSearch_SemanticFloorKeywordSurvivesBothModes/hybrid
    search_semantic_floor_db_test.go:102: DATABASE_URL not set
--- PASS: TestSearch_SemanticFloorKeywordSurvivesBothModes (0.00s)
    --- SKIP: TestSearch_SemanticFloorKeywordSurvivesBothModes/default (0.00s)
    --- SKIP: TestSearch_SemanticFloorKeywordSurvivesBothModes/hybrid (0.00s)
=== RUN   TestSearch_SemanticFloorGraphSeeds
=== RUN   TestSearch_SemanticFloorGraphSeeds/below_floor
    search_semantic_floor_db_test.go:142: DATABASE_URL not set
=== RUN   TestSearch_SemanticFloorGraphSeeds/cosine_one_control
    search_semantic_floor_db_test.go:142: DATABASE_URL not set
--- PASS: TestSearch_SemanticFloorGraphSeeds (0.00s)
    --- SKIP: TestSearch_SemanticFloorGraphSeeds/below_floor (0.00s)
    --- SKIP: TestSearch_SemanticFloorGraphSeeds/cosine_one_control (0.00s)
=== RUN   TestSearch_SemanticFloorFoldedReplies
    search_semantic_floor_db_test.go:185: DATABASE_URL not set
--- SKIP: TestSearch_SemanticFloorFoldedReplies (0.00s)
PASS
ok  	github.com/agent-mem/agent-mem/internal/graph/handlers	0.331s
EXIT 0
```

### semantic_pass

```text
=== RUN   TestSemanticFloorBoundaryAndOrder
--- PASS: TestSemanticFloorBoundaryAndOrder (0.00s)
=== RUN   TestSemanticFloorEmpty
--- PASS: TestSemanticFloorEmpty (0.00s)
=== RUN   TestSearch_SemanticFloorFiltersBothModes
=== RUN   TestSearch_SemanticFloorFiltersBothModes/default
=== RUN   TestSearch_SemanticFloorFiltersBothModes/hybrid
--- PASS: TestSearch_SemanticFloorFiltersBothModes (0.09s)
    --- PASS: TestSearch_SemanticFloorFiltersBothModes/default (0.06s)
    --- PASS: TestSearch_SemanticFloorFiltersBothModes/hybrid (0.04s)
=== RUN   TestSearch_SemanticFloorKeywordSurvivesBothModes
=== RUN   TestSearch_SemanticFloorKeywordSurvivesBothModes/default
=== RUN   TestSearch_SemanticFloorKeywordSurvivesBothModes/hybrid
--- PASS: TestSearch_SemanticFloorKeywordSurvivesBothModes (0.06s)
    --- PASS: TestSearch_SemanticFloorKeywordSurvivesBothModes/default (0.03s)
    --- PASS: TestSearch_SemanticFloorKeywordSurvivesBothModes/hybrid (0.03s)
=== RUN   TestSearch_SemanticFloorGraphSeeds
=== RUN   TestSearch_SemanticFloorGraphSeeds/below_floor
=== RUN   TestSearch_SemanticFloorGraphSeeds/cosine_one_control
--- PASS: TestSearch_SemanticFloorGraphSeeds (0.06s)
    --- PASS: TestSearch_SemanticFloorGraphSeeds/below_floor (0.03s)
    --- PASS: TestSearch_SemanticFloorGraphSeeds/cosine_one_control (0.03s)
=== RUN   TestSearch_SemanticFloorFoldedReplies
--- PASS: TestSearch_SemanticFloorFoldedReplies (0.03s)
PASS
ok  	github.com/agent-mem/agent-mem/internal/graph/handlers	0.572s
EXIT 0
```

### semantic_fail

```text
=== RUN   TestSemanticFloorBoundaryAndOrder
--- PASS: TestSemanticFloorBoundaryAndOrder (0.00s)
=== RUN   TestSemanticFloorEmpty
--- PASS: TestSemanticFloorEmpty (0.00s)
=== RUN   TestSearch_SemanticFloorFiltersBothModes
=== RUN   TestSearch_SemanticFloorFiltersBothModes/default
    search_semantic_floor_db_test.go:72: SemanticFloor integration tests require scratch database agentmem_test
=== RUN   TestSearch_SemanticFloorFiltersBothModes/hybrid
    search_semantic_floor_db_test.go:72: SemanticFloor integration tests require scratch database agentmem_test
--- FAIL: TestSearch_SemanticFloorFiltersBothModes (0.00s)
    --- FAIL: TestSearch_SemanticFloorFiltersBothModes/default (0.00s)
    --- FAIL: TestSearch_SemanticFloorFiltersBothModes/hybrid (0.00s)
=== RUN   TestSearch_SemanticFloorKeywordSurvivesBothModes
=== RUN   TestSearch_SemanticFloorKeywordSurvivesBothModes/default
    search_semantic_floor_db_test.go:102: SemanticFloor integration tests require scratch database agentmem_test
=== RUN   TestSearch_SemanticFloorKeywordSurvivesBothModes/hybrid
    search_semantic_floor_db_test.go:102: SemanticFloor integration tests require scratch database agentmem_test
--- FAIL: TestSearch_SemanticFloorKeywordSurvivesBothModes (0.00s)
    --- FAIL: TestSearch_SemanticFloorKeywordSurvivesBothModes/default (0.00s)
    --- FAIL: TestSearch_SemanticFloorKeywordSurvivesBothModes/hybrid (0.00s)
=== RUN   TestSearch_SemanticFloorGraphSeeds
=== RUN   TestSearch_SemanticFloorGraphSeeds/below_floor
    search_semantic_floor_db_test.go:142: SemanticFloor integration tests require scratch database agentmem_test
=== RUN   TestSearch_SemanticFloorGraphSeeds/cosine_one_control
    search_semantic_floor_db_test.go:142: SemanticFloor integration tests require scratch database agentmem_test
--- FAIL: TestSearch_SemanticFloorGraphSeeds (0.00s)
    --- FAIL: TestSearch_SemanticFloorGraphSeeds/below_floor (0.00s)
    --- FAIL: TestSearch_SemanticFloorGraphSeeds/cosine_one_control (0.00s)
=== RUN   TestSearch_SemanticFloorFoldedReplies
    search_semantic_floor_db_test.go:185: SemanticFloor integration tests require scratch database agentmem_test
--- FAIL: TestSearch_SemanticFloorFoldedReplies (0.00s)
FAIL
FAIL	github.com/agent-mem/agent-mem/internal/graph/handlers	0.307s
FAIL
EXIT 1
```

### build

```text

EXIT 0
```

### handlers

```text
{"level":"info","node":"jira:cluster-control","llm_nil":true,"slack_msgs":0,"cluster_nodes":1,"time":"2026-10-05T18:18:17+07:00","message":"cluster summary: skipping LLM synthesis (no generator or no slack messages)"}
{"level":"info","node":"jira:cluster-control","llm_nil":true,"slack_msgs":0,"cluster_nodes":1,"time":"2026-10-05T18:18:17+07:00","message":"cluster summary: skipping LLM synthesis (no generator or no slack messages)"}
--- FAIL: TestImportBambooHR_CSVBytes_ParsesAndUpserts (0.02s)
    import_bamboohr_enqueue_test.go:50: expected 3 people rows, got 1
--- FAIL: TestIngestURL_AlreadyFresh (0.01s)
    ingest_url_test.go:142: outcome = "queued_for_fetch", want already_fresh
    ingest_url_test.go:145: expected 0 jobs for already_fresh, got 2
{"level":"warn","error":"unknown time zone Mars/Base","timezone":"Mars/Base","time":"2026-10-05T18:18:47+07:00","message":"invalid graph.temporal.timezone; using UTC"}
FAIL
FAIL	github.com/agent-mem/agent-mem/internal/graph/handlers	43.938s
FAIL
EXIT 1
```

### main_handlers

```text
--- FAIL: TestArtifactTSV_UpLockBounded (5.06s)
    artifact_tsv_migration_test.go:191: after failed up: tsv column=1 trigger=1 functions=3, want 0/0/0
{"level":"info","node":"jira:cluster-control","llm_nil":true,"slack_msgs":0,"cluster_nodes":1,"time":"2026-10-05T18:19:16+07:00","message":"cluster summary: skipping LLM synthesis (no generator or no slack messages)"}
{"level":"info","node":"jira:cluster-control","llm_nil":true,"slack_msgs":0,"cluster_nodes":1,"time":"2026-10-05T18:19:16+07:00","message":"cluster summary: skipping LLM synthesis (no generator or no slack messages)"}
--- FAIL: TestArtifactTSV_PopulatedUpgrade (0.03s)
    backfill_artifact_tsv_test.go:282: db version after DownTo = 20260929065720 (<nil>), want 20261003154813
--- FAIL: TestImportBambooHR_CSVBytes_ParsesAndUpserts (0.01s)
    import_bamboohr_enqueue_test.go:50: expected 3 people rows, got 1
--- FAIL: TestIngestURL_AlreadyFresh (0.01s)
    ingest_url_test.go:142: outcome = "queued_for_fetch", want already_fresh
    ingest_url_test.go:145: expected 0 jobs for already_fresh, got 2
{"level":"warn","error":"unknown time zone Mars/Base","timezone":"Mars/Base","time":"2026-10-05T18:19:44+07:00","message":"invalid graph.temporal.timezone; using UTC"}
--- FAIL: TestSearch_SemanticFloorFiltersBothModes (0.00s)
    --- FAIL: TestSearch_SemanticFloorFiltersBothModes/default (0.00s)
        search_semantic_floor_db_test.go:78: SemanticFloor integration tests require scratch database agentmem_test at 127.0.0.1:5450
    --- FAIL: TestSearch_SemanticFloorFiltersBothModes/hybrid (0.00s)
        search_semantic_floor_db_test.go:78: SemanticFloor integration tests require scratch database agentmem_test at 127.0.0.1:5450
--- FAIL: TestSearch_SemanticFloorKeywordSurvivesBothModes (0.00s)
    --- FAIL: TestSearch_SemanticFloorKeywordSurvivesBothModes/default (0.00s)
        search_semantic_floor_db_test.go:108: SemanticFloor integration tests require scratch database agentmem_test at 127.0.0.1:5450
    --- FAIL: TestSearch_SemanticFloorKeywordSurvivesBothModes/hybrid (0.00s)
        search_semantic_floor_db_test.go:108: SemanticFloor integration tests require scratch database agentmem_test at 127.0.0.1:5450
--- FAIL: TestSearch_SemanticFloorGraphSeeds (0.00s)
    --- FAIL: TestSearch_SemanticFloorGraphSeeds/below_floor (0.00s)
        search_semantic_floor_db_test.go:148: SemanticFloor integration tests require scratch database agentmem_test at 127.0.0.1:5450
    --- FAIL: TestSearch_SemanticFloorGraphSeeds/cosine_one_control (0.00s)
        search_semantic_floor_db_test.go:148: SemanticFloor integration tests require scratch database agentmem_test at 127.0.0.1:5450
--- FAIL: TestSearch_SemanticFloorFoldedReplies (0.00s)
    search_semantic_floor_db_test.go:191: SemanticFloor integration tests require scratch database agentmem_test at 127.0.0.1:5450
FAIL
FAIL	github.com/agent-mem/agent-mem/internal/graph/handlers	41.361s
FAIL
EXIT 1
```

### vet

```text
package github.com/agent-mem/agent-mem/internal/llmgateway
	imports github.com/agent-mem/agent-mem/internal/graph/handlers from client_test.go
	imports github.com/agent-mem/agent-mem/internal/llmgateway from subject_queries.go: import cycle not allowed in test
EXIT 1
```

### cleanup

```text
esl-scratch-5452
EXIT 0
```

The final guard red run was made with all migrations applied and the code guard temporarily removed; cleanup restored the CHECK. The guard was restored before the final passing run. The direct constraint test supplies the SQLSTATE proof.

The optional attempt to request verbose SQLSTATE output with a psql meta-command passed through `-c` was rejected by psql before executing SQL; it is not verification evidence. SQLSTATE is asserted by the passing direct constraint test.
