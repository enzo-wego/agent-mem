# Code review: feat/graph-context-phase1-acl (deep-review, round 3)

Date: 2026-10-11. Reviewed `1a9ed74` against round 2 (`5a0105d`) and merge-base `483d03d`.
Ledgers: `2026-10-11-graph-context-phase1-acl-code.md` (F1-F6), `-code-r2.md` (F1 open).

## Verdict

**APPROVE. No further review needed.** F1 is closed. The delta adds no new findings.

## Receipts

- graph: not-applicable. This is a one-clause Round-N delta, traced with rg over every caller of
  `parentVisibleSQL`, `aclVisibleSQL`, `nodeVisible` and `visibleAttachments`.
- fan_out: inline: Round-N delta.
- execution: `go vet ./...` is clean. `make test-db` exits 0 with 2029 pass, 0 fail and 4 skips.
  `TestACLRoutes_NullScopeSlackFileNotExposedByOrgParent`,
  `TestACLRoutes_SlackFileNeedsOwnScopeEvenWithOrgParent` (18 subtests) and
  `TestACLScopePredicatesAgree` all pass.
- Hub DB: read-only aggregate SELECTs only (types, scope shapes and counts).
- Coverage check: `validate-ledger.sh --coverage-only` returned `OK: coverage valid`.

## Ledger

| ID | Round 2 | Round 3 |
|----|---------|---------|
| F1 | HIGH, open | **Resolved** (evidence below) |
| F2-F6 | Resolved | Resolved, unchanged |

## F1: evidence on production data shapes

`acl_guard.go:287` now reads `... OR (n.type <> 'slack_file' AND orgScope(p.scope))`. For a
`slack_file`, a REFERENCES parent therefore counts only when its scope is NULL, empty,
`public`, or in the asker's member set.

These are the REFERENCES parents of `slack_file` nodes on the hub, grouped by
type / scope shape / live:

| Parent type | Scope | Live | Edges | Files |
|---|---|---|---|---|
| slack | `slack:C…` | yes | 5027 | 4371 |
| slack_file | NULL | yes | 46 | 46 (excluded as a parent) |
| slack | `slack:C…` | deleted | 15 | 11 (excluded) |
| jira | bare `jira` | yes | 10 | 10 |
| cf | bare `confluence` | yes | 1 | 1 |

- The 11 jira and Confluence edges carry org scopes. Under the new clause they no longer count,
  and this was the round-2 leak.
- Every live Slack parent has a `slack:C…` channel scope, which is member-gated. The data holds
  no DM (`D…`) scopes, no bare `slack` scope and no `slack_thread` parents.
- Parents with a NULL, empty or `public` scope: **0** files are reachable through one.
  Every NULL-scope non-attachment node on the hub (slack 640, jira 254, gh_pr 83, and others)
  is a bodiless stub with 0 outgoing REFERENCES. The `public` nodes are wegohub (57 with
  REFERENCES, all to `payment_ref`) and claude_artifact (0 with REFERENCES). None of them is
  org-wide in a way that reaches a file.
- Residual, not a finding: a future writer that adds REFERENCES from a NULL-scope or `public`
  node to a `slack_file` would expose it. No current writer does that. `reconcileEdges` runs
  on fetched parents, which `deriveScope` has already scoped.

**Tests use production-shaped data.** `seedKeywordNode` stores `""` as `NULLIF('')`, i.e. NULL,
so the F1 files have NULL scope, matching production. The parent's `jira:PAY` and production's
bare `jira` both take the same `split_part(...,':',1)` org branch. The test covers non-member
and member-of-nothing askers on `/node`, `/search`, `/neighbors` (with and without cards),
`/epic` and `/resolve`. It also covers member access through a Slack parent and admin access.
Under the old clause, the `jira:PAY` parent would make `F1ONLY` visible, so the test fails.

## Regression checks (no new findings)

- `jira_attachment`: `type <> 'slack_file'` holds, so the org rule still applies. The round-1
  and round-2 tests pass.
- Non-attachments do not use `parentVisibleSQL`, so they are unchanged. The `nodeVisible(..., "gh_pr", …)`
  calls at `neighbors.go:562` and `search_hybrid_display.go:198` take the `scopeVisible` path.
- Go/SQL parity: `nodeVisible` runs the same `parentVisibleSQL`, and `visibleAttachments`
  and `aclVisibleSQL` embed it, so the Go and SQL checks cannot diverge.
- `acl_fix2_test.go`: adding a member Slack parent keeps the "member sees it" assertions
  meaningful now that the org parent no longer counts.
- The plan doc's "Fix round 2" section matches the code.
