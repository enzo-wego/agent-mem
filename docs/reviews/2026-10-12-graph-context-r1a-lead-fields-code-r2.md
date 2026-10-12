# Deep review r2: graph lead mode, fix round 1 (F1-F5)

- Branch: `feat/graph-lead-mode`, worktree `agent-mem-wt-lead`
- Scope: delta `92d6201..27342c4` only (Round-N re-review of `-code-r1.md`)
- Baseline for the inactive path: `origin/main` `8e97d74`
- Reviewer: deep-review engine, read-only (no source edits, no commits)
- Date: 2026-10-12

## Verdict

**APPROVE.** All five r1 findings are fixed as the plan's "Fix round 1" section specifies.
The delta adds no new findings.

The two shared non-lead files touched (`epic_scope.go`, `search.go`) cannot change the
inactive path:

- `epicMembers` still sends exactly main's SQL text, because the tag is `""`.
- The `search.go` edit sits inside `if lead`.

The full DB suite is green, including the 38/38 inactive-parity subtests.

## Evidence run

| Check | Result |
|---|---|
| `go build ./...` at `27342c4` | OK |
| `go vet ./...` | OK, clean |
| `make test-db` (isolated `pgvector/pgvector:pg16` container, `TEST_DATABASE_URL` unset; Makefile migrates then runs `go test -p=1 -count=1 -json ./internal/graph/... ./cmd/agent-mem/...` through `scripts/check-db-skips.go`) | exit 0. All 16 packages pass, including `internal/graph/handlers` and `internal/graph/jobs` (the known flake passed). |
| `TestLeadAInactiveRequestsMatchMain` | pass, 38/38 subtests |
| `TestLeadAActiveRequestsRunLeadQueries` (new tag assertions) | pass |
| `TestLeadB3RootEligibility`, `TestLeadB3ResolveMixedThreads`, `TestLeadB4MaskingBeforeCapMountedRoutes`, `TestLeadB5RootAbsentFromCandidates`, `TestLeadC_Search{Default,Hybrid}Fields` | pass |
| `TestLeadC_Migration*` (3 tests) | pass |
| `TestMountSurvivesNewResolveFailure` (new; also run standalone, no DB needed) | pass |
| Skips in the run | 4, all pre-existing and outside lead code (`TestDescribeAttachmentHandler_UnsupportedMime`, `TestParseDocument_RichPDF`, `TestTopicJudgeFixture`, `TestTopicJudgeGolden`). `check-db-skips` accepted them. |
| TODO / FIXME / `t.Skip` / stubs added in the delta | none (grep of added lines under `internal/`) |

## Inactive path vs main `8e97d74` (hard requirement)

- **`epic_scope.go`.** `epicMembers` is now `return epicMembersTagged(ctx, db, "", keys, ids)`.
  - The SQL becomes `"" + "\nSELECT DISTINCT node_id …"`, which is byte-identical to main's literal.
  - Args, the no-keys short-circuit and the error path are unchanged.
  - Every non-lead caller still goes through `epicMembers`. Only `leadRoots` passes `/* lead:epic */`.
- **`search.go`.** The only edit in this delta is the `leadPage` call inside `if lead { … }` (line 371).
  - `epicKeys` is still computed at line 139 and still feeds `searchFilter.epic` at line 170, exactly as on main.
  - `lead` is false whenever the flag is off or the header is absent (`leadGate`, unchanged in this delta).
- **`router.go`.** When `NewResolve` succeeds, the code is identical to `92d6201`.
  - When it fails, the nil `*Resolve` is mounted exactly as main did.
  - Main also called no method on it at mount time.
- **SQL tags.** Every other tag (`/* lead:* */`) is on a query reachable only under `if lead`, in `lead.go` or `lead_resolve.go` `serveLead`.
- **Test proof.** The inactive test asserts the tag counter is empty (`len(c) != 0` fails, `lead_a_test.go:387`, `:476`). It now sees every lead tag, and it passes.

## r1 findings at HEAD

| ID | r1 sev | Status | Verification |
|---|---|---|---|
| R1-F1 | MEDIUM | **fixed** | `lead.go` enrich drops the `thread_summaries` join and `tsSummary`. `summary := d.aiSummary` for every type, Slack roots included, then `leadText(admin, summary)` (mask, then 600-rune cap, unchanged). Resolve's title still falls back to `ts.summary` (topic) in the `/* lead:display */` query (`lead_resolve.go:184-186`). Tests were moved to the index summary: B3 mixed threads, B4 (`texts[2]` is the root's index text for resolve, node and both search modes), B5, C thread A/B. The B3 fixture now also writes the sentinel into the hidden root's `artifact_index`, so the no-leak assertions now cover the new source column. |
| R1-F2 | LOW | **fixed** | The `maxVer` guard is gone (`lead_c_migration_test.go:74`). Versions are found by file suffix, and `indexVer > botsVer` is kept. Goose v3.27.0 `Provider.apply` has no ordering check for up or down (`provider.go:468-508`), so `ApplyVersion(indexVer, false/true)` keeps working after a newer migration lands. Leftover nit, not a finding: the comment at the rollback step still says "Roll back only the newest migration". |
| R1-F3 | LOW | **fixed** | `router.go:96-98` nil guard. Without it, `WithLeadMode` writes `h.leadEnabled` on a nil receiver (`resolve.go:117`) and panics, so the new test fails on the old code for both `lead=false` and `lead=true`. The test uses a closed pool, so `LoadWeights` fails and `NewResolve` returns `(nil, err)`, which the test asserts. It mounts via `Mount`, recovers, and asserts resolve does not answer 200. |
| R1-F4 | LOW | **fixed** | `leadPage` no longer takes `epicKeys` and passes `nil` to `leadRoots`. `epicMembersTagged` then short-circuits, so no epic query runs from search. The epic filter is a scope filter, not ACL (`epic_scope.go:12-17`). Root eligibility still applies `aclVisibleSQL` on the root (`lead.go:129-133`), so nothing beyond the asker's ACL is exposed. `root_outside_epic` now expects search and hybrid to keep the reply, as node already did. Resolve still passes its epic keys (unchanged). |
| R1-F5 | LOW | **fixed** | 8 lead-only queries carry `/* lead:<name> */`: `roots`, `roots_eligible`, `epic`, `enrich`, `aggregate`, `misses`, `display`, `links`. A grep shows no untagged `Query` in `lead.go` or `lead_resolve.go`. The counter counts tags by regex, plus the two shape markers. The positive control now asserts `roots` and `enrich`, plus `misses` and `display` on resolve. `epic` and `links` are not asserted positively. That is acceptable: the requirement was that the inactive counter can see them, and it can. |

## Delta trace notes (no findings)

- **Removing `tsSummary` changes Slack `lead_summary` content.** It applies only in lead mode. A thread with a `thread_summaries` row but no `artifact_index` row now yields `""` instead of the topic. That is the decided F1 semantics. Lead mode is opt-in, so no inactive contract moves.
- **`/* lead:misses */SELECT` with no whitespace after the comment** is valid Postgres. The test run exercised it (`TestLeadC_ResolveCacheMissesAndEnqueue`).
- **Tagged SQL is new text, so pgx caches a new statement.** This happens only on lead requests and has no inactive effect.

## Findings

None.

## Coverage

```
graph: degraded: Round-N delta traced manually with rg (13 files, 7 lead SQL call sites, epicMembers callers); code-review-graph not run
fan_out: inline: Round-N delta
```

Coverage manifest validated with `validate-ledger.sh --coverage-only`, result `OK: coverage valid`.

<details><summary>review-ledger:v1</summary>

```json
{
  "round": 2,
  "reviewed_sha": "27342c4458d9d1c4ae12a925bce55d6ca107d8a2",
  "base_sha": "92d6201",
  "verdict": "APPROVE",
  "findings": [
    {"id": "R1-F1", "sev": "MEDIUM", "loc": "internal/graph/handlers/lead.go:296", "status": "resolved", "confidence": "verified"},
    {"id": "R1-F2", "sev": "LOW", "loc": "internal/graph/handlers/lead_c_migration_test.go:74", "status": "resolved", "confidence": "verified"},
    {"id": "R1-F3", "sev": "LOW", "loc": "internal/graph/handlers/router.go:96", "status": "resolved", "confidence": "verified"},
    {"id": "R1-F4", "sev": "LOW", "loc": "internal/graph/handlers/lead_search.go:26", "status": "resolved", "confidence": "verified"},
    {"id": "R1-F5", "sev": "LOW", "loc": "internal/graph/handlers/lead_a_test.go:56", "status": "resolved", "confidence": "verified"}
  ],
  "coverage": {
    "graph": "degraded: Round-N delta traced manually with rg (13 files, 7 lead SQL call sites, epicMembers callers); code-review-graph not run",
    "fan_out": "inline: Round-N delta",
    "value_reach": "ran: lead_summary source traced enrich -> leadText -> resolve/node/search; epicKeys traced search.go:139 -> searchFilter (unchanged) and leadPage (now nil)",
    "execution": "ran: go build, go vet, TestMountSurvivesNewResolveFailure, make test-db (isolated pgvector)",
    "security": "ran: delta-scoped manual pass; epic filter is a scope filter not ACL, ACL predicate on roots unchanged",
    "cut": "ran: no cuts warranted",
    "ticket": "ran: plan section Fix round 1 F1-F5",
    "contracts": "ran: inactive SQL text and response bytes unchanged; lead_summary content change is lead-only and opt-in",
    "tests_audit": "ran: no skips/TODOs/stubs added in delta; router test fails without the guard (nil field write)"
  }
}
```

</details>

APPROVE — no further review needed.
