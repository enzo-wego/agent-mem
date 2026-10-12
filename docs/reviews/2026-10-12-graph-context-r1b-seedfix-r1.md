# Review: fix/eval-seed-check-missing-node (R1b seed-check fix), round 1

- Branch: `fix/eval-seed-check-missing-node`, HEAD `da606a6`, base `origin/main`
- Scope: `cmd/agent-mem/eval_prepare.go` (`leadSeedsPresent`), two test files, plan doc A7
- Engine: deep-review, Round 1 (no PR, local review)

## Verdict

**APPROVE** with two LOW, non-blocking findings.

## What the fix does

`leadSeedsPresent` now skips a canonical seed whose `node_meta` entry is missing or has
`exists: false`. Everything else is unchanged: an existing seed still has to be a hop-0 item,
or its `thread_root` does.

## Checks asked for

| Check | Result | Evidence |
|---|---|---|
| Fix sits in the one shared function | Yes | `rg leadSeedsPresent` finds only 2 non-test callers: `eval_prepare.go:355` (prepare) and `eval_relation.go:175` (`loadRelationContext`, used by relation-assemble and relation-check). No other code runs the hop-0 seed check. `eval_common.go:477` and `eval_relevance.go:206` use `CanonicalSeeds` only as a set, not for this check. |
| The skip cannot hide a real mismatch for an existing seed | Yes, for a fresh prepare | `eval_prepare.go:282` appends every seed to `ids`. `fetchNodeMeta` (`eval_common.go:348-375`) writes an entry for every id it is asked about: `exists=true` when the row is in `graph.nodes` with `deleted_at IS NULL`, otherwise `exists=false`. So every seed has an entry, and `exists=true` always takes the old strict path. Exception: `--refresh` (F1). |
| node_meta in prepare is freshly built, so `exists` is current | Only without `--refresh` | Without `--refresh`, `need = ids` and the whole map comes from the same read-only transaction that canonicalized the seeds (`eval_prepare.go:328-352`). With `--refresh`, existing entries are reused as they are (F1). Plan step B1 runs prepare without `--refresh`, so the planned run is not affected. |
| relation-check reads the committed node_meta the same way | Yes | `loadRelationContext` reads `node_meta.json` (`eval_relation.go:159-166`) and calls the same function with the `canonical_seeds` from `replay.json`. Prepare writes both files together and only after everything succeeds, so their entries match. |
| Tests fail without the fix | Yes, checked by running them | I built `HEAD` with `eval_prepare.go` taken from `origin/main` (in a scratch copy) and ran `go test -run 'Lead\|Relation\|Prepare\|Seed'`. Three tests fail: `TestLeadSeedsPresent` (`eval_lead_r1_test.go:115`), `TestRelationCheckSeedMismatch` (`eval_lead_test.go:942`, `seed_mismatch`) and `TestSeedCheckSkipsMissingNodes` (`eval_lead_test.go:1331,1334`). At HEAD all three pass. The `exists=true` negative cases still assert failure (`eval_lead_r1_test.go:111`, `eval_lead_test.go:1338`, `eval_lead_test.go:937`). |

`go vet ./...`: clean. `go test ./cmd/agent-mem/ -run 'Lead|Relation|Prepare|Seed'`: ok.

## Findings

### R1-F1 LOW: under `--refresh`, a stale `exists:false` entry now skips the seed check
- Location: `cmd/agent-mem/eval_prepare.go:331-341` (with the new skip at `:171-173`)
- Evidence: with `--refresh`, an id that already has an entry in `node_meta.json` is left out of `need` and never fetched again. Suppose a seed was recorded with `exists:false` (as case-03's seed is today) and is ingested later. A later `prepare --refresh` keeps the stale `exists:false`, and the new guard then skips that seed. If resolve-lead does not return it as a hop-0 item, the result is a real `seed_mismatch` that nobody sees. Before this change, the same stale entry made prepare abort, so the failure was loud.
- Why LOW: this is an offline eval harness and the damage can be undone by re-running prepare. The path needs the operator to pass `--refresh`, which plan B1 explicitly avoids. `--refresh` already leaves `thread_root` stale by design.
- Fix: in the refresh loop, fetch again any id whose entry has `exists:false`, as well as ids with no entry: `if m, ok := meta[id]; !ok || !m.Exists { need = append(need, id) }`. An alternative is to always put the seeds into `need`.
- Confidence: Verified (code read; follows from the loop at `:335-339`).

### R1-F2 LOW: "no meta entry" skips silently in relation-check instead of failing closed
- Location: `cmd/agent-mem/eval_prepare.go:171-173` (reached from `eval_relation.go:175`)
- Evidence: in prepare, `!ok` can never be true, because `fetchNodeMeta` always writes an entry for each seed. In relation-assemble or relation-check, a missing entry can only mean `replay.json` and `node_meta.json` no longer match (a hand edit, or files from different runs). The new code skips such a seed and passes. The old code returned `seed_mismatch`.
- Fix (optional): treat `!ok` as a mismatch (`return false`) and skip only when `ok && !m.Exists`. Then change the "seed without meta must be skipped" assertions (`eval_lead_r1_test.go:114`, `eval_lead_test.go:1333`) to expect failure.
- Confidence: Verified (code read).

## Not findings (checked)

- A case where every seed has `exists:false` now passes the seed check whatever the hop-0 items are. That is what the fix intends: lead mode cannot return a node that was never ingested. `nonSeed` (hop != 0) is still computed from the lead items as before.
- A soft-deleted seed (`deleted_at` set) maps to `exists:false` and is skipped. That is consistent, because lead mode will not return it.
- The plan doc change (A7, line 315) matches the code.

## Receipts

- graph: degraded: code-review-graph not run; I enumerated callers with `rg` (2 call sites, both covered)
- fan_out: the four finder bundles were **not** spawned. I traced inline because the logic change is 4 lines. None of the listed exceptions applies, so this breaks the deep-review fan-out rule.
- coverage gate (`validate-ledger.sh --coverage-only`): **FAIL**, on axis `fan_out` (`expected 'ran (4/4) | inline: <listed exception> ...'`). I did not invent a receipt to pass it. The other axes were filled: value_reach ran, execution ran (vet, tests and a mutation run), security not-applicable (offline eval harness), contracts ran, tests_audit ran.
- Nothing was committed, pushed or posted. `.env` was not read. No live server was used.

VERDICT: APPROVE
