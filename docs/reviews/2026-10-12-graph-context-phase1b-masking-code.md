# Code review: graph context Phase 1b masking

Branch `feat/graph-context-phase1b-masking`, HEAD `3afda7a`, against merge-base `e2d7c40` (origin/main). Reviewed with deep-review, Round 1, read-only.

## Verdict

**REQUEST_CHANGES.** There is one HIGH finding. The wiring is sound: masking sits inside the trace wrapper, the neighbors child router is covered, panics fail closed, buffered headers drop Content-Length, Content-Encoding and ETag, and nothing streams or flushes. But the URL protection span swallows PII placed next to a URL, and that pattern is common in Slack and Jira text.

## Findings

### F1 HIGH (Verified): the URL protection span shields phone and card numbers that follow a URL

`internal/redact/redact.go:38` (`reURL = https?://\S+`), applied at `:114-118`.

`\S+` runs to the next whitespace. Every character in that run, including `|`, `"`, `,`, `>` and `]`, becomes a protected span. Step 3 then skips any phone or card candidate inside it. I ran the shipped `Mask` on these inputs:

| Input | Output |
|---|---|
| `<https://wego.slack.com/x\|+6591234567>` (Slack mrkdwn link) | unchanged |
| `<https://pay.x/p\|4111111111111111>` | unchanged |
| `[https://jira.x/PAY-1\|4111 1111 1111 1111]` (Jira wiki link) | `4111` is shielded. The remaining 12 digits are neither Luhn-valid nor labelled, so they also pass. |
| `{"url":"https://api.x.com/v1","phone":"+6591234567","card":"4111111111111111"}` (a pasted payload with no spaces) | phone and card unchanged; only the email is masked |
| `see https://jira.x/PAY-1,+6591234567` | unchanged |

**Failure scenario:** a Slack thread body stores `<url|label>` links verbatim, or a Jira or Datadog body holds a pasted request/response JSON. A non-admin `/resolve?include_bodies` or `/node` call then returns the + phone or the PAN unmasked. That breaks the security goal of no + phone and no Luhn-valid card in any field. The leak goes into agent and LLM context, so it cannot be recalled.

**Fix:** end the URL span at characters that cannot belong to a URL. The simplest option reuses the node-id exclusion class, for example ``https?://[^\s"'`<>\[\]{}|,;]+``. Optionally trim trailing `.`, `)` and `:`. Add the five rows above to `redact_test.go` as must-mask cases.

The plan does not settle whether PII inside a real URL should stay readable, for example `https://wa.me/6591234567` or `api.whatsapp.com/send?phone=…`. It only covers emails inside URLs. The author should decide that explicitly.

## Checked and dropped (no finding)

- **Wiring:** `withContextTrace(maskGraphResponse(...))` on search and resolve means traces hash what the client received (`router.go:92,95`). Node and epic are wrapped. Neighbors is wrapped inside its own chi router (`neighbors.go:60`). HEAD is not routed (chi without `GetHead` returns 405). No handler uses Flusher, Hijacker or ResponseController.
- **Panic:** `serveRecovering` throws away the partial buffer and returns 500 `mask_failed`. Invalid JSON, trailing data and a 2xx non-JSON body all fail closed. The approved status >= 400 plain-text rule is implemented at `mask.go:46-48`.
- **Map keys are not walked:** the response maps on these routes are keyed by type, via or arm names, never by user text (`epic.go:40,43`, `search.go:97`).
- **Backtracking:** everything is RE2, and `matchAt` stops at 19 digits. A 600 KB adversarial string took about 0.5 s.
- **Luhn** is correct. Phone takes precedence over card. Card shape rejects `(` and `.`.
- **Approved precedences hold:** `order id: 4111111111111111`, `Order #: 4111-1111-1111-1111` and `status:4111…` stay readable because the plan protects labelled booking values and node-id prefixes. This is residual risk to note, not a defect.

## Residual risk (not blocking)

- Arabic-Indic and full-width digits are not candidates, so `+٩٦٦٥٠١٢٣٤٥٦٧` passes unchanged. This is in the MEA/PDPL context; consider normalizing digits.
- Percent-encoded emails (`user%40wego.com`) pass. This is out of the plan's scope.

## Test adequacy

The route tests go through `mountedGraph` with a real recorder, so the trace-wrapper order is exercised. They cover all five routes plus hybrid search and cards, for both asker and admin. The redact table has no case with a URL next to PII, which is how F1 got through.

## Receipts

- graph: degraded: code-review-graph not queried; manual rg tracing of every changed symbol and its callers (`maskGraphResponse` has 5 mounts, `redact.Mask` has 2 callers, `rePaymentRef`/`reActionRef` are aliased in identifiers.go)
- fan_out: inline: user declined fan-out (caller asked for a single bounded pass)
- Probes: a copy of `internal/redact` and `internal/graph/ids` in scratch, running the shipped `Mask` verbatim
- `go vet` on the four touched packages is clean. `go test ./internal/redact ./internal/graph/ids` passes. `make test-db` passes: exit 0, 16 packages, 0 failures, no DB skips.
