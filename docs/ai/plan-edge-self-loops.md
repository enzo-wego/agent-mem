# Plan: stop and remove self-loop REFERENCES edges

REVIEWER: pi (auto: GPT weekly 52% left)

## Problem

Prod (hub, payments Mac mini) has 235 edges where `from_node_id = to_node_id`,
all `kind = REFERENCES`: 107 jira, 93 gh_pr, 33 slack, 2 cf. They were created
between 2026-07-09 and 2026-10-05 (counted 2026-10-05 with a read-only SELECT).
Example: `jira:ADS-512 REFERENCES jira:ADS-512`. New ones still appear daily.

Cause: `reconcileEdges` (`internal/graph/handlers/fetch_body.go:401`) upserts
an edge for every key the extractor finds in a body. A Jira issue or PR body
usually contains its own key, so the node references itself. There is no
`from == to` check. Callers: `fetch_body.go:222`, `ingest_content.go:292`,
`describe_attachment.go:294`, `backfill_slack.go:329`.
`pruneStaleEdges` only removes edges missing from the current findings, and a
self-loop is always in them, so a refetch never clears it.

Found by the chrome-pane session (agent-mem-4a) and confirmed by the conductor.

## Goal

1. No code path creates an edge whose two ends are the same node.
2. The existing self-loops are gone from the hub.
3. The DB itself refuses a self-loop from now on, so a future insert path
   cannot bring the bug back.

## Non-goals

- No change to the extractor. It is right to find the key in the body. The
  edge layer decides what makes a valid edge.
- No change to other edge kinds or to `pruneStaleEdges`.
- No change to the ingest outcome or job behaviour beyond skipping the
  self-edge.
- The worker does not run anything against the hub DB and does not deploy.

## Files expected to change

- `internal/graph/handlers/fetch_body.go`: in `reconcileEdges`, `continue`
  before the node and edge upserts when `f.NodeID == fromNodeID`. Skip the
  target-node stub upsert too, since the node is the caller and already exists.
- One test in `internal/graph/handlers/` next to `edge_reply_root_test.go`,
  in the same style, calling `reconcileEdges` directly. A body containing its own
  key plus one other key gives exactly one edge (to the other key) and no row
  with `from_node_id = to_node_id`. A body containing only its own key gives zero
  edges and an empty return.
  **The guard test must not lean on the constraint.** Once the CHECK constraint
  exists, an unguarded `reconcileEdges` just logs the rejected INSERT and moves on
  (`fetch_body.go:429-433`), so the test would pass without the guard. In the
  guard test's setup, run `ALTER TABLE graph.edges DROP CONSTRAINT IF EXISTS
  edges_no_self_loop`, and restore it in `t.Cleanup` with the same `ADD CONSTRAINT`
  statement the migration uses. That way the test fails when the guard is removed.
  Put the constraint check in its own test: a direct `INSERT INTO graph.edges`
  with `from_node_id = to_node_id` fails with SQLSTATE 23514
  (check_violation).
- Two new migrations, each created with the repo's generator (find it in the
  `Makefile`, `migration_create` or equivalent; never hand-write a timestamp),
  one operation per file:
  1. `delete_edge_self_loops`: `DELETE FROM graph.edges WHERE from_node_id =
     to_node_id;` Down: no-op with a comment (the rows were invalid and are not
     restored).
  2. `edges_no_self_loop`: `ALTER TABLE graph.edges ADD CONSTRAINT
     edges_no_self_loop CHECK (from_node_id <> to_node_id);` Down: `DROP
     CONSTRAINT IF EXISTS`. Match how existing migrations in the repo are written
     (goose annotations, schema qualification).
- New `docs/ai/report-edge-self-loops.md`.

## Approach

1. **Check sync first.** `graph.edges` has `sync_id`, `sync_version`, and
   `machine_id`. Read the sync code (grep `sync_version` / `sync_id` under
   `internal/`) and answer in the report: can a client machine push an edge row
   back to the hub after the hub deletes it? Does a delete need a tombstone? Do
   the CHECK constraint and a rejected client push fail loudly or quietly? If a
   deleted self-loop would come back through sync, or the constraint would break
   a client's sync batch, **STOP and report** before writing the migrations. The
   code guard (step 2) is safe either way and can still go in.
2. **Code guard + test.** As above. Write the test first, watch it fail, then
   add the guard.
3. **Migrations.** As above. Prove they can be applied separately on the scratch
   DB: apply all, roll back only the last one, show the delete migration is still
   recorded in `goose_db_version` (or the repo's equivalent), then re-apply. Before
   the delete, insert two self-loop rows and one normal edge. After the delete,
   only the normal edge is left. After the constraint is added, a self-loop
   INSERT fails with a check violation.
4. **Scratch DB only.** A throwaway `pgvector/pgvector:pg16` container on a free
   port (check `docker ps` first; 5448-5451 have been used today), database
   `agentmem_test`, `go run ./cmd/agent-mem migrate`, `DATABASE_URL` pointed at
   it. Never use the dev DB on 5433. Remove the container afterwards.
5. **Commit and push** branch `fix/edge-self-loops` off `origin/main`. No PR,
   no merge, no deploy.

## Extra item: fix the SemanticFloor test guard (bd agent-mem-vm42)

`semanticFloorDB` in `internal/graph/handlers/search_semantic_floor_db_test.go`
(on main since PR #64) calls `t.Fatal` when `DATABASE_URL` is empty, and when the
DSN isn't exactly `127.0.0.1:5450/agentmem_test`. So a plain `go test ./...`
with no DB fails, and so does any scratch DB on another port. Replace both checks
with what the package's `testDB` helper (`helpers_test.go:13`) does:
`t.Skip` when `DATABASE_URL` is empty. Keep one safety check: `t.Fatal` unless
the database name is `agentmem_test`, so the truncating tests can never reach
the dev DB `agentmem`. Drop the host and port pin. Verify that
`go test -run SemanticFloor ./internal/graph/handlers/` SKIPs with no
`DATABASE_URL`, PASSes against your scratch DB on any port, and FAILs fast against
a DSN whose database is not `agentmem_test` (point it at your scratch server with
`/postgres` as the db name; never at port 5433). Add this file to the
files-to-change list. Close nothing in bd; the conductor does that.

## Acceptance criteria

1. The report answers the step 1 sync questions, citing file:line.
2. The guard test fails without the guard and passes with it, with all
   migrations applied (the test drops the constraint itself). Both outputs are
   pasted, the run is against the scratch DB, and it's PASS, not SKIP. The
   separate constraint test passes.
3. The separability demo in step 3 is pasted: apply, roll back the last
   migration, show the earlier one still recorded, re-apply. Also the
   delete/constraint behaviour on the seeded rows.
4. `go build ./...` passes. `go test ./internal/graph/handlers/` passes except
   the failures that also fail on `origin/main` (tracked as `agent-mem-j1nn`:
   TRYThread e2e, BambooHR CSV, IngestURL AlreadyFresh). Name every failure and
   say whether it also fails on main. `go vet` has a pre-existing import cycle
   (`agent-mem-hurt`), so report its output but don't fix it.
5. Diff touches only the files listed, plus `search_semantic_floor_db_test.go` for the extra item. No TODOs, and no new skips apart from the empty-`DATABASE_URL` skip.
6. The three guard outcomes for the extra item (SKIP / PASS / FAIL) are pasted.

## After the worker (conductor, human-gated)

Deploy runs the migrations on the hub. Before deploy, back up the rows that will
be deleted: `COPY (SELECT * FROM graph.edges WHERE from_node_id = to_node_id)
TO STDOUT` into a file in the scratchpad. After deploy, `SELECT count(*) FROM
graph.edges WHERE from_node_id = to_node_id` must be 0, and must stay 0 after
the next ingest cycle. Coordinate with the session already deploying in this
workspace (agent-mem-e3) so the two deploys don't overlap.

## Review history

pi review 1 (openai-codex gpt-6-astra), CHANGES REQUIRED, 1 finding: the CHECK
constraint hides a missing guard → the guard test drops the constraint in setup
and restores it in cleanup. The constraint gets its own test.

## Revision 2: step 1 STOP resolved (conductor, auto: recommended, 2026-10-05)

Round 1 (commit 9dfb6fb) stopped correctly on the literal step 1 condition. The
conductor resolves it as follows. **Proceed with steps 2-5 and the vm42 item
unchanged.**

- The hub's graph-edge push import (`sync_handlers.go:63-76`,
  `graph_sync.go:492-506`) can't restore a deleted self-loop once migration 2 is
  in place. The CHECK rejects the row, and the handler counts it as rejected and
  continues (`sync_handlers.go:70-76`). The constraint is the protection, and
  migration order (delete, then constraint) is what makes it hold.
- Replicas' cached copies: `internal/worker/server.go:92` runs
  `database.RunMigrations` at worker startup, so every machine applies the same
  delete + constraint when it upgrades. No tombstone protocol is needed.
- Current clients don't push graph edges (`engine.go:155-173`).
- Out of scope, and the report should list it as a follow-up: the hub's push
  response reports `rejected` counts that the client ignores
  (`engine.go:181-205`).

Add to the report's sync section: "Resolved by the conductor: the CHECK
constraint plus migrations at startup cover restoration and replica cleanup."
Update `report-edge-self-loops.md` with the step 2-5 and vm42 results in the
same commit series.
