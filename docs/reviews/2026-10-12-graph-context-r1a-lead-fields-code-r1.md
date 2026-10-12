# Code review R1: graph lead mode (R1a)

Date: 2026-10-12. Branch `feat/graph-lead-mode`. Reviewed HEAD `92d6201`; merge-base with
`origin/main` is `8e97d74`. Engine: deep-review, Round 1, read-only. Plan:
`docs/ai/2026-10-12-graph-context-r1a-lead-fields.md`, with plan reviews r1, r2 and r3.

## Verdict: APPROVE

There is nothing at CRITICAL or HIGH, and nothing blocks the merge.
- **Flag off or no header:** every route behaves exactly as on `main`. This was proven by running the branch's
  compatibility goldens against a build of `8e97d74`.
- **ACL and masking in lead mode:** both hold on every route.
- **Migrations:** follow the one-index-per-file, `CONCURRENTLY`, `NO TRANSACTION` rule.
- **`--quiet`:** leaks no URL or request text.

There is one MEDIUM question (F1), about which column `lead_summary` reads. Answer it before p-agent starts
consuming the field. The four LOW items are cheap follow-ups.

## Receipts

- `graph: ran (25 changed symbols, 24 call sites, 2 unresolved, 0 truncated, built 92d6201)`
  - This is code-review-graph, refreshed at HEAD (`head_matches_build: true`).
  - `rg` covered the 2 bare-name `noSideEffects` call sites.
- `fan_out: ran (4/4)`. All four finder bundles returned: structural, temporal, tests-and-claims and outward.
- Security axis: a scoped manual pass under the Security/privacy lens.
  - The harness `security-review` skill was not run, because this is an isolated worktree.
- `validate-ledger.sh --coverage-only` returned `OK: coverage valid`.

### Changed symbols (production code)

| Symbol | Callers | Source |
|---|---|---|
| `Resolve.ServeHTTP` (resolve.go) | router | both |
| `Resolve.WithLeadMode`, `Search.WithLeadMode`, `Node.WithLeadMode` | router.go:85,92,96 | rg |
| `CanonicalizeSeeds` → `canonicalizeSeedsNotify` | eval_prepare.go:111, resolve.go:267,635 | both |
| `sortedLinks` (extracted from `seedLinks`) | resolve.go:854, lead_resolve.go:341 | rg |
| `leadGate` | resolve.go:204, node.go:101, search.go:119 | rg |
| `leadRoots` | lead_resolve.go:59, lead_search.go:25, node.go:73 | rg |
| `leadEnrich` | lead_resolve.go:294, lead_search.go:39, node.go:85 | rg |
| `leadText` / `capRunes` | node.go:92, search.go:391,418, lead.go:318 | rg |
| `Resolve.serveLead`, `Node.serveLead`, `Search.leadPage` | resolve.go:362, node.go:166, search.go:372 | rg |
| `traceHTTP.ServeHTTP`, `headerNoSideEffects`, `noSideEffects` | resolve.go:502, lead_resolve.go:328, context_trace.go:92 | graph (unresolved) + rg |
| `runRelevance` / `runRelevanceInner` / `quietEvalError` / `evalErrorCategory` | eval.go:53 | rg |
| `mountRoutes`, `NewServer` (flag wiring) | Mount, worker | graph |

### Execution

| Command | Result |
|---|---|
| `go vet ./...`, `go build ./...` | clean |
| `PATH=/opt/homebrew/bin:$PATH make test-db` (throwaway container) | 2318 pass, 3 fail, 4 skip |

- **Failures:** the 3 failures are the known `internal/graph/jobs TestPeriodicTickerReclaimInterleavings` and its 2 subtests.
- **Skips:** the 4 skips are pre-existing and environment-gated.
- **New tests:** there are 266 `TestLead*` and quiet-test pass events with 0 failures. No new test calls `t.Skip`.
- **Goldens re-run on `main` (deciding experiment for a candidate):**
  - Steps:
    - Extracted `git archive 8e97d74` into the scratchpad.
    - Copied `lead_a_test.go` and the 30 `lead_a_*.golden.json` files into it.
    - Removed only the branch-only `LeadModeEnabled` / `WithLeadMode` references and the `X-Graph-Lead: off` header assertion.
    - Migrated a scratch pgvector container and ran `go test -run 'TestLeadAInactiveRequestsMatchMain|TestCompatibility'`.
  - Result: `ok`, with **38/38 subtests passing on main**. That covers every route, every caller and all 9 env×header configs.
  - So the goldens are main's output, and the branch's inactive path equals main's.
  - The container and scratch tree were removed afterwards.

## Findings

### F1 MEDIUM (needs author confirmation): `lead_summary` for Slack threads is the topic label, not the summary

`internal/graph/handlers/lead.go:299-304`

**What the code does**
- For a Slack item, `lead_summary` prefers `thread_summaries.summary` over `artifact_index.summary`.
- The producer writes `ds.Topic` into `thread_summaries.summary` (`summarize_thread.go:227`).
- The prompt defines `topic` as "short factual label, max 10 words" (`summarize_thread.go:352`).
- The 2-3 sentence text is `overview`.
- `artifact_index.summary` for a Slack root is `topic + "\n\n" + overview` (`index_artifact.go:261-266`). That is strictly richer, and lead mode overrides it with the bare topic.

**Effects**
- Lead resolve with an empty `n.title` sets the title to `ts.summary` (`lead_resolve.go:186`). `lead_summary` gets the same string, so the item carries the topic twice and `context_tokens` counts it twice.
- The overview never reaches lead resolve or lead node.
- Hybrid lead rows carry title = topic, `summary` = overview, and `lead_summary` = topic again.

**Why it is a question**
- The code follows the approved plan to the letter: A4 says "`thread_summaries.summary`".
- So this is a plan-level gap, not a deviation.
- EnzoBot is the consumer the feature exists for, and it would get a 10-word label where a summary was intended.

**Suggested fix:** for Slack, prefer `artifact_index.summary`, or `thread_summaries.overview` with the topic as a prefix. Then the 600-rune mask and cap does real work.

**Confirmation:** confirm the intended column before the p-agent rounds read `lead_summary`.

### F2 LOW: the migration test fails as soon as any newer migration lands

`internal/graph/handlers/lead_c_migration_test.go:78-80`

- The test calls `t.Fatalf` unless `graph_nodes_thread_key_idx` is the newest migration in `migrations/`.
- The next unrelated migration on `main` will turn this test red inside someone else's PR. This is certain to happen, not a timing risk.
- `ApplyVersion` already works for any version.

**Fix options:**
- Drop the `maxVer` guard.
- Or run the "roll back only the last" case only while the index migration is still the newest, and keep the separability assertions version-addressed.

### F3 LOW: worker panics at startup if `NewResolve` fails

`internal/graph/handlers/router.go:95-96`

- The code is `resolve, _ := NewResolve(deps.DB); resolve.WithLeadMode(...)`.
- `NewResolve` returns `nil, err` when `scoring.LoadWeights` fails (`resolve.go:101-104`, `combine.go:56`).
- **On `main`:** a nil handler was mounted, so only `/api/graph/resolve` failed, at request time.
- **On this branch:** the nil dereference happens in `mountRoutes`, so the whole worker fails to start.
- Likelihood is low, because migrations ran just before against the same pool.
- Fix: guard with `if resolve != nil`. `Search` and `Node` never return nil.

### F4 LOW: lead search applies the epic filter to reply roots; the plan says resolve only

`internal/graph/handlers/lead_search.go:18-25`

- Plan A2a limits the epic/business root check to resolve.
- `leadPage` passes `epicKeys` with `exempt` set to the result ids only.
- So on `/search?epic=K` with `X-Graph-Lead: 1`, a reply that is in the epic, whose root is not in the epic and not on the page, is dropped.
- This over-filters and leaks nothing, but it is a silent deviation, not recorded in the Findings section.

**Fix options:**
- Pass `nil` epic keys from search.
- Or record the stricter rule in the plan's Findings.

### F5 LOW: the acceptance-1 query counter does not see most lead-only queries

`internal/graph/handlers/lead_a_test.go:50`

- The counter matches three markers: `graph.answer_bots`, `lead:aggregate` and `unnest($1::text[], $2::text[])`.
- It cannot see:
  - the first `leadRoots` query (`lead.go:94`)
  - `epicMembers` inside `leadRoots` (`lead.go:158`)
  - the misses query (`lead_resolve.go:152`)
  - the display query (`lead_resolve.go:184`)
  - the links query (`lead_resolve.go:236`)
- Today every lead query sits behind one `if lead`, so nothing leaks.
- But the test's claim "no lead-only query runs on inactive requests" is only partly enforced.
- Fix: tag each lead-only SQL `/* lead:<name> */` and count by the `lead:` prefix.

## Checked clean (focus areas)

### 1. Inactive path is identical to main

**Resolve**
- The decoder is untouched (`src = r.Body`). The `canonicalizeSeeds` path is unchanged.
- `noSideEffects` matches the old inline rule. `sortedLinks` is a pure extraction.

**Response structs**
- The new struct fields are a nil embedded `*leadFields` plus `FocusTS`, `From` and `To` (all omitempty). They add no keys.

**Node and search**
- They only add `leadGate` before `requireAsker`.

**Epic and neighbors**
- Untouched.

**Proof**
- The executed run on `8e97d74` above.

### 2. ACL and masking in lead mode

**Root eligibility (A2a)**
- One rule, `leadRoots`, is shared by resolve, node, default search and hybrid search.
- The root must exist, not be deleted, have `type='slack'`, have the same scope as the member and pass `aclVisibleSQL`.
- The epic check also applies, unless the root is itself an authorized candidate.
- Resolve's exempt set is `filtered`, which is already filtered by epic and ACL.

**Enrichment**
- The `leadEnrich` data query has no ACL of its own. Every caller passes only eligible roots or ids that were already authorized.

**Aggregate**
- It applies `aclVisibleSQL`, `deleted_at IS NULL` and `type='slack'` per row.

**Links**
- Links require both original endpoints to be scored, authorized members of selected groups.
- Self-links are dropped.

**Masking**
- Link prose and titles are masked for filtered callers by the generic `maskGraphResponse` string walk (`mask.go:107-123`).
- `leadText` runs `redact.Mask` before `capRunes` on `lead_summary`, node `summary`, default `summary` and hybrid `summary`/overview.
- Cutting a masked placeholder reveals nothing.
- Admin detection is the shared `isAdminRequest`.

**Fail-closed**
- Every lead query error is 503 `lead_lookup_failed` (Query, Scan and `rows.Err()`, plus missing-row checks).

### 3. No-trace switch and `--quiet`

**No-trace switch**
- `traceHTTP` skips only `Enqueue` on header `1` or `true` (case-insensitive). The request id header and JSON `request_id` stay.
- The header overrides the body flag, so header `0` beats body `true`.
- The lead `fetch_body` enqueue is gated by `noSideEffects`.
- No new log lines.

**`--quiet`**
- The request echo is replaced by the variant name.
- The unreachable-server error becomes a category.
- Run errors are reduced to `transport`, `http_<n>` or `decode`.
- Every other error becomes `setup` and never wraps the cause.
- Cobra prints only `Error: eval relevance failed: <category>`.

### 4. SQL and migrations

**Index use**
- The index expression and partial predicate match the aggregate exactly.
- `TestLeadC_AggregateScaleAndPlan` runs at 200k nodes and asserts both that `graph_nodes_thread_key_idx` appears in the plan and that there is no Seq Scan.

**Migration files**
- One index per file.
- `-- +goose NO TRANSACTION`, `CREATE INDEX CONCURRENTLY IF NOT EXISTS`, and `DROP INDEX CONCURRENTLY IF EXISTS` on the way down.
- The `answer_bots` seed is idempotent.
- Separability is proven via `goose_db_version`, with the caveat in F2.

**Query shape**
- No N+1, no rows left open across queries, and the request context is used throughout.

### 5. Tests

- Acceptance items 2-10 are each pinned by assertions that would fail on a wrong implementation.
- The tests include vacuity guards: `after_acl`, plain controls, a live-bot control and pre-checks that fixtures are masked.
- Gaps: F2 and F5.

## Dismissed or not reported

**Dismissed (would-be MEDIUM)**
- **Goldens possibly recorded from the branch, not main.** Refuted by the executed run on `8e97d74` (38/38).

**Dropped by the cost filter**
- **Interrupted `CONCURRENTLY` build leaves an INVALID index.** Pre-existing repo pattern.
- **Eval `request_ids` no longer resolve to trace rows.** Intended by A1.
- **Mixed author attribution on reply items.** Documented decision.
- **Uncapped thread-summary title.** Not required by the plan.
- **No body-size limit in the lead decode.** Callers are authenticated.
- **`checkScopes` error shows as an empty result.** Unchanged from main.
- **`arm_errors` text in the `--json` file.** Outside the plan's stdout/stderr/error scope.
- **Lead traces carry no lead marker.** Observability only.

<details>
<summary>review-ledger:v1</summary>

```json
{
  "round": 1,
  "reviewed_sha": "92d620134489416d78f7574f7e6587a2876b7ee4",
  "base_sha": "8e97d74",
  "verdict": "APPROVE",
  "producer": "payments@0.26.0",
  "reviewer_agent": {"harness":"Claude Code","model":"claude-opus-5-5","source":"transcript","harness_version":"2.1.296","effort":"medium"},
  "coverage": {
    "graph": "ran (25 changed symbols, 24 call sites, 2 unresolved, 0 truncated, built 92d6201)",
    "fan_out": "ran (4/4)",
    "value_reach": "ran: new headers X-Graph-Lead / X-Graph-No-Side-Effects reaction, error codes bad_max_items / lead_lookup_failed, lead response fields, graph.answer_bots; no persisted state value introduced or repurposed",
    "execution": "ran: go vet/build clean; make test-db 2318 pass, 3 fail (pre-existing jobs TestPeriodicTickerReclaimInterleavings + subtests), 4 env-gated skips; acceptance-1 goldens re-run against a git archive of 8e97d74 on a scratch pgvector container: 38/38 subtests match",
    "security": "ran: scoped manual pass under Security/privacy lens (ACL/root eligibility, mask-before-cap, admin determination, --quiet leak paths, no-trace side effects); harness security-review not run because the review checkout is an isolated worktree",
    "cut": "skipped: advisory complexity cut is apex-review's stage; standalone finding pass",
    "ticket": "ran: approved plan docs/ai/2026-10-12-graph-context-r1a-lead-fields.md plus plan reviews r1-r3 used as the acceptance anchor; no Jira ticket on this branch",
    "contracts": "ran: X-Graph-Lead and X-Graph-No-Side-Effects senders checked in p-agent src, dashboard, graphmcp and gh code search; eval relevance output parsers; context_traces consumers; migration name collisions",
    "tests_audit": "ran: full test-review.md audit of lead_a/b/c, lead_c_migration and eval_quiet tests against acceptance items 1-11"
  },
  "findings": [
    {"id": "R1-F1", "sev": "MEDIUM", "loc": "internal/graph/handlers/lead.go:299", "title": "lead_summary for Slack prefers thread_summaries.summary, which holds the <=10-word topic, over the richer artifact_index.summary", "status": "open", "confidence": "needs-author-confirmation",
     "evidence": [
       {"kind": "static", "path": "internal/graph/handlers/summarize_thread.go:227", "conclusion": "thread_summaries.summary is written from ds.Topic"},
       {"kind": "static", "path": "internal/graph/handlers/summarize_thread.go:352", "conclusion": "topic is defined as a short factual label, max 10 words"},
       {"kind": "static", "path": "internal/graph/handlers/index_artifact.go:266", "conclusion": "artifact_index.summary for a Slack root is topic plus overview"}],
     "note": "matches plan A4 wording; plan-level question"},
    {"id": "R1-F2", "sev": "LOW", "loc": "internal/graph/handlers/lead_c_migration_test.go:78", "title": "migration test fails once any newer migration exists", "status": "open", "confidence": "verified",
     "evidence": [{"kind": "static", "path": "internal/graph/handlers/lead_c_migration_test.go:78", "conclusion": "t.Fatalf unless indexVer == maxVer"}],
     "note": "certain to break the next unrelated migration PR"},
    {"id": "R1-F3", "sev": "LOW", "loc": "internal/graph/handlers/router.go:96", "title": "nil *Resolve dereference at mount when NewResolve fails", "status": "open", "confidence": "likely",
     "evidence": [{"kind": "static", "path": "internal/graph/handlers/resolve.go:101", "conclusion": "NewResolve returns nil, err on LoadWeights failure"}],
     "note": "was request-time failure of one route on main; now a startup panic"},
    {"id": "R1-F4", "sev": "LOW", "loc": "internal/graph/handlers/lead_search.go:25", "title": "lead search applies the epic filter to reply roots; plan A2a says resolve only", "status": "open", "confidence": "likely",
     "evidence": [{"kind": "static", "path": "internal/graph/handlers/lead.go:153", "conclusion": "non-exempt roots need inEpic membership"}],
     "note": "over-filtering only; no leak"},
    {"id": "R1-F5", "sev": "LOW", "loc": "internal/graph/handlers/lead_a_test.go:50", "title": "acceptance-1 query counter misses five lead-only queries", "status": "open", "confidence": "verified",
     "evidence": [{"kind": "static", "path": "internal/graph/handlers/lead_a_test.go:50", "conclusion": "markers cover answer_bots, lead:aggregate and the two-array unnest only"}],
     "note": "no leak today; weak guard for future changes"}
  ],
  "dismissed": [
    {"id": "R1-D1", "sev": "MEDIUM", "loc": "internal/graph/handlers/lead_a_test.go:352", "title": "lead_a goldens may be recorded from the branch rather than main", "note": "refuted by running the goldens against 8e97d74",
     "evidence": [{"kind": "attempt", "target": "scratch checkout of 8e97d74 + scratch pgvector", "command": "go test -count=1 -v -run 'TestLeadAInactiveRequestsMatchMain|TestCompatibility' ./internal/graph/handlers/", "exit": 0, "output": "ok github.com/agent-mem/agent-mem/internal/graph/handlers; 38 subtests PASS", "conclusion": "refutes"}]}
  ]
}
```

</details>
