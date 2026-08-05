# Files in neighbors: surface attachment leaves + make file nodes clickable

Issue: `agent-mem-q6a`

## Problem (measured on prod, 2026-08-05)

Opening the `/live` topic popup for `slack:C08SVNFA30R:1784184069.195639`
("Saudi Rail train tax: V1 vs V3 API decision"):

- **TIMELINE tab shows no files.** It calls
  `GET /api/graph/node/{id}/neighbors?depth=1`. Hop-1 of that root is 8 rows
  (1 feature, 2 `gh_pr`, 5 `slack`) — zero `slack_file`. The frontend already
  has a "Files" group (`GRAPH_TYPE_GROUPS`, order 4); it simply never receives
  a file row.
- **GRAPH tab does show them**, because it calls
  `/api/graph/cluster/summary?depth=2`. The two file nodes are hop **2**: they
  hang off `slack:C0AV14LGPMG:1781081424.346499` (the Saudi Rail taxation
  thread in #partner-saudi-rail) via `REFERENCES`:
  - `slack_file:F0B6RMXUKSA` — Saudi_Rail(HHR)_GoLive_Checklist
  - `slack_file:F0B90RTPEPK` — Saudi Rail - Tax Analysis
- **MCP has the same blind spot.** `graph_neighbors` (default depth 1) is a
  thin passthrough to the same HTTP endpoint, so an agent asking about this
  thread never sees the files.
- **File dots are effectively unclickable.** `ClusterGraph.tsx` excludes
  `slack_file` from `LABELLED` and draws non-feature nodes at r=3 with a
  pointer area of r+2, so they are unlabelled 3px dots in a 30-node hairball.

Verified non-issues (do not "fix" these):
- The message the report started from
  (`C0AV14LGPMG:1782118242.921599`) has **no file** — Slack reports
  `FileCount 0`. It is ingested correctly (node exists, body stored).
- All 27 messages of that thread are nodes, and the messages that *did* post
  files carry their own `REFERENCES` edge to the file node.

## Goal

A client asking agent-mem about a thread — via the `/live` timeline **or** via
MCP `graph_neighbors` at the default depth — gets the thread's files.

## Non-goals

- Empty-`url` file nodes (6 of 1743 repo-wide) — separate issue.
- Re-typing Google-Sheet links currently stored as `slack_file` into
  `gws_doc`. Out of scope.
- Adding edges from reply messages to their thread root.
- Any new MCP tool.

## Files expected to change

- `internal/graph/handlers/neighbors.go` — the fix.
- `internal/graph/handlers/neighbors_test.go` — coverage.
- `dashboard/src/pages/ClusterGraph.tsx` — label + hit target.
- `internal/graphmcp/server.go` — one-line tool-description update only.

## Approach

### 1. Attachment leaves do not consume a hop (backend)

In `neighborsHandler.serve`, after the existing BFS loop finishes, run one
extra pass: for every node already surfaced (plus the opened root), pull its
`REFERENCES` children of type `slack_file` / `jira_attachment` and emit them
as `neighborItem`s that were not already in `seen`.

Rules:
- Reuse the existing `seen` map so nothing is duplicated.
- `item.Hop` = parent hop + 1; set `item.Node.Via` to the parent's display
  title so the UI can say which thread the file came from.
- Respect the existing ACL: run the same `scopeVisible` check, and never
  attach a leaf whose parent was filtered out.
- Skip this pass when `kindFilter` is set (an explicit edge-kind query should
  stay literal).
- `expandableThrough` stays as-is — files remain non-traversable corridors.
  This pass pulls files *in* as leaves; it never walks *through* them.
- Cap the pass at 20 file rows per request (`ponytail:` comment naming the
  cap), so a thread with a photo dump can't flood the payload.

One extra query, no change to the BFS itself, and it fixes the timeline and
MCP together because both read this endpoint.

### 2. Make file nodes findable in the cluster graph (frontend)

In `dashboard/src/pages/ClusterGraph.tsx`:
- Add `slack_file` and `jira_attachment` to `LABELLED`.
- Give those types the same radius as `feature` (5, pointer area 7) so they
  are a real click target.

`onNodeClick` already opens `n.url` — both files in this cluster have valid
Google-Sheets URLs, so no handler change is needed.

### 3. Tell MCP clients files come back

Update the `graph_neighbors` description in `internal/graphmcp/server.go` to
state that attached files and Jira attachments are included as leaves
regardless of depth. Description text only — no schema change.

## Acceptance criteria

1. `GET /api/graph/node/slack%3AC08SVNFA30R%3A1784184069.195639/neighbors?depth=1`
   returns rows for **both** `slack_file:F0B6RMXUKSA` and
   `slack_file:F0B90RTPEPK`, each with a non-empty `url` and a `via` naming
   the Saudi Rail taxation thread.
2. The same request with `?kind=SAME_TOPIC` returns **no** file rows
   (filtered queries stay literal).
3. `/live` popup → TIMELINE tab for that thread shows a **Files** section
   listing those two sheets.
4. MCP `graph_neighbors {"id": "slack:C08SVNFA30R:1784184069.195639"}`
   (no depth) returns the two file rows.
5. GRAPH tab: file dots are labelled and clicking one opens the sheet.
6. A thread with more than 20 files returns exactly 20 file rows, not more.
7. `go build ./... && go test ./internal/graph/handlers/... ./internal/graphmcp/...`
   pass. Dashboard builds.

## How to verify

- Unit: extend `neighbors_test.go` — a root whose hop-1 slack neighbor owns a
  file leaf must yield that file at depth 1; the `kind`-filtered variant must
  not; a 25-file fixture must yield 20.
- Integration: **never** against the live dev DB — use the `agentmem_test`
  scratch DB (see `feedback_integration_tests_wipe_db`).
- Prod, after `make deploy` (build amd64 locally → push GHCR → VPS pulls;
  never build on the VPS): curl the endpoint above on `enzogo.io.vn` and quote
  the two file rows; screenshot the TIMELINE Files section and a labelled file
  dot in GRAPH.
- Remember to rebuild the embedded dashboard assets after touching
  `dashboard/src`.
