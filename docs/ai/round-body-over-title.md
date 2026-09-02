# Plan — the notifiers read the message, not its label

**Issue:** `agent-mem-syqh` (P1) · **Repo:** `~/go/src/github.com/agent-mem` · **Branch off:** `main` (at `ea5b697`)

You have no memory of the conversation that produced this. Every reference below was checked
against the tree at `ea5b697` and against the payments hub on 2026-09-02.

---

## The defect

Four read paths in the notification stack take a node's **title** in preference to its **body**:

| Site | Feeds |
|---|---|
| `detect_hot_topics.go:218` | `first_text` and `blob`, i.e. the LLM relevance judge's entire input |
| `detect_hot_topics.go:489` | the alert DM's "who said what" transcript |
| `notify_watch_channels.go:214` | the watched-channel DM text |
| `summarize_thread.go:79` | the cached thread summary, which the alert DM's briefing is built from |

All four spell it `COALESCE(NULLIF(n.title,''), n.body, '')`. For a Slack node the title is a
short generated label, so wherever one exists the real message is never read.

Ross Veitch's CEO directive in `#payments`:

```
slack:CKQ6XGTCZ:1788327991.032099
title_len 42   body_len 1960
title: "Google's Universal Commerce Protocol (UCP)"
```

The judge ruled `relevant=false` on those 42 characters. The body it never saw asks for exactly
what the subscription is scoped to: "what our checkout and payments stack needs to support UCP:
tokenized payments, instant confirmation, Merchant of Record flows". Those phrases sit roughly
1100 to 1300 characters into the body. The DM that eventually reached the subscriber said he
"shared a mention ... with no further discussion in the thread", describing a link-drop rather
than four numbered asks with a two-week deadline.

Scale, measured on the hub over 30 days: 6000 slack nodes, 268 with a title, and 30 of those
have a body more than twice the title length. Worst case in the table is a 65-character title
over a 56926-character body. Roughly one a day, and skewed toward the substantial messages.

## Goal

For Slack nodes, the judge, the thread summary, and both DM builders read the message body.

## Approach

At all four sites, flip the preference: prefer the body, keep the title as the fallback for a
node that has one and no body (an attachment-only message).

```sql
COALESCE(NULLIF(n.body,''), n.title, '')
```

That is the whole change in SQL terms. Do not concatenate title and body: the title is derived
from the body, so it adds nothing but duplication.

**Then deal with the truncation that this exposes**, which is the part that needs care rather
than a flip:

- `detect_hot_topics.go` builds `blob` as `string_agg(text || …)` and then reads
  `LEFT(COALESCE(g.blob,''), 2000)`. With real bodies in play, one 56KB message would fill the
  entire budget and evict every other message in the thread. Add a per-message cap of
  `left(text, 2000)` inside the aggregate, and raise the overall cap to `LEFT(…, 6000)`.
  The 2000-per-message figure is chosen so Ross's 1960-character body survives intact; the
  payments terms that decide his verdict sit at ~1300, so a smaller cap would reintroduce the
  bug in a subtler form. Attachment text keeps its existing 500-char cap.
- `summarize_thread.go` has no equivalent cap on its per-message text. Read the builder loop
  below line 92 before changing anything, and if the prompt is unbounded, cap each message
  the same way rather than leaving a 56KB message to blow the prompt. Report what you found
  and what you chose.

## Non-goals

- Do not change the title-generation path. Titles stay as they are; this is about which field
  the readers prefer. Also do not go looking for the generator: no `UPDATE graph.nodes SET title`
  exists in this repo for slack types (ingest passes an empty title, `ingest_content.go:492-495`),
  so it is written elsewhere and is not in scope.
- Do not touch the eligibility gate, the continents config, `min_participants`, the always-alert
  bypass from #51, or subscription 1's `scope_definition`.
- Do not change how non-Slack node types are read anywhere else in the codebase. A Jira or
  Confluence title is a real title and other call sites may legitimately prefer it. Confine the
  change to these four sites.

## Acceptance criteria

- `go build ./...` clean, `go test ./internal/graph/handlers/...` pass.
- A test proves the judge blob for a thread whose node has a short title and a long body
  contains text from the middle of the body, not just the label. `TestFindHotThreads_AttachmentBlob`
  (`detect_hot_topics_internal_test.go:546`) is the closest existing pattern to copy.
- A test proves the title is still used when the body is empty.
- A test pins the per-message cap, so a single huge message cannot evict its thread-mates from
  the blob.

## How to verify on the hub

Deploy per `CLAUDE.md`: `ssh enzo@payments`, `git pull --ff-only`,
`docker compose up -d --build worker`. `make deploy` is broken and targets the retired VPS.

1. Confirm the running binary carries the change, not just the build log.
2. **Prove the judge's input changed**, which is deterministic and therefore the real evidence.
   Run `findHotThreads`'s query by hand for subscription 1 and show that the blob for
   `slack:CKQ6XGTCZ:1788327991.032099` now contains the string `Merchant of Record`. Quote it.
3. **Prove the reader-facing text changed.** Ross's thread summary is cached and hash-skipped
   (`summarize_thread.go:151`), so clear or invalidate that cache entry for
   `(CKQ6XGTCZ, 1788327991.032099)`, let `summarize_thread` regenerate, and quote the old and
   new summary side by side. The old one is the "shared a mention ... with no further discussion"
   text.
4. **Note what cannot be re-tested and why.** Ross is now `always_alert` (#51), so his messages
   bypass the judge and will never produce a fresh verdict. Do not try to force one, and do not
   remove his flag to test. If you want a judge-verdict flip as evidence, find a different
   long-body thread from a non-always-alert author, clear its `graph.topic_judgments` row, and
   report the before and after verdicts.
5. Report the judge prompt size for the UCP thread before and after, so the token cost of the
   raised caps is a measured number rather than a guess.
