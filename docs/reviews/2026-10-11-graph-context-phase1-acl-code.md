# Code review: feat/graph-context-phase1-acl (deep-review, round 1)

Date: 2026-10-11. Reviewed `78eb48c` against merge-base `483d03d` (worktree
`/Users/enzo/go/src/github.com/agent-mem-wt-acl`). No PR exists yet. Reviewer: deep-review
engine. It ran four finder bundles (structural, temporal, tests-and-claims, outward) and a
central refutation gate.

## Verdict

**REQUEST_CHANGES.** One finding blocks: the org rule widens Slack-file visibility through
Jira, GitHub or Confluence parents.

The core boundary holds:
- No request without `X-Graph-Admin: 1` got unfiltered data on any path traced.
- The route policy and chi route on the same string, `RawPath` or `Path` (chi v5.2.5,
  `mux.go:446-452`).
- The `/api/graph` Mount wildcard sends every unknown path through the policy.
- HEAD only reaches 405/404.
- Every data path in search, node, neighbors, epic and resolve goes through
  `aclVisibleSQL` or `nodeVisible`.
- The Go and SQL org predicates agree.

## Receipts

- graph: ran (190 changed symbols, 9 call sites, 6 unresolved, 0 truncated, built 78eb48c03af45b215623cd8fd4fcff92d0e942ca); degraded: per-symbol callers queried only for the 5 ACL core symbols, the rest traced with rg by the finders
- fan_out: ran (4/4)
- execution:
  - `go vet ./...` clean.
  - `make test-db` exit 0: 16/16 packages, 2002 pass, 4 unrelated skips.
  - `go test -race` clean on `internal/graphmcp` and the ACL tests in `internal/graph/handlers`.
  - Throwaway proof tests were **not** run. The team-mode PreToolUse guard blocked writing
    `zz_throwaway_*_test.go`, and the review was not routed around it.
- security: a scoped pass over the merge-base diff. The worktree is isolated, so no harness
  security-review ran.
- Coverage check: `validate-ledger.sh --coverage-only` returned `OK: coverage valid`.

## Findings

### F1. HIGH: private Slack files become visible to every employee once a Jira, GitHub or Confluence body links them

**Blocking until disproven.**

**Where**
- `internal/graph/handlers/acl_guard.go:277-284` (`parentVisibleSQL`), reached from
  `aclVisibleSQL` (`:287-294`) and `nodeVisible` (`:296-308`).
- An attachment's visibility is "any visible non-attachment parent with a REFERENCES edge".
  The attachment's own scope is never checked.
- This branch adds `OR split_part(p.scope,':',1) IN ('jira','github','confluence')` to the
  parent predicate.

**Trigger**
1. A Jira issue, PR or Confluence page body contains a
   `wego.slack.com/files/<U>/<F…>` URL. The extractor turns it into a REFERENCES edge from
   that node to `slack_file:F…` (`internal/graph/extractor/extractor.go:241-254`;
   `reconcileEdges` in `fetch_body.go`).
2. The file was posted in a private channel or a DM, so its scope is `slack:G…` or `slack:D…`.
3. `GET /api/graph/node?id=slack_file:F…` with `X-Asker-User` set to any employee with an
   eeid now returns 200, with the title, URL and body (including the describe_attachment
   description).
4. The same applies to search, neighbors, epic and resolve.

Before this branch no filtered asker ever held a `jira:*`, `github:*` or `confluence:*`
scope, because `member_scopes` holds Slack scopes only. So this path was closed in practice.

**Why it matters.** D1 says "Slack stays per channel". The plan's "multi-parent keeps today's
behaviour" (Acceptance 3) did not consider that today no Jira parent was ever visible. Once
someone reads a disclosed DM file, that cannot be undone.

**Confidence.** Likely. The mechanism is a static read of plain SQL. A proof test was
blocked (see Receipts). Whether such edges exist in production is author-only state:

```sql
SELECT count(*)
FROM graph.nodes f
JOIN graph.edges e ON e.to_node_id = f.id AND e.kind = 'REFERENCES'
JOIN graph.nodes p ON p.id = e.from_node_id
WHERE f.type = 'slack_file'
  AND f.scope LIKE 'slack:%'
  AND split_part(p.scope, ':', 1) IN ('jira', 'github', 'confluence');
```

**Fix.** Gate `slack_file` on its own channel as well as on a parent:
- A `slack_file` must have its own scope NULL, empty, `public` or in the member scopes,
  **and** a visible parent.
- Equivalently, leave the org clause out of `parentVisibleSQL` when the child is a
  `slack_file`. `jira_attachment` can keep the org rule.
- Mirror the change in `nodeVisible`.
- Add the trigger above as a test: a non-member gets 404, a channel member gets 200.

### F2. MEDIUM: operator-facing MCP registration commands are wrong

**Where**
- `README.md:745-751`: the `claude`, `codex` and `gemini mcp add` examples still run
  `docker exec -i agent-mem-worker-1 agent-mem mcp` with no `-e AGENT_MEM_ASKER=<id>`.
  Copying them registers an MCP where every tool returns `ErrAskerRequired`.
- Plan "Verify after merge" step 4 says `-e AGENT_MEM_ASKER` with no value. Over ssh, a bare
  `-e AGENT_MEM_ASKER` reads the variable from the remote shell, where it is unset.

**Related.** The plan's Risk "a shell-profile AGENT_MEM_ASKER is inherited by bot panes" does
not hold: `docker exec` does not pass host env. The real shared-identity risk is the worker
`.env` `env_file` (`docker-compose.yml:37`). Putting the variable there makes every MCP
session the same person.

**Fix.** Put `-e AGENT_MEM_ASKER=<slack id>` in every example and in plan step 4, and correct
that Risk line.

### F3. LOW: a merge chain can map one employee's eeid to another employee's scopes

**Where.** `acl_guard.go:189` follows `merged_into` even when the matched row already has its
own eeid.

**Trigger**
1. Row A (eeid 10) is merged into B (eeid NULL), and B is merged into C (eeid 20).
2. `mergePersonInto` (`import_bamboohr.go:437`) allows this, because it only refuses a
   direct merge of two eeid-bearing rows.
3. `X-Asker-User: 10` then resolves to eeid 20.

**Precedent.** Migration `20260728000002_unmerge_distinct_employees.sql` repaired 57 such
rows. Slack-id and email lookups are safe, because the merge NULLs the loser's identity
columns.

**Confidence.** Hypothesis; reachability in production is unconfirmed.

**Fix.** Stop at the first row with a non-NULL eeid, or reject with `asker_conflict` when the
start and end eeids differ. Add a 2-hop test.

### F4. LOW: in-repo verification examples now return 403

- `plugin/skills/add-graph-source/SKILL.md:95` (a shipped skill) runs `curl /api/graph/search`
  with no asker or admin header. The skill's own verify step fails after deploy.
- `CLAUDE.md:91` has the same problem with `/node/<id>/neighbors`.
- `CLAUDE.md:137` says "`jira*`, `github*`, `confluence*`". The code matches the prefix
  exactly, and `jiraevil:X` stays hidden.

**Fix.** Add `-H "X-Graph-Admin: 1"` to both curl examples, and reword `:137` to say the
prefix matches exactly.

### F5. LOW: the admin log line writes a decoded path with `%s`

**Where.** `acl_guard.go:72`.

**Problem.** `r.URL.Path` is percent-decoded, so `%0A` in a node id forges a log line. The
dashboard sends the admin header on every call, so this line logs per dashboard request.

**Fix.** Use `%q` for the route.

### F6. LOW: test gaps that hide branches

- `asker_unresolved_test.go:230` `neighbors_root_query_error` now stops at the asker lookup
  with 503. The `neighbors.go` root-query 500 branch it is named for is no longer exercised.
- Acceptance 2 says "incl. linked rows". The hidden fixture's title matches the query, so
  `addLinkedThreads` is never driven by a hidden thread that is reachable only by link.
- The org-scope checks cover only `/node` and `/search`.
- No test checks that the `AGENT_MEM_ASKER` env value reaches `SetAsker`
  (`cmd/agent-mem/mcp.go:70`).
- `dashboard/scripts/authFetch.test.ts` is in no runner.
- `TestEvalCaseLabelsUnchangedFromBase` skips silently in a shallow clone.

**Fix.** Add the missing cases, rename the neighbors test, and wire the dashboard test into
`package.json` or the Makefile.

## Not findings (refuted or already accepted)

**Routing bypasses: none.** These all fail closed with `admin_required` or land on a filtered
handler that calls `requireAsker` first:
- `//search`, a trailing `/neighbors/`, and `epic/A/B`;
- `epic/a%2Fb`, `node/a%2F..%2F/neighbors`, and `%73earch`.

`/api/gr%61ph/...` skips the policy, but chi cannot route it to a graph handler.

**Resolver edge cases: no issue.**
- LIMIT 8 truncation is impossible, because the identity columns are UNIQUE.
- DB, cancel and scope-builder errors all give 503 with `rejected`.
- Rejected trace rows hold no items.
- The MCP asker is per process, with no shared state.

**Pre-existing, not regressions:**
- `expanded_nodes` reports the count before the ACL filter.
- A `canonical_seeds` trace entry can name a hidden id.
- `sync_url` mode skips auth.
- Neighbors `cards=1` on NULL-scope stubs can show participants of a private thread.
- Briefs with NULL `sources` are shown without a source check.
- The 5-minute scope cache delays revocation.

**Accepted in the plan:** p-agent Home tab and `/slack-user` return 403, the epic brief needs
`AGENT_MEM_ASKER`, and the two MCP tools are removed. No other consumers were found in
p-agent, one-claude, payments-knowledge or `~/.claude`.

**Nit (not a finding):** `askerACL.resolved` and the `$resolved` SQL parameter can no longer
be false on a filtered path.
