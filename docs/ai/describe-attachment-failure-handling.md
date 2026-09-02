# Plan — agent-mem-6b5 + agent-mem-16e: stop persisting attachment failures as knowledge

**Issues:** `agent-mem-6b5` (P1), `agent-mem-16e` (P1) · **Repo:** `~/go/src/github.com/agent-mem`
**Branch off:** `main` (NOT off `feat/agent-mem-32j-attachment-ocr-notify`)

You have no memory of the investigation that produced this. Every claim below was verified against
the tree and against the production database. File:line references are real.

---

## Evidence

Production row for the Slack screenshot that started this (`slack_file:F0BP8ULST4L`):

```
SELECT body_full FROM graph.artifact_bodies WHERE node_id='slack_file:F0BP8ULST4L';

 Image processing failed - unable to process the attachment due to technical limitations
 
 OCR:
 No text could be extracted
```

That is a **model-level soft failure stored as a successful description**. `Gemini.Describe`
returned `err == nil` — the gateway call succeeded, the JSON parsed
(`internal/llmgateway/client.go:302-322`), and the model's own text said it could not do the job.
`describe_attachment` then wrote it to `graph.nodes.body`, `graph.artifact_bodies.body_full`, and
embedded it into `graph.artifact_index`, and returned success. **Nothing will ever retry it**,
because the job did not fail.

Scope: **35 of 322** `slack_file:%` rows (11%) match `body_full LIKE 'Image processing failed%'`.
So the download path and credentials are fine in general — 287 succeeded. These 35 are per-image or
per-moment failures that got frozen in place by the missing failure detection.

Separately, production worker logs show dozens of permanently-403 Jira attachment downloads being
retried at `attempt=4` with ~250,000s (~69h) backoff:

```
"transient: describe_attachment download: fatal: download HTTP 403:
 https://wegomushi.atlassian.net/rest/api/3/attachment/content/124194"  attempt=4 delay=249870
```

Note `fatal:` sitting inside a message classified `transient`. That is agent-mem-6b5.

---

## Part A — agent-mem-6b5: restore ErrFatal through the download wrap

`internal/graph/handlers/describe_attachment.go:67`:

```go
return fmt.Errorf("%w: describe_attachment download: %v", jobs.ErrTransient, err)
```

`%w` binds `jobs.ErrTransient`; **`%v` flattens the inner error to a string**, destroying the
`jobs.ErrFatal` that `downloadWithAuth` attaches to any 4xx (`describe_attachment.go:237-239`).
`jobs.IsRetryable` (`internal/graph/jobs/backoff.go:58-75`) checks `errors.Is(err, ErrFatal)` first
and returns false — but the link is gone, so it matches `ErrTransient` and reschedules forever.

**Fix:** make the inner error wrap with `%w` too. Go is 1.25.7 (`go.mod`), so `fmt.Errorf` supports
multiple `%w` verbs:

```go
return fmt.Errorf("%w: describe_attachment download: %w", jobs.ErrTransient, err)
```

`IsRetryable` checks `ErrFatal` **before** `ErrTransient`, so a 4xx correctly becomes non-retryable
while 5xx/429 (which `downloadWithAuth` returns *without* `ErrFatal`) still match `ErrTransient` and
retry. Verify that ordering in `backoff.go` yourself before relying on it.

**Required tests** (`internal/graph/jobs` or handlers, wherever it reads naturally):

- an error produced by this exact wrap around a 403 → `jobs.IsRetryable(...) == false`
- an error produced by this exact wrap around a 503 → `jobs.IsRetryable(...) == true`
- an error produced by this exact wrap around a plain network error → `true`

Construct the inner error the same way `downloadWithAuth` does, so the test breaks if either side
changes. A test that hand-rolls `fmt.Errorf("%w", jobs.ErrFatal)` does not prove the real path.

---

## Part B — agent-mem-16e: never write a non-result

Three changes in `internal/graph/handlers/describe_attachment.go`.

### B1 — validate the bytes before the vision call

Before handing `data` to `Gemini.Describe` for `image/*`, check it actually looks like the declared
type. `http.DetectContentType(data)` (stdlib, no new dependency) sniffs the first 512 bytes.

- empty `data` → error
- declared `image/*` but sniffed `text/html` (or `text/plain`) → error

This deterministically catches the "auth page served with HTTP 200" class, which
`downloadWithAuth` cannot catch because it only rejects status >= 400
(`describe_attachment.go:234-240`) and Slack/Jira serve sign-in HTML with a 200.

Return a **retryable** error here (a plain wrapped error is retryable by `IsRetryable`'s default
rule), and log the sniffed type and the declared mime. `worker.go:86` bounds it — `job.Attempts >=
job.MaxAttempts` fails the job out, so a permanent problem terminates rather than looping.

### B2 — treat a stated non-result as a failure, not a description

After `Gemini.Describe` returns with `err == nil`, decide whether it actually produced anything.
Add a small helper, e.g.:

```go
// isNonResult reports whether a vision call came back with no usable content —
// either empty, or the model explicitly saying it could not process the input.
// Conservative by design: a long, real description that happens to discuss a
// failure must NOT match.
func isNonResult(description, ocr string) bool
```

Rules, in this order:

1. both `description` and `ocr` blank after `TrimSpace` → non-result
2. `description` is **short** (say < 200 chars) **and** contains a known failure marker,
   case-insensitive — e.g. `"image processing failed"`, `"unable to process the attachment"`,
   `"no text could be extracted"`

The length condition is what stops a genuine 2,000-char description of a screenshot *of an error
dialog* from being thrown away. Do not drop it.

On a non-result: **return an error before any write** — no `UPDATE graph.nodes`, no
`artifact_bodies` upsert, no `artifact_index` embedding. Retryable, so a transient model blip
recovers on its own and a permanent one fails out at `MaxAttempts`. Log the node id, the mime, and
the first line of what the model said.

**Required tests** for `isNonResult`, table-driven:

- the **exact** observed production payload
  (`description = "Image processing failed - unable to process the attachment due to technical
  limitations"`, `ocr = "No text could be extracted"`) → `true`
- both empty → `true`
- a realistic long description of a UI screenshot that contains the word "failed" (e.g. describing a
  payment error state) → **`false`**. This is the important one — it is the guard against the
  heuristic eating real data.
- a normal description with normal OCR → `false`

### B3 — a bounded, explicit backfill for the 35 poisoned rows

The existing rows will never re-run on their own. Add a way to re-enqueue `describe_attachment` for
them, following the shape of `BackfillMissingThreadSummaries` (`summarize_thread.go:339` area) —
that is the established precedent in this repo for a backfill query + enqueue loop.

Requirements:

- Selects `graph.artifact_bodies` rows whose `body_full` matches the failure markers, joined to
  `graph.nodes` for the mime/url/source needed by the `describe_attachment` payload. Match the
  payload shape built at `ingest_content.go:340-352`.
- **Explicitly triggered** — an admin endpoint or a job someone enqueues. It must NOT run
  automatically on startup.
- **Hard cap** on rows per invocation (default something small, e.g. 50) and a `log()` of how many
  matched vs how many were enqueued. This repo's standing rule is that bulk LLM runs are capped and
  monitored, and that silent truncation reads as "covered everything" when it did not.
- Deduplicate against an already-queued/running `describe_attachment` for the same node, the way
  `enqueueSummarizeThread` does (`summarize_thread.go:366-388`).

Do **not** run the backfill against production. Shipping the capability is the deliverable.

---

## Non-goals

- Do **not** fix the Jira 403s themselves (`agent-mem-7ku`) — that is credentials/permissions, a
  separate issue. Part A only stops them being retried forever.
- Do **not** change the vision prompt (`describe_attachment.go:52`).
- Do **not** touch `detect_hot_topics.go` — that is `agent-mem-32j`, already done on another branch.
- Do **not** deploy, and do **not** merge either branch.
- Do not rework the job runner, backoff formula, or `MaxAttempts`.

## Files expected to change

| File | Change |
|---|---|
| `internal/graph/handlers/describe_attachment.go` | `%v` → `%w` (A); byte sniff (B1); `isNonResult` + early return (B2); backfill function (B3) |
| `internal/graph/handlers/describe_attachment_test.go` | `isNonResult` table tests, sniff tests |
| `internal/graph/jobs/backoff_test.go` (or handlers) | retryability tests for the real wrap (A) |
| wherever the backfill is triggered from | admin route or job registration (B3) |

No migration.

## Acceptance criteria

1. A 403 through the real `downloadWithAuth` → `describe_attachment.go:67` wrap is **not** retryable.
2. A 503 through the same path **is** retryable.
3. `image/*` bytes that sniff as `text/html` never reach `Gemini.Describe`.
4. The exact observed production non-result payload is detected, and **nothing is written** — assert
   no `artifact_bodies` row appears for that node.
5. A long legitimate description containing the word "failed" is **not** treated as a non-result.
6. The backfill finds rows matching the failure markers, respects its cap, logs matched-vs-enqueued,
   and does not double-enqueue a node that already has a queued job.
7. `go build ./...`, `go vet ./...` clean; new tests pass.

## How to verify

- **NEVER run handler integration tests against the live dev DB** — these tests `DELETE FROM
  graph.nodes` and gate on `DATABASE_URL`, which is the same variable the worker uses. Use the
  scratch database: `DATABASE_URL="postgresql://agentmem:agentmem@localhost:5433/agentmem_test"`.
  (`agent-mem-rtn` tracks fixing that gate; do not fix it here.)
- `isNonResult` and the sniff check are pure functions — unit-test them with no database at all.
- For criterion 4, drive the handler against the scratch DB with a stubbed `Gemini` returning the
  observed payload, then assert the absence of writes.

## Report back

The diff, test output quoted verbatim, and what you left out. If a test fails, say so with the
failure text. Do not hand back a green summary you did not earn.

## Landmines

- `Gemini.Describe` returns `err == nil` on this failure. Do not try to detect it by checking the
  error — there isn't one. The signal is in the returned content.
- `entities` is discarded with `_ = entities` at `describe_attachment.go:123`. Pre-existing, leave it.
- The document branch (`isDocumentMime`) accumulates `description`/`ocrText` across pages and
  already skips pages whose `Describe` errors (`:92-97`). Decide deliberately whether the
  non-result check applies per page or to the combined result, and say which you chose and why. A
  single bad page in a 40-page PDF must not discard the other 39.
