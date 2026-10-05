# Slack file user-session fallback implementation report

## Scope and decisions

Implemented the approved plan with Revisions 2 and 3 overriding the original sections. Incident counts (2,241 failures / zero successes) and the missing bot scope are conductor-supplied context, not independently verified production evidence. No live secrets, live settings rows, env files, hub access, or dev database on port 5433 were used. No PR, merge, or deploy is part of this change.

Credentials are confined to `https://files.slack.com` with an exact URL host match. Other URLs marked `source=slack` are downloaded anonymously and never trigger user fallback. Slack redirects must remain on that same scheme/host and stop at ten redirects; other sources retain their prior redirect policy. This is credential containment, not a general SSRF fix.

The fallback is deliberately narrower than p-agent's `_downloadSlackFile` reference: only bot 401/403 or a 200 HTML login response for a non-HTML job triggers one retry. Network failures, 429, 5xx, and refused redirects do not trigger fallback. A cookie is optional. MIME detection uses parsed media types, including `text/html; charset=utf-8`. Real HTML attachments remain valid downloads. Login pages without usable fallback are fatal; user 429/5xx remain retryable. Diagnostic text preserves the actual bot status.

The live getter is `Deps.SlackUserCreds func() (token, cookie string)`, wired in `internal/worker/server.go` to one fresh, synchronized `Config.Snapshot()`. Saving settings updates existing registered handlers without restarting the worker. No new Slack API calls or automated requeue were introduced.

## Settings and operator instructions

The gateway API key was already masked in the settings API (`internal/worker/settings_handlers.go:51-59,70`, `maskKey`); copying cleartext exposure was unnecessary. The two new keys use that existing masking path for both GET and PUT responses: nonempty values are asterisk-masked, retaining at most the last four characters. The dashboard displays only **Set / Not set**, never a stored value, and uses password inputs for new values.

In **Settings → Slack attachment fallback**:

- **User token** (`slack_user_token`): paste the `xoxc-...` session token, or an `xoxp-...` user token; click **Save**.
- **Session cookie (optional)** (`slack_user_cookie`): paste only the `d` cookie value, typically `xoxd-...`, not the `d=` prefix or a complete Cookie header; click **Save**. An `xoxp` bearer token needs no cookie.
- Each field has a separate **Clear** button. Clearing the token disables fallback. Clearing only the cookie retains bearer-only operation.

The API accepts `clear_settings: ["slack_user_token", "slack_user_cookie"]`, restricted to these two keys. Omitted/blank credential saves preserve stored values; clear wins if a request both sets and clears the same key. Runtime settings always include both keys, even as empty strings, so clears survive upsert-only persistence and `ApplyDBSettings` reload.

## Exposure trace

Every production enqueue path was traced:

| Path | Evidence | Provenance / limitation |
| --- | --- | --- |
| Ingest content | `internal/graph/handlers/ingest_content.go:89-95,320-354` | Caller-provided `metadata.files[].url_private`; upserts the attachment node before enqueue, but that does not prove Slack supplied the URL. |
| Slack backfill | `internal/graph/handlers/backfill_slack.go:352-380` | Slack message file metadata; insertion failures are logged without preventing the enqueue attempt, so node existence is not an absolute invariant. |
| Fetch body | `internal/graph/handlers/fetch_body.go:251-289` | Fetched attachment metadata after successful attachment-node upsert; depth and skip limits apply. |
| Explicit repair | `internal/graph/handlers/describe_attachment.go:499-564,575-594` | `BackfillFailedAttachments` selects existing Slack/Jira attachment nodes and calls `enqueueDescribeAttachment`; no startup requeue was added. |

The first three use `enqueueDescribeIfNeeded`, which directly enqueues `describe_attachment` at `internal/graph/handlers/describe_attachment.go:90`. Explicit repair contains the only other direct production enqueue, at `:588`.

The original STOP condition is known and superseded by Revision 2: an API-key holder can request a description of **any Slack file Enzo can see**, into the graph. Enzo accepted that residual authorization risk. Graph membership is not the security boundary; exact HTTPS host containment is. Anonymous downloads of other destinations remain possible by design.

## Red proof (before production changes)

The new tests ran against the original downloader and failed for the intended missing behavior:

```text
$ go test ./internal/graph/handlers/ -run 'TestDownloadSlackBot(403|HTML)UserSucceeds$' -count=1 -v
=== RUN   TestDownloadSlackBot403UserSucceeds
    describe_attachment_fallback_test.go:64: fallback download failed: fatal: download HTTP 403: https://files.slack.com/files-pri/test.png
--- FAIL: TestDownloadSlackBot403UserSucceeds (0.01s)
=== RUN   TestDownloadSlackBotHTMLUserSucceeds
    describe_attachment_fallback_test.go:68: download = "login", want user image bytes
--- FAIL: TestDownloadSlackBotHTMLUserSucceeds (0.00s)
FAIL
FAIL github.com/agent-mem/agent-mem/internal/graph/handlers 0.675s
```

Settings tests also demonstrated the missing persistence before implementation:

```text
=== RUN   TestSlackUserCredentialsRoundTrip
    config_test.go:58: slack_user_token was not saved
--- FAIL: TestSlackUserCredentialsRoundTrip (0.00s)
=== RUN   TestSlackUserSettingsSaveMaskPreserveAndClear
    settings_handlers_test.go:137: credentials did not survive persisted-map reload
--- FAIL: TestSlackUserSettingsSaveMaskPreserveAndClear (0.04s)
```

## Verification environment

Created a throwaway `pgvector/pgvector:pg16` container named `agent-mem-rp86-scratch`, published only on loopback with dynamically allocated port **62633**, database **agentmem_test**. Validated host `127.0.0.1`, port `62633 != 5433`, and database name before migration/testing. Both `DATABASE_URL` and `AGENT_MEM_TEST_DATABASE_URL` point only to that target. All credentials used by regression tests are fake, including `xoxc-FAKE-LEAK-CANARY` and `xoxd-FAKE-LEAK-CANARY`.

Migration output:

```text
INF Running migrations dir=./migrations
INF Migrations applied
```

Dashboard build and embed synchronization:

```text
$ npm run build
> dashboard@0.0.0 build
> tsc -b && vite build
vite v8.0.1 building client environment for production...
transforming...✓ 1192 modules transformed.
dist/index.html                     0.45 kB │ gzip:   0.29 kB
dist/assets/index-CY22GbDN.css     36.17 kB │ gzip:   7.29 kB
dist/assets/index-D-SKE5Nj.js   2,352.89 kB │ gzip: 668.72 kB
✓ built in 389ms
$ rsync -a --delete dashboard/dist/ internal/worker/dashboard/
(exit 0)
```

The build exited 0 with Node's `module.register()` deprecation warning and Vite's large-chunk warning. Locked dependency install exited 0 and warned about an unapproved optional `fsevents` install script; no script approval or dependency changes were made. Generated embedded assets are included in this branch.

## Passing Go verification

`go build ./...` and `go vet ./...` both exited 0 with no output. The first integrated build exposed incorrect two-value assignments for `mime.ParseMediaType`; these were corrected to its three-value signature before the passing runs. Verification commands used an environment allowlist (`env -i`, PATH/HOME/TMPDIR and the scratch DB variables), avoiding inherited service credentials.

Focused command:

```text
$ go test ./internal/graph/handlers/ -run 'DescribeAttachment|Download' -count=1 -v
--- PASS: TestDownloadSlackFallbackStatuses (0.12s)
    --- PASS: TestDownloadSlackFallbackStatuses/real_bytes_skip_fallback (0.01s)
    --- PASS: TestDownloadSlackFallbackStatuses/real_HTML_skip_fallback (0.00s)
    --- PASS: TestDownloadSlackFallbackStatuses/bot401_user_succeeds (0.01s)
    --- PASS: TestDownloadSlackFallbackStatuses/no_token_preserves_bot403 (0.00s)
    --- PASS: TestDownloadSlackFallbackStatuses/no_token_bot_login_fatal (0.00s)
    --- PASS: TestDownloadSlackFallbackStatuses/both403 (0.01s)
    --- PASS: TestDownloadSlackFallbackStatuses/bot401_user403_preserves_status (0.01s)
    --- PASS: TestDownloadSlackFallbackStatuses/user404 (0.01s)
    --- PASS: TestDownloadSlackFallbackStatuses/user503_transient (0.01s)
    --- PASS: TestDownloadSlackFallbackStatuses/user429_transient (0.01s)
    --- PASS: TestDownloadSlackFallbackStatuses/bot503_no_fallback (0.00s)
    --- PASS: TestDownloadSlackFallbackStatuses/bot429_no_fallback (0.00s)
    --- PASS: TestDownloadSlackFallbackStatuses/bot404_no_fallback (0.00s)
    --- PASS: TestDownloadSlackFallbackStatuses/bearer_only (0.01s)
    --- PASS: TestDownloadSlackFallbackStatuses/bot403_user_login (0.02s)
    --- PASS: TestDownloadSlackFallbackStatuses/bot401_user_login (0.01s)
    --- PASS: TestDownloadSlackFallbackStatuses/both_login (0.01s)
--- PASS: TestDownloadSlackCredentialAllowlist (0.01s)
--- PASS: TestDownloadNonSlackNeverUsesUserCredentials (0.00s)
--- PASS: TestDownloadSlackRedirects (0.02s)
--- PASS: TestDownloadSlackRedirectLimit (0.00s)
--- PASS: TestDownloadSlackNetworkErrorNoFallback (0.00s)
--- PASS: TestDownloadSlackFreshCredentials (0.00s)
--- PASS: TestDownloadSlackFailedUserRequestNoFurtherFallback (0.00s)
--- PASS: TestDownloadNonSlackPreservesTransportAndBodyErrors (0.00s)
--- PASS: TestDownloadNonSlackKeepsCrossHostRedirectBehavior (0.00s)
--- PASS: TestDownloadSlackUserRedirectLimit (0.02s)
--- PASS: TestDownloadSlackBot403UserSucceeds (0.01s)
--- PASS: TestDownloadSlackBotHTMLUserSucceeds (0.00s)
--- PASS: TestDescribeAttachmentHandler_BadPayload (0.00s)
--- PASS: TestDescribeAttachmentHandler_EmptyURL (0.00s)
--- SKIP: TestDescribeAttachmentHandler_UnsupportedMime (0.00s)
--- PASS: TestDescribeAttachment_RichPDF_SkipsGemini (0.42s)
--- PASS: TestDescribeAttachment_ThinPDF_UsesGeminiOnScreenshots (0.47s)
--- PASS: TestDescribeAttachment_LiteParseUnavailable_FallsBack (0.00s)
--- PASS: TestDescribeAttachment_Download403NotRetryable (0.00s)
--- PASS: TestDescribeAttachment_Download503Retryable (0.00s)
--- PASS: TestDescribeAttachment_DownloadNetworkErrorRetryable (0.00s)
--- PASS: TestDescribeAttachment_NonResultNotPersisted (0.03s)
--- PASS: TestFetchBodyCascade_DepthOneSkipsDescribeAttachment (0.04s)
PASS
ok github.com/agent-mem/agent-mem/internal/graph/handlers 1.830s
```

Worker/config command (all tests passed, excerpt includes all new tests):

```text
$ go test ./internal/worker/... ./internal/config/... -count=1 -v
--- PASS: TestSlackUserSettingsSaveMaskPreserveAndClear (0.06s)
--- PASS: TestSlackUserSettingsSameDownloadHandlerHotReload (0.05s)
PASS
ok github.com/agent-mem/agent-mem/internal/worker 1.785s
--- PASS: TestSlackUserCredentialsRoundTrip (0.00s)
PASS
ok github.com/agent-mem/agent-mem/internal/config 0.442s
```

The tests cover plan cases 1–11, including actual HTTP Authorization/Cookie inspection, login-page MIME parameters, no-cookie bearer operation, no user credentials on Jira, real first-status diagnostics, and fatal versus retryable outcomes. Distinctive fake credential assertions check GET and PUT bodies, captured settings/download logs, and failing download errors (the errors used for job `last_error`). The same exported attachment handler instance downloads with newly saved credentials, switches to bearer-only after cookie clear, and stops fallback after token clear; no restart or reconstruction occurs.

The test seam replaces `http.DefaultTransport` temporarily with a custom RoundTripper that maps the original HTTPS URL to a local `httptest` TLS server while retaining the original Host. The production allowlist is never relaxed. Foreign redirect tests assert the foreign server receives **zero requests**, not merely stripped headers.

## Full scratch database suite

```text
$ make test-db TEST_DATABASE_URL=<validated scratch DSN>
DATABASE_URL=<scratch> AGENT_MEM_TEST_DATABASE_URL=<scratch> AGENT_MEM_EVAL= go test -p=1 -count=1 -json ./internal/graph/...
ok github.com/agent-mem/agent-mem/internal/graph           0.601s
ok github.com/agent-mem/agent-mem/internal/graph/acl       0.573s
ok github.com/agent-mem/agent-mem/internal/graph/bfs       0.580s
ok github.com/agent-mem/agent-mem/internal/graph/entities  0.586s
ok github.com/agent-mem/agent-mem/internal/graph/extractor 0.639s
ok github.com/agent-mem/agent-mem/internal/graph/fetchers  0.932s
ok github.com/agent-mem/agent-mem/internal/graph/handlers 46.150s
ok github.com/agent-mem/agent-mem/internal/graph/hydrate   0.714s
ok github.com/agent-mem/agent-mem/internal/graph/identity  0.631s
ok github.com/agent-mem/agent-mem/internal/graph/ids       0.446s
ok github.com/agent-mem/agent-mem/internal/graph/jobs     52.027s
ok github.com/agent-mem/agent-mem/internal/graph/normalizer 0.654s
ok github.com/agent-mem/agent-mem/internal/graph/scoring   0.568s
ok github.com/agent-mem/agent-mem/internal/graph/temporal  0.469s
```

The JSON stream reported **14 passing packages, 1,593 passing test/subtest events, zero failures**, and exactly these three documented skips:

```text
TestDescribeAttachmentHandler_UnsupportedMime:
  requires network; covered by integration tests
TestParseDocument_RichPDF:
  lit binary not in PATH; skipping real PDF test
TestTopicJudgeGolden:
  set AGENT_MEM_EVAL=1 to run the topic-judge eval (needs real DB + API key)
```

## Actual embedded UI smoke and review

Launched the built Go worker at `http://127.0.0.1:34686` against the scratch DB with an environment allowlist, `AGENT_MEM_GRAPH_RUNNER=none`, sync disabled, no gateway configured, and a temporary data directory. The worker reported jobs disabled and all LLM work disabled. Browser access was restricted to `127.0.0.1`.

Observed and exercised the embedded dashboard, not a mocked page:

- Both inputs are password fields; initial empty Save and Clear buttons are disabled.
- Saved the fake token and cookie through the actual controls. Both statuses became **Set**, inputs emptied only after successful saves, and real GET response bodies contained neither fake credential.
- Visually inspected the Slack fallback section in a browser screenshot: Set statuses, empty write-only inputs, Save/Clear buttons, and operator hints rendered correctly.
- Cleared only the cookie: real GET showed an empty cookie and the token still set.
- Cleared the token: real GET showed both empty; fallback disabled.
- Reloaded the page, reopened Settings, and verified empty input/disabled Clear remained.

Observed browser assertions:

```text
{ maskedResponse: true, writeOnlyInputCleared: true }
{ tokenSet: true, cookieSet: true, responsesMasked: true }
{ cookieCleared: true, tokenPreserved: true }
{ tokenCleared: true, fallbackDisabled: true }
{ reloadPreservedClear: true }
```

Two independent read-only reviews covered download security/semantics and settings/config/server/UI integration. Both found no actionable defects or specification gaps. Reviewers did not run tests or access live systems; the execution evidence above is from the parent session.

The browser tab and scratch worker were closed; the throwaway container, smoke executable, and temporary data directory were removed. No verification scaffolding remains in the branch.

## Remaining operational boundary

No real Slack credential or production file was used. Whether Enzo's current session can fetch particular production files, or those files redirect outside the allowlist, is intentionally unverified. The conductor's approved PR/review/merge/deploy, credential entry, ten-job canary, and separately approved rate-limited requeue remain human-gated operations outside this implementation. A refused redirect must remain a failure; do not widen the host allowlist to make a production canary pass.

## Round 2: off-host redirect

Implemented the redirect-refusal plan with Revisions 2 and 3 taking precedence.
The exact HTTPS `files.slack.com` allowlist is unchanged. Only a redirect stopped
by the Slack policy sets the per-attempt refusal flag; the flag resets before
each HTTP attempt and the bot result is retained separately. A bot refusal can
trigger one user-session attempt, but only for an originally allowed URL.
Initially disallowed URLs remain anonymous, never fall back, and policy-stopped
redirects are fatal (`slack redirect (no auth)`).

All final Slack 3xx response bodies are closed without being read. A final 302
without Location or a 304 is a fatal numeric HTTP result, not a refusal.
The ten-redirect cap is checked before the target policy, remains transient,
and never triggers fallback. Network errors stay redacted and transient.
Mixed diagnostics preserve bot/user reasons; user 429/5xx remain transient.
Non-Slack redirect and body handling are unchanged.

### Red / green evidence

The production downloader was still the pinned baseline
`09786c96ff8790b653f4e4216d4eb5c93616ac88` when the new regression ran:

```text
$ env -u DATABASE_URL -u AGENT_MEM_TEST_DATABASE_URL AGENT_MEM_EVAL= \
    go test ./internal/graph/handlers/ \
    -run '^TestDownloadSlackOffHostRedirectFallsBack$' -count=1 -v
=== RUN   TestDownloadSlackOffHostRedirectFallsBack
    describe_attachment_download_security_test.go:428: error = http get: download request failed, bytes = "", requests = 1, foreign = 0
--- FAIL: TestDownloadSlackOffHostRedirectFallsBack (0.00s)
FAIL
FAIL github.com/agent-mem/agent-mem/internal/graph/handlers 0.664s
exit 1
```

Created throwaway container `agent-mem-rp86-redirect-scratch` from
`pgvector/pgvector:pg16`, published only at `127.0.0.1:53402`, database
`agentmem_test`. Checked its binding, database readiness and `53402 != 5433`
before migration. Migration, focused tests, smoke and the full suite use an
environment allowlist with both `DATABASE_URL` and
`AGENT_MEM_TEST_DATABASE_URL` explicitly set to that scratch DSN and
`AGENT_MEM_EVAL=`. No inherited secret values were printed or inspected.

```text
$ go run ./cmd/agent-mem migrate
INF Running migrations dir=./migrations
INF Migrations applied
exit 0
$ go build ./...
exit 0 (no output)
$ go vet ./...
exit 0 (no output)
$ go test ./internal/graph/handlers/ -run 'Download|DescribeAttachment' -count=1 -v
--- PASS: TestDownloadSlackFallbackStatuses (0.12s)
--- PASS: TestDownloadSlackRedirects (0.01s)
--- PASS: TestDownloadSlackOffHostRedirectFallsBack (0.00s)
--- PASS: TestDownloadSlackFinalRedirectBodies (0.00s)
--- PASS: TestDownloadSlackRedirectCapBeforeRefusal (0.00s)
--- SKIP: TestDescribeAttachmentHandler_UnsupportedMime (0.00s)
PASS
ok github.com/agent-mem/agent-mem/internal/graph/handlers 1.626s
exit 0
```

The serial local HTTP cases inspect bot/user credentials on actual attempts and
prove zero foreign requests, same-host redirect support, fatal refusal without
a user token, both-attempt refusal, and downgrade/suffix/explicit-port containment.
The instrumented RoundTripper cases prove zero reads and exactly one close of
each final Slack 3xx body, closure before fallback, anonymous redirect refusal,
numeric 302/304 classification, bot-redirect/user-302 flag reset, and transient
user network failure after a bot refusal. Fake credential leak canaries remain
absent from errors and captured logs.

A separate throwaway local HTTP smoke exercised the downloader and was removed:

```text
=== RUN   TestRedirectRoundTwoSmoke
SMOKE: bot redirect -> user bytes; files requests=2; foreign requests=0
--- PASS: TestRedirectRoundTwoSmoke (0.00s)
PASS
ok github.com/agent-mem/agent-mem/internal/graph/handlers 0.523s
exit 0
```

### Full scratch suite and boundaries

`make test-db TEST_DATABASE_URL=<validated scratch DSN>` exited 0:
14 packages passed, no failure events. Actual skips:

```text
TestDescribeAttachmentHandler_UnsupportedMime:
  requires network; covered by integration tests
TestParseDocument_RichPDF:
  lit binary not in PATH; skipping real PDF test
TestTopicJudgeGolden:
  set AGENT_MEM_EVAL=1 to run the topic-judge eval (needs real DB + API key)
```

No live secrets, settings rows or env files were read; no hub or dev DB on 5433
was touched. No dashboard/settings changes, PR, merge, deploy or requeue.
Production curl/canary observations remain conductor context, not worker
acceptance evidence; real-session production validation remains outside scope.
The throwaway smoke file and scratch container were removed.

Beads tracking could not be created: `bd create` returned
`database not initialized: issue_prefix config is missing`.
No Beads database initialization or shared tracker repair was attempted.
