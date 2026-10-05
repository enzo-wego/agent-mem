# Slack file user-token plan — review 2

## Scope and evidence

Reviewed the local plan, including its explicit Revision 2 overrides, against freshly fetched `origin/main` **5f466f136f9a265ed6d0ff7b9841d41d7da569f2**, not the older working-tree code. Compared the implementation and regression tests in p-agent **0a9b9b51d07b975a75d65c3724d55106af9bfdeb**. Used the previous review as the finding ledger and traced the revised design through the repository.

No live secret values, settings rows, env files, or production systems were read. This is a plan review, not implementation verification: no application build, database suite, or real Slack download was run. No dedicated security scanner ran; the credential boundary was reviewed manually.

## Previous findings

| R1 finding | Disposition | Evidence / remaining implementation obligation |
|---|---|---|
| 1. Caller-controlled attachment URL triggers the plan's STOP condition | Resolved by R2.1 | The plan now acknowledges the ingest path and explicitly replaces the STOP instruction with an HTTPS exact-host credential boundary, redirect restrictions, and an accepted residual authorization risk. Both bot and owner credentials are covered. |
| 2. Settings plumbing incorrectly optional | Resolved by R2.2 | All necessary config layers are now required, including `Update`, persistence, startup loading, snapshot and masked API response. The live getter is feasible: `internal/worker/server.go:158–185` constructs and registers `Deps` with the live config available. |
| 3. DB read failures masquerade as absent credentials | Resolved by R2.2–R2.3 | Reading live config avoids the error-swallowing `loadSetting` pattern. If the implementer chooses the DB alternative, errors must explicitly remain transient and receive a regression test. There is no apparent reason to need that alternative here. |
| 4. HTML refusal behavior ambiguous / untested | Resolved by R2.4 | Parameterized MIME parsing, bot HTML without owner credentials, owner HTML refusal, ordinary bot success, 401 and 429 now have specified outcomes/tests. R2 explicitly overrides the previous contradictory instruction to preserve today's behavior for bot HTML. |
| 5. No reliable off switch / cookie removal | Resolved by R2.5 | Separate clears are mandatory, omitted/blank saves preserve existing values, and persisted clearing plus bearer-only operation must be tested. |
| 6. Evidence does not cover the guarantees | Resolved by R2.6 | Worker/config tests are explicitly run; GET, PUT, settings-save logs, download logs and returned failures are checked with fake-secret canaries. A same-instance hot-reload test is required. Incident counts and scope diagnosis are correctly separated from acceptance evidence. |

These are resolutions **in the specification**, not claims that code or tests implementing them already exist.

## Exposure trace

The production enqueue paths found on the reviewed revision are:

- `internal/graph/handlers/ingest_content.go:320–354`: caller-provided file metadata, including `url_private`, becomes an attachment node and download payload.
- `internal/graph/handlers/backfill_slack.go:352–380`: Slack message file metadata becomes a describe payload. Note that attachment-node insertion errors are logged but do not stop this path; “always already in the graph” must not be resurrected as a security invariant.
- `internal/graph/handlers/fetch_body.go:266–289`: fetched attachment metadata is enqueued following successful attachment-node upsert, subject to the existing depth/skip limits.
- `internal/graph/handlers/describe_attachment.go:428–494, 505–524`: explicit repair selects existing attachment nodes and calls `enqueueDescribeAttachment`.

The first three use `enqueueDescribeIfNeeded`, whose actual enqueue is at `describe_attachment.go:89`. Repair has the second direct enqueue at `:518`.

R2's destination boundary, rather than provenance inferred from node existence, is the relevant protection. It does **not** ensure a requested Slack file belonged to an authentic ingested Slack message. The plan explicitly records that an API-key holder can request descriptions of Slack files readable by Enzo; that accepted risk is not resolved by the host allowlist and must remain in the implementation report. Anonymous requests to non-allowlisted URLs also remain possible by design; this is credential containment, not a general SSRF fix.

## Handoff notes / minor specification rough edges

No additional blocking finding survived review. The following details should be retained during implementation; they do not require another design decision or user clarification:

1. **Use a synchronized live snapshot.** `Config.Snapshot()` takes the read lock (`internal/config/config.go:135–139`), while `Update()` takes the write lock (`:290–292`). The getter should obtain both credentials from one fresh snapshot, not read exported fields directly or capture a startup snapshot. Add the getter to `Deps` and wire it in `internal/worker/server.go`; these wiring edits are necessarily included by R2.2 even though the original “Files expected to change” list omitted them.
2. **Preserve other sources' redirect behavior and bound Slack redirects.** The new `CheckRedirect` belongs only to the Slack branch of the shared downloader (`describe_attachment.go:304–344`). Go's custom callback replaces its default ten-redirect guard, so retain a redirect-count guard as well as the destination check. Otherwise a same-host loop runs until the existing 60-second timeout rather than stopping after ten redirects. The timeout makes this bounded already, so this is a small implementation safeguard rather than a blocking security finding. Exercise both a permitted same-host redirect and a rejected cross-host redirect; the latter destination should receive no request, not merely a request with stripped credentials.
3. **Persist clears as empty values, not omitted map entries.** `internal/database/settings.go:40–46` only upserts keys present in the map; omission does not remove a stored secret. `RuntimeSettings()` should include each credential key with an empty value after an explicit clear, and startup loading must accept that empty value. R2.5's reload test already requires this result. If using `clear_settings`, restrict it to these two supported keys and define deterministic handling when a request also supplies a replacement for the same key.
4. **One diagnostic template omits bot 401.** R2.4's `bot 403|login, user login` example should also permit `bot 401`. Preserve the actual first-attempt status in the error instead of reporting 403 for a 401 response. The required fatal classification is otherwise unambiguous.
5. **The implementation intentionally differs from p-agent.** The reference falls back after any thrown GET error and requires both owner values. This plan deliberately restricts fallback to 401/403 or login HTML and permits bearer-only operation. Its explicit download rules take precedence over “copy” in the reference section. Do not import p-agent's broader retry behavior or require the cookie for an xoxp token.

The available code establishes that the revised approach is implementable. It cannot establish that Enzo's current credentials work, that `files.slack.com` will not redirect a particular production file elsewhere, or that the incident's missing-scope explanation is correct. The plan appropriately reserves those observations for the human-gated canary. A refused redirect must stay a failure, not become a reason to expand the credential allowlist during implementation.

## Verdict

All six R1 findings are addressed at plan level. The residual access risk is explicit, the runtime wiring is feasible, and the required tests cover the previously missing behavioral and secret-handling guarantees. The minor notes above do not prevent a context-free implementer from proceeding.

APPROVED
