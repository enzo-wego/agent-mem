# Code review: feat/graph-context-phase1-acl (deep-review, round 2)

Date: 2026-10-11. Reviewed `5a0105d` against round 1 (`78eb48c`) and merge-base `483d03d`.
Ledger: `2026-10-11-graph-context-phase1-acl-code.md` (F1-F6).

## Verdict

**REQUEST_CHANGES.** F1 is still open. The fix gates a `slack_file` on its own scope, but
no code path ever writes a `slack_file`'s scope. In production those files have a NULL scope,
so the new gate always passes. A Jira, GitHub or Confluence parent still exposes them to
every employee. F2-F6 are resolved.

## Receipts

- graph: not-applicable: this is a Round-N delta review, so the delta symbols were traced
  with rg.
- fan_out: inline: Round-N delta.
- execution:
  - `go vet ./...` is clean.
  - `make test-db` exits 0: 16/16 packages, 2028 pass, 4 skips. All six new round-1 tests
    pass.
  - `cd dashboard && npm test` (Node 24) passes 11/11.
- Coverage check: `validate-ledger.sh --coverage-only` returned `OK: coverage valid`.

## Ledger

| ID | Round 1 | Round 2 |
|----|---------|---------|
| F1 | HIGH | **Open.** Mechanism changed, but the leak is still reachable (see F1 below) |
| F2 | MEDIUM | Resolved. The README and plan examples carry `-e AGENT_MEM_ASKER=<slack user id>`, and the Risk line is rewritten |
| F3 | LOW | Resolved. The loop at `acl_guard.go:190` stops at the first row that has an eeid. The 2-hop test passes. Merged-away rows can only narrow scopes, not widen them |
| F4 | LOW | Resolved. `SKILL.md:94` and `CLAUDE.md:91` send `X-Graph-Admin: 1`, and `CLAUDE.md:137` says "exactly" |
| F5 | LOW | Resolved. `%q` at `acl_guard.go:72` |
| F6 | LOW | Resolved. Root-query 500, linked-only thread, org scopes on neighbors/epic/resolve, MCP env test, dashboard `npm test` and the shallow-clone fail are all covered |

## F1 (still open). HIGH: the own-scope gate is a no-op on real `slack_file` rows

**Where.** `internal/graph/handlers/acl_guard.go:283`. The clause
`(n.type <> 'slack_file' OR n.scope IS NULL OR n.scope = '' OR ...)` is true for any file
whose scope is NULL.

**Evidence.** Every path that creates a `slack_file` node inserts it without a scope:

- `internal/graph/handlers/ingest_content.go:325-330` inserts `(id, type, natural_key, url, ...)`.
- `internal/graph/handlers/backfill_slack.go:421-426` does the same.
- `internal/graph/handlers/fetch_body.go:409` (`reconcileEdges`, the Jira, GitHub and
  Confluence body-link stub) inserts `(id, type, natural_key, ...)`.

`describe_attachment.go:238` updates only `body`. `deriveScope` runs only for the fetched
parent. No migration sets an attachment's scope.

**Failure scenario.** This is the round-1 trigger, unchanged:

1. A Jira body links `wego.slack.com/files/U…/F…` for a DM or private-channel file.
2. `slack_file:F…` has a NULL scope, so the gate passes.
3. The org clause on the parent passes too.
4. `/node`, `/search`, `/neighbors`, `/epic` and `/resolve` return the file to any employee
   who has an eeid.

**Why the tests pass.**
- `acl_fix2_test.go:24` seeds the file with scope `slack:GPRIV`, a shape production never
  produces.
- `refresh_slack_members_acl_test.go:78` was changed to the same shape.
- No test covers a NULL- or empty-scope `slack_file` under an org parent. `TestACLRoutes_OrgScopes`
  puts its empty-scope file under a Slack parent only.

**Confidence.** Likely. The mechanism is plain SQL, and the NULL scope follows from reading
the code. The deciding production fact is checkable with
`SELECT count(*) FROM graph.nodes WHERE type='slack_file' AND (scope IS NULL OR scope='')`.
This is block-until-disproven, because a disclosed DM file cannot be undone.

**Fix.** Keep the org rule out of the parent predicate when the child is a `slack_file`. In
`parentVisibleSQL`, replace `OR <orgScopeSQL(p.scope)>` with
`OR (%[1]s.type <> 'slack_file' AND <orgScopeSQL(p.scope)>)`.

- A file is then visible only through a NULL, public or member-scope parent, which in
  practice means its Slack message.
- `jira_attachment` keeps the org rule. Non-attachments are untouched.
- `nodeVisible` runs the same SQL, so Go/SQL parity holds by construction.
- Change the F1 test to seed the file with a NULL scope.

## Parity and regression checks (no new findings)

- `nodeVisible` (`:299-311`) delegates to `parentVisibleSQL` over the stored row, so Go and
  SQL cannot diverge on attachments.
- `jira_attachment`: `type <> 'slack_file'` is true, so its behavior is unchanged.
- Non-attachments do not use `parentVisibleSQL`, so they are unchanged.
- Placeholder rewrite: `%[1]s`, `%[2]d` and `%[3]s` map to alias, scope parameter and org SQL
  in the same way as the positional form they replace.
- `visibleAttachments` and `aclVisibleSQL` take the change automatically.
- The own-scope gate only narrows. A `slack_file` with its own scope set loses the
  shared-in-another-channel path. That fails closed, and no current writer sets that scope.
