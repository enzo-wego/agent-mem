# Make a pasted Slack permalink resolve, and let files be artifacts

Issues: `agent-mem-ckk` (permalink), `agent-mem-qxz` (files in resolve)

Follows `docs/ai/files-in-neighbors.md` (`agent-mem-q6a`, shipped as `c224f74`),
which fixed `/neighbors` only. A client that starts from a URL still gets
nothing.

## Problem (measured against prod, 2026-08-05)

Target: `https://wego.slack.com/archives/C0AV14LGPMG/p1782118242921599?thread_ts=1781081424.346499&cid=C0AV14LGPMG`

| Call | Result |
|---|---|
| `graph_node(url=<full URL above>)` | **empty** |
| `graph_node(url=<same, query stripped>)` | resolves to `slack:C0AV14LGPMG:1782118242.921599` |
| `graph_resolve(query=…, seeds=[<full URL>])` | **0 artifacts** |
| `graph_resolve(query=…, seeds=[<query stripped>])` | 45 artifacts, **0 files** |
| `graph_neighbors(id=<node id>)` | 38 rows, 4 files ✅ (already fixed) |

Two independent causes, both confirmed in code and on prod:

**A. Exact URL match.** `node.go:75` (`WHERE ($1 = '' OR n.url = $1)`) and
`resolve.go` `canonicalizeSeeds` (`WHERE url = $1`) compare against the stored
bare permalink. Every real pasted Slack URL carries `?thread_ts=…&cid=…`, so it
matches nothing. In `canonicalizeSeeds` the failure is silent and worse than a
404: the seed stays the raw URL string, fails the `slack:` prefix check, skips
the reply→thread-root promotion, and enters the BFS as a garbage node id.

**B. Bodyless nodes can never be artifacts.** `hydrate/budget.go:65-68`:

```go
if body == nil {
    missed = append(missed, c.NodeID)
    continue
}
```

A file has no body by nature — it is a title plus a URL. Proven on prod:
`graph_resolve` seeded at the thread root, depth 1, budget 16000 returns
`artifacts: 35 | types: ['cf','slack']` and
`cache_misses: 3, of which slack_file: 2 — ['slack_file:F0B90RTPEPK','slack_file:F0B6RMXUKSA']`.
The two Saudi Rail sheets reach the BFS and are then discarded at hydration.
Not a budget problem: the budget was nowhere near exhausted.

## Goal

Pasting a Slack message URL — exactly as Slack's "Copy link" produces it — into
`graph_node` or `graph_resolve` finds the message, and `graph_resolve` returns
the thread's files among its artifacts.

## Non-goals

- Changing `/neighbors` again — it already works.
- Making `hydrate.Greedy` fetch bodies, or changing what gets enqueued for
  `fetch_body`. `missed` keeps its current meaning ("body wasn't in cache") and
  its current contents.
- Re-typing the Google-Sheet links stored as `slack_file`.
- `agent-mem-pox` (Greedy's oversized-body behaviour) — untouched.

## Files expected to change

- `internal/graph/handlers/detect_hot_topics.go` — new `slackNodeIDFromURL`
  helper, next to the existing `slackPermalink` it inverts.
- `internal/graph/handlers/node.go` — use it, plus strip query/fragment.
- `internal/graph/handlers/resolve.go` — same in `canonicalizeSeeds`.
- `internal/graph/hydrate/budget.go` — bodyless file/attachment becomes a
  zero-token artifact.
- Tests: `node_test.go`, `resolve_test.go`, `internal/graph/hydrate/*_test.go`.

## Approach

### 1. Parse Slack permalinks instead of string-matching them

Add the inverse of `slackPermalink`:

```go
// slackNodeIDFromURL turns a Slack permalink into a canonical node id.
// "" when the URL isn't a Slack archive link.
func slackNodeIDFromURL(rawURL string) string
```

Parse with `net/url` and read the **path** only, so `?thread_ts=…&cid=…` and
any `#fragment` are irrelevant by construction. Path shape:
`/archives/<CHANNEL>/p<digits>`. The ts is the digits with a `.` inserted 6
from the end (`1782118242921599` → `1782118242.921599`) — the exact inverse of
`slackPermalink`'s `strings.ReplaceAll(parts[2], ".", "")`. Return `""` on any
shape mismatch, a non-numeric `p` segment, or fewer than 7 digits.

Use it in both lookups **before** the URL comparison, and look the node up by
`id` when it returns non-empty — that drops the dependence on the stored `url`
column entirely for Slack.

Then, for the remaining (non-Slack) URL fallback, strip query + fragment before
comparing, so a Jira/Confluence/GitHub link with tracking params also resolves.
Keep the raw-URL comparison as well, so any stored URL that legitimately
contains a query string still matches.

In `canonicalizeSeeds` the existing reply→thread-root promotion then applies
unchanged — which is what puts the root's files at hop 1.

### 2. A bodyless file is still an artifact

In `hydrate.Greedy`, when `body == nil` **and** the type is `slack_file` or
`jira_attachment`, emit a `Hydrated` with `Body: ""` and `Tokens: 0` (a title +
URL costs nothing worth budgeting) and continue appending it to `missed`
exactly as today — so the `fetch_body` enqueue path is byte-for-byte unchanged.

Every other bodyless type keeps today's behaviour: `missed`, no artifact. Scope
the change to the two types that have no body by nature, so nothing that *is*
waiting on a body starts leaking empty artifacts.

`ponytail:` comment naming why these two types are special-cased.

## Acceptance criteria

1. `graph_node(url=<full URL with ?thread_ts&cid>)` returns node
   `slack:C0AV14LGPMG:1782118242.921599`.
2. `graph_resolve(query="Saudi Rail tax", seeds=[<full URL>])` returns > 0
   artifacts and includes both `slack_file:F0B90RTPEPK` (Saudi Rail - Tax
   Analysis) and `slack_file:F0B6RMXUKSA` (Saudi_Rail(HHR)_GoLive_Checklist).
3. A Slack URL with a trailing slash, and one with a `#fragment`, both resolve.
4. A non-Slack URL with `?utm_source=x` resolves to the node stored without it.
5. A bodyless node of type `slack` or `cf` still produces **no** artifact
   (regression guard on the narrow scoping).
6. `missed` / `cache_misses` still contains the file ids after the change.
7. `graph_neighbors` output for
   `slack:C08SVNFA30R:1784184069.195639` is unchanged (still 3 file rows).
8. `go build ./...`; `go test ./internal/graph/... ./internal/graphmcp/...`
   shows no new failures. Two failures are known-pre-existing and tracked as
   `agent-mem-cqt`: `TestImportBambooHR_CSVBytes_ParsesAndUpserts` and
   `TestIngestURL_AlreadyFresh`. Do not "fix" them here; do not count them
   as yours.

## How to verify

- Unit: table-driven cases for `slackNodeIDFromURL` (full URL with query, bare,
  trailing slash, fragment, non-Slack, malformed `p` segment, too-few digits).
  Handler tests for node + resolve seeded via the existing helpers.
- Hydrate: a bodyless `slack_file` yields one artifact with `Tokens == 0` and
  still appears in `missed`; a bodyless `slack` yields none.
- Integration tests run **only** against the `agentmem_test` scratch DB
  (`DATABASE_URL=postgres://agentmem:agentmem@localhost:5433/agentmem_test?sslmode=disable`).
  Never the dev DB — it truncates the graph and syncs to prod.
- No dashboard change here, so no bundle rebuild is needed.
- Prod verification after `make deploy` is the reviewer's job, not the
  worker's. Do not commit, push, or deploy.
