# Plan: fenced-JSON P0 + non-terminal parse failures

Two repos, two independent bugs, both found by unpausing production for ten
minutes on 2026-08-01. Part A unblocks everything. Part B is what stops the next
bad-output bug from destroying data.

- `~/go/src/github.com/llm-gateway` — branch off `main`
- `~/go/src/github.com/agent-mem` — branch off `main` @ `6467a85`

---

## Evidence

Ten minutes of live processing, then re-paused:

```
pending_messages   failed 8,663 → 8,681  (+18)
                   completed 88,628 → 88,628  (+0)
```

Zero successes. Every message that ran was permanently destroyed. Worker log,
repeated for every job type:

```
Failed to process message  error="parse gemini response: parse observation:
  invalid character '`' looking for beginning of value"
describe_attachment Gemini.Describe: llm-gateway: describe: parse JSON:
  invalid character '`' ...
transient: link_topics confirm: invalid JSON
```

Reproduced directly against the live gateway:

```
POST /generate {"system":"Reply with JSON only.","user":"Return {\"ok\":true}","tier":"cheap"}
→ {"backend":"claude","text":"```json\n{\n  \"ok\": true\n}\n```", ...}
```

---

## Part A — llm-gateway: unwrap fenced responses (P0)

### Why here and not in agent-mem

The OpenRouter path sends `response_format: {"type": "json_object"}`, which
guarantees a bare JSON body. Every agent-mem parser was written against that
contract. The Claude Agent SDK only guarantees it when `output_format` carries a
schema, and agent-mem passes none — so the model wraps its answer in a markdown
fence like it would in chat.

The gateway is the thing claiming to be a drop-in for the OpenRouter path, so the
gateway is where the contract gets restored. Patching agent-mem's call sites
instead means fixing it four times and leaving the next client of this gateway
(Claude-Code-Remote shares it) to rediscover the same bug.

### Change

`app/claude.py`, in `_run()`, the `if structured is None:` branch — the one that
returns `{"text": joined, "meta": meta}`. Unwrap before returning.

Unwrap **only when the entire response is a single fenced block**:

- after `.strip()`, text starts with ``` and ends with ```
- drop the opening fence line (which may carry an info string: ```json, ```JSON,
  or bare ```) and the closing fence
- return the inner content, stripped

If those conditions do not all hold, return the text untouched. Do **not**
regex out every backtick, and do **not** try to detect JSON — a response that
legitimately contains a fenced snippet inside prose must survive intact. A
half-stripped body is worse than a fenced one because it fails further from the
cause.

Put the helper at module scope with a name that says what it does
(`_unwrap_fenced_block`) so the describe path and any future caller share it,
and comment *why* it exists — the OpenRouter `json_object` contract — or someone
will delete it as defensive noise.

### Tests (`test_gateway.py`)

Table-driven over the helper, no network:

- ```` ```json\n{"ok":true}\n``` ```` → `{"ok":true}`
- ```` ```\n{"ok":true}\n``` ```` → `{"ok":true}`
- `{"ok":true}` → unchanged
- prose containing a fenced block but not wholly fenced → unchanged
- text with a leading fence and no closing fence → unchanged
- inner content that is not JSON at all → returned as-is, no exception

---

## Part B — agent-mem: a parse failure must not destroy the message (P1)

### Why

`processor.go:87` calls `MarkMessageFailed` for any error that
`llmgateway.IsRetryable` does not recognise. That status is terminal —
`ClaimPendingMessage` only ever selects `'pending'` — so a malformed LLM
response discards the message forever. That is what turned bug A from an outage
into data loss. Part A stops today's cause; Part B stops the class.

`pending_messages` has no retry budget today (columns: `payload, created_at,
processed_at, id, error, content_session_id, message_type, status`), so
"just requeue instead" would loop forever on a genuinely poisonous payload.

### Change

**1. Migration** `migrations/20260801000005_pending_messages_attempts.sql`:

```sql
ALTER TABLE pending_messages ADD COLUMN attempts INT NOT NULL DEFAULT 0;
```

Down: drop the column. Follow the commenting style of the recent migrations in
that directory — say why the column exists, not what the DDL does.

**2. `internal/database/pending.go`**

- `ClaimPendingMessage` — increment `attempts` as part of the claim, in the same
  statement that flips status to `'processing'`. Doing it in a second query
  means a crash between the two resets the budget and the poison payload runs
  forever.
- `RequeuePendingMessage` — leave as is; it must **not** reset `attempts`.
- Update the `MarkMessageFailed` doc comment: still terminal, now reached only
  after the budget is spent.

**3. `internal/worker/processor.go`** — replace the bare `MarkMessageFailed`
branch at :86-90 with a budget check:

```go
const maxMessageAttempts = 3
```

- `llmgateway.IsRetryable(err)` → requeue and back off, unchanged, and this path
  must stay **outside** the budget: a gateway outage is not the message's fault
  and must not consume its attempts.
- otherwise, if `msg.Attempts < maxMessageAttempts` → `RequeuePendingMessage`,
  log at warn with the attempt count
- otherwise → `MarkMessageFailed`, log at error including the attempt count so
  the log says *why* it gave up

Add `Attempts` to the `PendingMessage` struct and the claim's row scan.

### Tests

`internal/database` and/or `internal/worker`, whichever the existing tests in
those packages fit — do not invent a new harness:

- a message failing with a non-retryable error is requeued to `'pending'` and
  its `attempts` has increased
- the same message on its 3rd failure lands in `'failed'`
- a retryable (`ErrUnreachable`) failure requeues **without** spending an attempt

Note: `httptest` cannot bind ports under the codex/omp sandbox. If you hit
`bind: operation not permitted`, ask for approval to run the test outside the
sandbox. Do **not** delete, skip, or rewrite the test to avoid the bind —
that has been attempted before in this repo and is not acceptable.

---

## Verify

```bash
cd ~/go/src/github.com/llm-gateway
python3 -c "import ast;[ast.parse(open(f).read()) for f in ['app/main.py','app/claude.py','app/config.py','app/openrouter.py']]"
python3 -m pytest test_gateway.py -q

cd ~/go/src/github.com/agent-mem
unset DATABASE_URL          # integration tests TRUNCATE the DB if this is set
go build ./... && go vet ./... && go test ./...
#   4 failures in internal/hooks + internal/skills are PRE-EXISTING on main
```

No dashboard change in this task, so no bundle rebuild.

## Rules

- **Do not deploy either repo.** Commit and push branches; the lead reviews,
  merges, deploys and restarts the gateway.
- **Do not touch the VPS** — not `.env`, not the database, not systemd.
- **Do not unpause.** `processing_paused` is `true` and stays `true`. It was
  briefly false today and that is exactly how the data was lost.
- **Do not requeue the existing failed rows.** 8,681 rows are sitting in
  `'failed'`; recovering them is a separate decision the lead makes after these
  fixes are live, because a bulk requeue is a bulk LLM spend.
- Do not change any gateway model, backend, tier or config value.
