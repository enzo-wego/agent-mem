# Thread summaries lose everything after line 1

Plan for `agent-mem-t8r` (P1) and `agent-mem-bsv` (P2, blocked by t8r).
Written 2026-08-17. Evidence gathered from prod (`ssh enzo@payments`).

## Trigger

`https://wego.slack.com/archives/C09H1QMK882/p1786709371372099` is in the graph but
`graph_search` cannot find it by anything it actually says, and it refuses to link to
the PK tax threads it obviously belongs with.

## Root cause

`internal/graph/handlers/summarize_thread.go:92`

```go
line := withDept(author, dept, jobTitle, domain, role) + ": " + firstLine(body, 280) + "\n"
```

`firstLine` (`internal/graph/handlers/channels.go:292`) cuts at the first `\n`. The 280
is a rune cap that almost never binds — the newline does.

Surbhi's message body:

```
Hi @Supriya @liping
Now that we have completed all the changes for PK & if all is good with the latest
test cases, we'd like to plan the release of PK taxation.
To keep things streamlined from a filing perspective, 1st September can be targetted.
Let us know what you think.
cc @Payments Geeks @Alex
```

What `genThreadDeepSummary` received for that message:

```
Surbhi Babbar (…): Hi @Supriya @liping
```

The release proposal and the 1 September date were never shown to the LLM. The four
replies (2026-08-17 04:18–04:25) are single-line, so they survived intact — and they are
all about NON-PKR test cases. The summary it produced is a *correct* summary of the
transcript it was given:

| field | value |
|---|---|
| `summary` | NON-PKR test case review and CSV verification |
| `overview` | Enzo completed NON-PKR test cases and asked Nagendra to review them… |
| `kind` | substantive |
| `updated_at` | 2026-08-17 04:25:24 |

The transcript is the bug, not the summarizer.

## Consequences

### Search

`index_artifact.go:143` embeds `topic + "\n\n" + overview` — the thread summary, never
the body. So the node is retrievable only by words it does not contain.

| query | result |
|---|---|
| `PK taxation release September Pakistan tax` | absent from top 15 |
| `NON-PKR test cases review CSV verification` | rank 1, sem 0.856 |

### Linking

The topic judge also reads the summary. It parsed `NON-PKR` (bookings not in Pakistani
Rupee — still PK tax work) as *non-Pakistan*, and used that as the reason to reject.

Against `slack:CUV9EAYGY:1786677145.318029` (Surbhi's own PK Tax release thread, 3h
apart): `same_topic=f`, `confidence=0.85`,

> Artifact A concerns NON-PKR (non-Pakistan) test case status; Artifact B concerns PK
> (Pakistan) Tax release preparation… different geographic scopes

19 of 20 judgments rejected. The one survivor is `C09H1QMK882:1780380612.840289`
(same channel, Tax V2 PK, 0.82). The corrupted summary did not merely fail to attract
links — it actively repelled the correct ones.

**Note on the rejection count:** 89% of all judgments in the corpus are rejections
(41,651 of 46,735). 19/20 is not statistically anomalous. The anomaly is the *reason*.

### Not a consequence: the timeline

Dashboard `/timeline` reads `/api/search/timeline` → `observations`,
`session_summaries`, `user_prompts` — the flat coding-session memory. No graph node has
ever appeared there. Nothing to fix.

## Blast radius (prod, 2026-08-17)

| metric | count |
|---|---|
| slack nodes total | 26,398 |
| with multi-line bodies | 4,188 (16%) |
| multi-line with first line < 60 chars | 1,997 |
| thread_summaries rows | 1,802 |
| threads containing a multi-line message | 1,440 (80%) |

The 1,997 are the severe cases: a greeting or bare @-mention opener means near-total
content loss. Since 80% of summarized threads are affected, a targeted backfill saves
only ~20% over a blanket regenerate — not worth bespoke SQL.

## Fix

### Phase 1 — stop the truncation

Replace `firstLine(body, 280)` at `summarize_thread.go:92` with a flatten that spans
newlines: collapse `\s*\n\s*` runs to a single separator, then cap at ~400 runes.

- New helper, not a change to `firstLine` — `firstLine` has other callers where
  cutting at the newline is correct (chips, titles).
- The existing 7,000-char transcript builder cap already bounds total prompt size, so
  a longer per-message cap cannot blow the budget; it just means fewer messages fit on
  very long threads. Acceptable: a truncated tail beats a truncated head.
- Unit test: a multi-line body must reach the transcript with its line-2 content
  present.

### Phase 2 — regenerate

Bump the signature prefix `v8:` → `v9:` in **both** places (they must stay in sync):

- `summarize_thread.go:115`
- `channels.go:522`

This invalidates all 1,802 cached summaries. Each one then walks
`summarize_thread → index_artifact (force) → link_topics`.

### Phase 3 — cap and canary (mandatory)

`link_topics` is ~15 judge calls per node. 1,802 nodes ≈ **27,000 judge calls** plus
1,802 summarize + 1,802 embed. This is the expensive step.

1. Canary: force-regenerate ~20 threads including `C09H1QMK882:1786709371.372099`.
   Confirm the summary names "PK taxation" and "1 September", and that
   `graph_search("PK taxation release September")` returns it.
2. Confirm the `CUV9EAYGY:1786677145.318029` pair flips to `same_topic=t` on re-judge.
   `topicLinkContentHash` includes both summaries, so the changed summary produces a
   cache miss automatically — no `force` needed.
3. Only then bump the prefix. Watch `graph.jobs` queue depth and LLM spend.

### Phase 4 — agent-mem-bsv

Re-measure after Phase 3. The NON-PKR misreading may largely disappear once summaries
carry real content. What will not disappear:

- **Rejections are unobservable.** 41,651 rows nobody can see. No dashboard view, no
  metric on the `why` text. This one was found only by hand-querying the table.
- **Re-judging is shortlist-bound.** A pair is re-judged only if it reappears in the
  new embedding shortlist. A correct partner sitting at rank N+1 keeps its false
  rejection forever, silently.

## Correction to an earlier reading

`topic_link_judgments` is **not** a stale cache. `topicLinkContentHash`
(`link_topics.go:395`) already keys on the rules version, both summaries, shared
identifiers, the coarse time bucket, and case ids. A corrected summary changes the hash
and forces a fresh judgment. `linkTopicsForceFromIndexArtifact` returning `false` is
deliberate and correct — the hash does the invalidation. No cache-busting work is
needed; Phase 2 is sufficient.

## Out of scope

- Embedding the body alongside the summary. Would mask t8r rather than fix it, and
  doubles index size.
- A domain glossary for the judge prompt (PKR = currency, PK = market). Revisit only
  if Phase 4 re-measurement still shows the misreading.
