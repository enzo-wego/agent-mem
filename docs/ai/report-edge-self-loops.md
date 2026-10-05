# Edge self-loop implementation report

## Status: blocked at step 1

Stopped before writing migrations, as required by the plan's sync safety gate. No production code, tests, or migrations changed. The plan was copied verbatim into this worktree.

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

## Verification and boundaries

Inspected the sync collection, import, push handler, and client acknowledgement paths in this worktree. Confirmed branch `fix/edge-self-loops`. `docker ps --format '{{.Names}} {{.Ports}}'` showed no containers exposing 5448–5452; it showed the dev Postgres container exposing 5433. No database connections or scratch container were made because the safety gate stopped implementation before DB work.

No API keys, tokens, or settings were read. No dev or hub database access, PR, merge, or deployment occurred. No tests/build/vet were run: this is a documentation-only STOP report, not an implementation or a passing acceptance report.

Steps 2–5 and the SemanticFloor extra item (`agent-mem-vm42`) remain unimplemented at this checkpoint. No Beads issues were closed.

## Decision needed

Authorize proceeding on the explicit assumption that all participating clients use the current graph-pull-only engine, accepting that the hub still supports incoming graph edges and replicas retain deleted rows; or revise the plan to define the hub's graph-push/deletion policy first. No migrations will be written until that scope decision is made.
