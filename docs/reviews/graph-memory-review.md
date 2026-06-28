# Graph Memory — Code Review

> **Last reviewed:** 2026-06-28 (commit `504d69e`)
> **Scope:** `internal/graph/` (jobs queue/worker/janitor/heartbeat/backoff/state/dispatcher/registry, `bfs/expand`, `acl/scopes`, `identity`, handlers: search/resolve/node/neighbors/detect_hot_topics/slack_notify, migration) + dashboard live/globe.
> **Build:** `go build ./internal/graph/...` passes (exit 0). LSP clean on spot-checked files.
> **Verdict:** REQUEST CHANGES — 1 Critical, 3 High, 5 Medium, 4 Low.

To refresh: re-run the review and overwrite this file, bumping the date/commit header.

## Fixes applied (2026-06-28, uncommitted)

Tracked in beads; verified with `go test` against a throwaway migrated pgvector DB.

- ✅ **C1** (`agent-mem-czc`, closed) — `/node` + `/neighbors` now enforce ACL via a shared `scopeVisible`/`askerScopeSet` helper (`handlers/acl_guard.go`). Hidden nodes 404; hidden neighbors are neither surfaced nor traversed.
- ◐ **H1** (`agent-mem-7h1`, open — partial) — removed the self-referential `slack_groups` query that leaked every `slack:*` scope to any usergroup member; ACL is now sourced solely from `graph.member_scopes` (**fail-closed**: real askers get no Slack scope until it's populated). Regression test `TestBuilder_SlackUsergroupDoesNotGrantChannelScope` added. **Remaining:** a `conversations.members` refresh job to populate `member_scopes` with `slack:CHANNEL` rows so per-channel Slack ACL actually works.
- ✅ **M5** (`agent-mem-w1o`, closed) — search SQL now treats NULL/`''` scope as open, matching `resolve.checkScope` (both route through `scopeVisible`); ACL build errors fail closed.

Remaining findings (H2, H3, M1–M4, L1–L4) are filed as open beads issues. Note: no current caller asserts `X-Asker-User`/`asker_eeid`, so the ACL filter path is built but unexercised today — these fixes have zero blast radius on current behavior.

---

## Severity summary

| Sev | Count |
|-----|-------|
| Critical | 1 |
| High | 3 |
| Medium | 5 |
| Low | 4 |

---

## CRITICAL

### C1 — `node` and `neighbors` read endpoints bypass ACL entirely
`internal/graph/handlers/node.go:43-78`, `internal/graph/handlers/neighbors.go` (whole handler) · **conf: HIGH**

`Search` (search.go:89-94) and `Resolve` (resolve.go:96-150) build an asker scope set and filter by `n.scope`. `GET /api/graph/node?id=...` and `GET /node/{id}/neighbors` do **neither** — they never read `X-Asker-User`, never touch `acl.Builder`, and the SQL has no `scope` predicate. Any key-bearing caller can read the **full body** (`ab.body_full`, node.go:54,59) of any node by id/url — including private Slack channels they aren't in — and walk the whole edge graph via `/neighbors`. An integration that correctly passes `X-Asker-User` to `/search` gets filtered results, then calls `/node` with an id from those results' `edges_out`/neighbors and reads content the user can't see.

**Fix:** thread the same asker→scopeSet check into both handlers. In `node.go`, after fetching the row, fetch `n.scope` and apply `checkScope` (resolve.go:280-293) before returning body; 404 if the asker (`eeid != 0`) lacks the scope. In `neighbors.go`, filter expanded nodes through the scope set as `Resolve.ServeHTTP` does (resolve.go:139-150). Factor `checkScope` + scope-set construction into one shared helper so all four read paths share a single enforcement point.

---

## HIGH

### H1 — ACL Slack-scope query is effectively a no-op / self-referential predicate
`internal/graph/acl/scopes.go:63-74` · **conf: MEDIUM**

The correlated `EXISTS` joins `slack_groups` to `people` but the only tie to the outer row is `AND n.scope = 'slack:' || split_part(n.scope, ':', 2)` — comparing `n.scope` to a value **derived from `n.scope` itself**, not to the group's channel. No join links the group/channel `g` to `n.scope`. So the subquery is true whenever the asker is in *any* slack group, regardless of channel — or returns nothing — either way not enforcing per-channel membership.

**Fix:** the group row must expose the channel id it grants; join on it, e.g. `WHERE n.scope = 'slack:' || g.slack_channel_id`. Add a `scopes_test.go` case asserting a member of group A (channel X) does **not** receive `slack:Y`. Verify against the `graph.slack_groups` schema first (see Open Questions).

### H2 — Heartbeat goroutine shares the run context; a hung handler is never timed out
`internal/graph/jobs/worker.go:46-58`, `internal/graph/jobs/heartbeat.go:57-61` · **conf: HIGH**

For `Heartbeat: true` types (`refresh_slack_groups`, `import_bamboohr`, `recompute_person_distance`), `runOne` skips the `context.WithTimeout` wrapper (worker.go:54). The heartbeat keeps bumping `lease_until` forever. A handler that hangs on a stuck external call runs **indefinitely**, the janitor never reclaims it (lease keeps advancing), and with `PoolSize: 1` that whole job type is wedged until process restart.

**Fix:** give heartbeat handlers a finite cap (`context.WithTimeout(ctx, entry.MaxRuntime)`, MaxRuntime ≫ lease, ~30m), OR ensure every external client inside them has its own `http.Client{Timeout}` / per-call deadline. `slackHTTP` (slack_notify.go:12) has a 15s timeout — confirm Gemini and BambooHR clients do too; if they do, this drops to MEDIUM but the structural "no ceiling on heartbeat jobs" gap remains.

### H3 — `detect_hot_topics` reschedule uses the cancelled handler context
`internal/graph/handlers/detect_hot_topics.go:58-70` · **conf: HIGH**

The self-reschedule `Enqueue` runs in a `defer` using the same `ctx` passed to the handler (line 62). When the dispatcher's `runCtx` deadline fires (Lease 120s, no heartbeat → `WithTimeout`) or on shutdown, `ctx` is already cancelled, so the `defer`'s `Enqueue` fails ("context canceled"), is logged as a warning, and **the self-rescheduling chain dies silently**. Hot-topic detection then never runs again until a process restart re-seeds it (server.go:167-172). The comment at line 59-60 claims the chain "never dies on a transient failure," but a handler timeout is exactly the killing case.

**Fix:** enqueue the next tick with a fresh background context + short timeout: `rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second); defer cancel()` inside the defer, decoupled from the handler run ctx. Consider a watchdog/cron that re-seeds if no `detect_hot_topics` row is queued/running (startup check at server.go:167 only runs at boot).

---

## MEDIUM

### M1 — Hot-topic dedup rollback creates a re-notify / duplicate-DM race
`internal/graph/handlers/detect_hot_topics.go:84-105` · **conf: MEDIUM**

Flow: `INSERT ... ON CONFLICT DO NOTHING` to claim (sub, thread) → send DM → on DM failure `DELETE` the claim so a retry resends. If the DM actually succeeded but the HTTP response was lost (timeout after Slack processed it), the code treats it as failure, deletes the dedup row, and the next 5-min tick **re-sends** — duplicate DMs. Claim+send+delete is also non-atomic (INSERT autocommits), leaving a small concurrent-run window (unlikely at `PoolSize: 1`).

**Fix:** prefer at-least-once + idempotency over delete-on-failure. Keep the dedup row even on send failure; record send status (`notified_at NULL` until confirmed), retry only NULL rows — or accept duplicates and drop the DELETE. Document the chosen guarantee.

### M2 — BFS edge attenuation is fixed `*0.5` per hop regardless of edge kind; frontier cap silently truncates
`internal/graph/handlers/resolve.go:103,122-128` · **conf: MEDIUM**

`frontier := bfs.NewFrontier(200)` caps at 200. On a dense seed (busy Slack channel root with hundreds of thread siblings via THREAD cohesion, expand.go:65-76), pushes beyond 200 are dropped — results depend on push order, and the trace reports `len(visited)`, hiding discarded neighbors. Attenuation is a flat `c.Score * 0.5` ignoring `n.EdgeKind`, so a THREAD sibling and a strong cross-source edge attenuate identically; a noisy thread can crowd out higher-value hops within the cap.

**Fix:** confirm `NewFrontier(200)` evicts lowest-score (check frontier.go); if FIFO/LIFO with a hard cap, switch to a bounded max-heap keyed on score. Consider per-kind attenuation (THREAD weaker). At minimum surface a `truncated bool` in `resolveTrace`.

### M3 — `MergeByEmail` canonical selection breaks when two rows both have eeids
`internal/graph/identity/identity.go:192-198` · **conf: MEDIUM**

The loop picks a non-nil-eeid row only `if canonical.eeid == nil` (line 195). If two rows for the same email both have eeids (two BambooHR records, or a bad backfill), it keeps the first and silently merges away a *different* employee's eeid row — conflating two real people under one canonical id, which propagates to all `author_person_id` (215-218).

**Fix:** if more than one candidate has a non-nil eeid **and** they differ, do not merge — log and skip (data-quality alarm). Add a test for the two-distinct-eeids case.

### M4 — Name-based identity merge registered but unreviewed here; high blast radius
`internal/graph/handlers/handlers.go:58` (`merge_identities_by_name`) · **conf: LOW**

Name-based merging (commit `18ec3f0`) is risky: two distinct employees sharing a display name would be merged, rewriting `author_person_id` graph-wide with no easy undo (soft-delete + pointer at identity.go:228-234, but node rewrites at :215 are destructive). Handler body not opened this pass.

**Fix:** require a corroborating signal (same email OR same normalized name AND overlapping source/team), never name alone; gate behind dry-run/report mode. Recommend a focused review of `NewMergeIdentitiesByNameHandler`.

### M5 — Search/resolve swallow ACL builder errors and diverge on NULL-scope visibility
`internal/graph/handlers/search.go:92`, `internal/graph/handlers/resolve.go:96` · **conf: MEDIUM**

Both do `scopes, _ := h.aclBld.For(ctx, ...)`, discarding the error → `scopes` nil on DB hiccup. Worse, NULL/empty scope is handled **inconsistently**: search SQL `$3::text[] IS NULL OR n.scope = ANY($3)` **excludes** NULL-scope rows for a real asker (NULL never matches `ANY`), but `checkScope` (resolve.go:280-293) treats `scope == nil || ""` as **open**. Same node, two visibility answers — and a misconfig that leaves nodes unscoped becomes globally readable via resolve with no signal.

**Fix:** pick one NULL/empty-scope rule and apply it identically in search SQL and `checkScope`. Log (don't discard) ACL build errors; consider failing closed for a real asker when scopes can't be computed.

---

## LOW

### L1 — Backoff/attempts accounting can over-count on lease-expiry path
`internal/graph/jobs/janitor.go:71-81`, `queue.go:115`, `worker.go:86` · **conf: MEDIUM**

`Claim` increments `attempts` at claim time. Janitor requeue doesn't touch attempts (correct), but a job claimed-then-janitor-reclaimed without ever running the handler still consumes an attempt. No bug, but worth a comment: "attempts counts claims, not handler runs."

### L2 — Dead/divergent default registry config
`internal/graph/jobs/registry.go:42-50` · **conf: HIGH**

Every entry from `NewRegistry()` is overwritten by `RegisterAll` (handlers.go:50-75) at boot, since handler constructors return their own `Entry`. The defaults silently drift from real values — a maintenance trap.

**Fix:** delete the per-type defaults in `NewRegistry()` (keep only the empty map), or have `RegisterAll` override only `Handler` and inherit tuning. One source of truth.

### L3 — `lookupAskerEEID` allows asker spoofing across identity columns
`internal/graph/handlers/search.go:304-314` · **conf: LOW**

Header matched against `slack_user_id OR email OR github_login OR jira_account_id` with `LIMIT 1`, no source qualifier — a caller can pass any user's email/login as `X-Asker-User`. By-design today (router.go:6-17 trust note: asker "advisory until authenticated"), but the `OR` across 4 columns can also resolve the *wrong* person.

**Fix (when hardening):** qualify by claimed source (`X-Asker-Source` + column) and bind eeid to a verified principal.

### L4 — `slackPostJSON` ignores HTTP status code
`internal/graph/handlers/slack_notify.go:75-80` · **conf: MEDIUM**

Decodes the body regardless of `res.StatusCode`. A 429/5xx with a non-JSON body makes Decode fail or leaves an empty `Error`, producing opaque failures; the 429 `Retry-After` is dropped.

**Fix:** check `res.StatusCode`; honor `Retry-After` on 429; return a typed error.

---

## Open questions (low-confidence — surfaced, not blocking)

- **Slack ACL correctness (H1):** could not see the `graph.slack_groups` schema. If `member_user_ids` is the only column and there's genuinely no channel column, the per-channel Slack ACL may be unbuildable as written. Confirm schema before accepting the H1 fix.
- **Name-merge handler body** (M4) not read this pass — needs its own review.

---

## Positive observations

- Job queue claim is textbook: `FOR UPDATE SKIP LOCKED` + atomic `UPDATE ... RETURNING` (queue.go:99-118); janitor reclaim also uses SKIP LOCKED (janitor.go:62-68).
- `Backoff` (backoff.go:14-30) — exponential, capped, jittered, cap re-applied after jitter.
- `IsRetryable` (backoff.go:57-77) — sensible 429/5xx-retry, 4xx-fatal, network-retry classification, well documented.
- `identity.Resolve` cycle detection guards the `merged_into` chain (identity.go:259-262).
- `MergeByEmail` wraps node + identity_map rewrites + soft-delete in one transaction with rollback (identity.go:200-240).
- ACL `For` cache copies the slice before returning (scopes.go:40) — no aliasing of cached data.
- Thread-cohesion expansion gated to unfiltered traversals only (expand.go:65) so kind-scoped queries stay edge-pure.
- `detect_hot_topics` checks `RowsAffected()` on the dedup insert (line 91) rather than trusting `ON CONFLICT`.

---

## Recommendation

**REQUEST CHANGES.** The Critical unfiltered `/node` + `/neighbors` read path is a cross-channel data leak that undermines the scope model the rest of the feature enforces, and H1 (Slack-scope query) looks structurally incorrect. The two HIGH robustness issues (heartbeat has no ceiling; hot-topic chain dies on the very timeout case its comment claims to survive) should land in the same pass. The queue/backoff/identity-transaction core is genuinely solid — the gaps are concentrated at the ACL enforcement edges and the self-rescheduling/heartbeat lifecycle.
