# Plan — agent-mem-32j: notify on screenshot-only reports

**Issue:** `agent-mem-32j` · **Repo:** `~/go/src/github.com/agent-mem` · **Branch off:** `main`

You have no memory of the conversation that produced this. Everything you need is below.
Every file:line reference was verified against the tree at commit `bf0e0e9`.

---

## The incident this fixes

On 2026-08-10 at 08:54:51Z, Khaled Elkalla posted in `#product-issues` (`CDP50BYVD`):

> "Who can update the text of ValU to be Valu. We write the name 3 times and that one is
> inconsistent and incorrect. The U needs to be lowercase. Description under selecting Valu option."

…attached to a **screenshot of the Wego payments form** (`slack_file:F0BP8ULST4L`). The actual
defect — `"Pay with ValU"` in the redirect description while every other label reads `Valu` — is
legible only in the image.

The owner (Enzo, `SlackDMUserID`) received no notification until **09:28:11Z**, 34 minutes later,
when Surbhi Babbar replied. Root cause, confirmed by reading the handlers:

| Notifier | Why it stayed silent |
|---|---|
| `notify_watch_channels` | Only watches the `partners` continent, matched by **name prefix** `ext-wego-` (prod also `wego-tap`). `product-issues` does not match, so the channel is never scanned. `notify_watch_channels.go:18`, `:55-74`, `:162-180` |
| `detect_hot_topics` | Gate is `WHERE g.participants >= $3 OR g.has_important` (`detect_hot_topics.go:217`). Khaled alone = 1 non-bot participant, and he is not in the important set. Surbhi **is** an `importance.json` pin (`score 0.75`), so `bool_or(is_important)` flipped true the moment she replied — that reply is the notification. |

And the image was never a factor either way: the judge is fed `h.Blob`, which is
`string_agg(text, ' ')` over **message text only** (`detect_hot_topics.go:199`, `:213`), truncated
to 2000 chars. The screenshot *is* fully processed — `describe_attachment` runs Gemini vision on
`image/*` with the prompt *"Describe this attachment in detail. Extract any visible text (OCR).
List key entities mentioned."* and writes `description` + `ocr_text` into `graph.artifact_bodies`
(`describe_attachment.go:52`, `:113-117`, `:136-147`). That content is simply never read by the
notification path, and neither DM builder mentions attachments at all.

---

## Goal

1. **Attachment text becomes judgeable evidence.** The hot-topic relevance judge sees a message's
   attachment description + OCR alongside its text, so a screenshot-only bug report can be
   classified on its actual content.
2. **A lone reporter can alert.** `min_participants = 1` becomes a usable, GUI-settable value, so a
   scoped subscription fires on the first message instead of waiting for a pinned person to reply.
3. **The scope is editable without a redeploy.** `min_participants` and `scope_definition` become
   editable in the dashboard GUI, not just at create time / via the sources-refresh job.

The payments-form detection itself is **configuration, not code** — a topic subscription with a
`scope_definition`, created through the GUI after this ships. Do not hardcode payments, ValU, or
`CDP50BYVD` anywhere.

## Non-goals

- No new job, no new handler, no second LLM pass, no image classifier. The existing judge does the
  work; it just gets better input.
- Do **not** touch `notify_watch_channels` or the continent config. Adding `product-issues` to the
  `partners` continent would DM on every message in a general-purpose channel — rejected.
- Do not change `msg_count` or `participants` semantics. Only `blob` gains content.
- Do not fix the dead `max_author_depth` column or the `min_participants` DB-default-4 /
  API-default-2 inconsistency. Both are real, both are out of scope — file follow-up issues instead.
- Do not create the payments subscription yourself. Enzo creates it in the GUI.

---

## Approach

### Part 1 — attachment text into the judge blob

`findHotThreads`, `internal/graph/handlers/detect_hot_topics.go:176-232`. In the `recent` CTE, join
each message node to the bodies of the attachments it references, and append that to `text`.

The join path is verified: ingest writes `graph.edges(from_node_id = <message node>,
to_node_id = 'slack_file:<F…>', kind = 'REFERENCES')` (`ingest_content.go:329-334`; byte-identical
at `backfill_slack.go:354-385`), and `describe_attachment` writes
`graph.artifact_bodies(node_id, description, ocr_text)` keyed on that same attachment node id
(`describe_attachment.go:136-147`). Edge columns are `from_node_id` / `to_node_id` / `kind` —
**not** `src_id`/`dst_id`/`type` (`migrations/20260527000001_graph_schema.sql`, `CREATE TABLE
graph.edges`).

```sql
LEFT JOIN LATERAL (
  SELECT string_agg(
           left(trim(COALESCE(ab.description,'') || ' ' || COALESCE(ab.ocr_text,'')), 500),
           ' ')                                              AS att
  FROM graph.edges e
  JOIN graph.artifact_bodies ab ON ab.node_id = e.to_node_id
  WHERE e.from_node_id = n.id AND e.kind = 'REFERENCES'
) a ON true
```

and `text` becomes the message text with `a.att` appended (message text **first**, so it survives
the `LEFT(blob, 2000)` truncation at `:213`).

Two constraints that matter, both about not letting OCR drown the signal:

- **Per-attachment cap of 500 chars** (the `left(...)` above). A full-page form screenshot OCRs to
  thousands of characters; uncapped, one image evicts every real message from a 2000-char blob.
- **Message text ordering is preserved.** `blob` is still `string_agg(text, ' ')` grouped per
  thread; only each `text` grows a suffix.

A message with no attachments must produce byte-identical `text` to today — `COALESCE`/`NULLIF` so
no stray whitespace or `NULL` propagation. The `LATERAL` must not multiply rows (it aggregates, so
it returns exactly one row) — verify `msg_count` and `participants` are unchanged for a thread that
has attachments.

### Part 2 — surface the attachment in the DM

`buildAlert` (`detect_hot_topics.go:435-460` area). When the thread's messages reference any
attachment that has a body, add one line naming it, e.g. `📎 _<title or filename>_`. One line, from
data already fetched — do not add a query per DM if the messages are already loaded; if they are
not, one extra query for the thread is acceptable.

### Part 3 — make the gate and the scope editable

Three small, related changes:

1. **`update` handler** (`detect_hot_topics.go`, `func (h *Subscriptions) update`) currently accepts
   **only** `sources` and runs `UPDATE ... SET sources=$2`. Extend it to accept optional
   `min_participants`, `scope_definition`, and `active`, updating only the fields present in the
   request body (pointer fields, `COALESCE`-style partial update). Existing callers sending only
   `sources` must keep working unchanged.

2. **Guard: a lone-message subscription must be channel-scoped.** In both `create` and `update`,
   reject `min_participants < 2` when `channel_filter` is empty, with a 400 and a clear message.
   Reason: `min_participants = 1` makes `participants >= 1` true for every thread, so the gate
   collapses entirely onto the LLM judge. Scoped to one channel that is the intent; unscoped it is
   an LLM judge call for every thread in the graph, every 5 minutes. The `graph.topic_judgments`
   cache (`migrations/20260704000001_topic_judgments.sql`, reused until `msg_count` changes,
   `detect_hot_topics.go:121-124`) bounds it to ~one call per new message, which is still the whole
   workspace. This guard is the cost cap — do not skip it.

3. **Dashboard GUI.** Subscriptions are managed in `dashboard/src/pages/LiveGlobe.tsx:1009-1204`
   (create/list) with the client in `dashboard/src/api.ts:565-646`. Today `min_participants` is only
   *rendered* (`LiveGlobe.tsx:3345`, `≥{s.min_participants} people discussing`) and
   `scope_definition` is not in the client model at all (only the derived `scope_summary` is,
   `api.ts:585`). Add inputs for `min_participants` and `scope_definition` to the create form and an
   edit affordance on an existing subscription, wired to the extended `update` endpoint. Surface the
   400 from the guard as a readable inline error, not a silent failure.

   This repo's standing rule: a new setting that is not editable in the dashboard GUI is not done.

---

## Files expected to change

| File | Change |
|---|---|
| `internal/graph/handlers/detect_hot_topics.go` | `findHotThreads` SQL (LATERAL join, `text` suffix); `buildAlert` attachment line; `create` guard; `update` extended fields + guard |
| `internal/graph/handlers/detect_hot_topics_*_test.go` | new tests, see below |
| `dashboard/src/api.ts` | `scope_definition` on the model; `update` payload gains `min_participants` / `scope_definition` / `active` |
| `dashboard/src/pages/LiveGlobe.tsx` | form inputs + edit affordance + error surfacing |

No migration is required — `min_participants` already exists (`migrations/20260626000002_graph_topic_subscriptions.sql`) and accepts `1`; `scope_definition` already exists
(`migrations/20260629000001_topic_scope_sources.sql`). If you find yourself writing a migration,
stop and re-read this line.

**Rebuild the embedded dashboard** after touching `dashboard/src` — the binary serves an embedded
build, so a source-only change ships nothing.

---

## Acceptance criteria

1. A thread whose only message carries an image attachment with `description`/`ocr_text` in
   `graph.artifact_bodies` produces a `blob` containing that text. Proven by a test, not by reading
   the SQL.
2. A thread with **no** attachments produces a `blob` byte-identical to the pre-change behaviour.
3. `msg_count` and `participants` are unchanged for a thread that has attachments (the LATERAL does
   not fan out rows).
4. Per-attachment contribution is capped at 500 chars; a 5000-char OCR does not evict message text
   from the 2000-char blob.
5. `PATCH`/`PUT` on a subscription with only `{"sources": [...]}` behaves exactly as before.
6. `min_participants: 1` with an empty `channel_filter` is rejected with 400 on both create and
   update; with a non-empty `channel_filter` it is accepted and persisted.
7. `min_participants` and `scope_definition` are both settable and re-editable from the dashboard,
   and a rejected save shows the server's message.
8. `go build ./...`, `go vet ./...`, `go test ./internal/graph/...` all pass. Dashboard builds.

## How to verify

- **Unit** — table-driven tests over `findHotThreads` against the scratch DB. **Never run handler
  integration tests against the live dev DB** — it truncates the graph and the fixtures sync to
  prod. Use the `agentmem_test` scratch database. This is a hard rule in this repo.
- **Fixture for criterion 1** — insert a `slack` node, a `slack_file:` node, a `REFERENCES` edge
  between them, and an `artifact_bodies` row with known `description`/`ocr_text`; assert the
  returned `Blob` contains the OCR marker string.
- **Regression for criterion 2** — same thread minus the attachment rows; assert `Blob` equals the
  message text exactly.
- **API** — `curl` the create/update endpoints for the 400 and the 200 cases; show both responses.
- Do **not** deploy. `make deploy` requires an explicit request in the round that runs it.

## Report back

The diff, the test output quoted (not summarised), the two `curl` responses, and anything you left
out. If a test fails, say so with the failure text — a named failure is worth more than a green
summary you did not earn.

---

## Known landmines

- `internal/graph/handlers/importance.json` is loaded but its **scores are not used by the
  notifier** — only the override *names* are resolved to eeids (`importance.go:45-74`, and it is a
  no-op unless `owner == cfg.OwnerEEID`). Do not "fix" scoring here.
- `max_author_depth` is dead: persisted, defaulted, exposed over JSON, never referenced in any
  predicate (`findHotThreads` binds only 4 params, `:227`). Leave it; file a follow-up.
- `min_participants` DB default is 4, API create default is 2 (`:761`). Rows predating the API
  behave differently. Leave it; file a follow-up.
- `graph.channel_notifications.notified_at` has no reader anywhere. Unrelated to this work.
- The judge's own system prompt is at `detect_hot_topics.go:355-377` and instructs strictness
  ("If it is mainly about a different subject … answer false"). Feeding it OCR makes that
  strictness matter more, not less — do not weaken the prompt to compensate.
