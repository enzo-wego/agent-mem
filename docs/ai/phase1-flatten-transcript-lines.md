# Phase 1 — LLM transcript builders must not cut at the first newline

Worker brief. Self-contained: assume no knowledge of the conversation that produced it.

Issue: `agent-mem-t8r` (P1). Parent plan: `docs/ai/thread-summary-firstline-plan.md`.
Repo: `/Users/neocapitelo/go/src/github.com/agent-mem`, branch `main`.

## Background

`firstLine(s string, n int)` in `internal/graph/handlers/channels.go:292` cuts its input
at the first `\n`, then caps at `n` runes. That is correct for the ~20 callers that build
a title, chip, label or error string.

It is wrong for the three places that build a **transcript to feed an LLM**. There, the
newline cut silently deletes everything after line 1 of every multi-line Slack message —
the rune cap almost never binds because the newline gets there first.

Observed in production on 2026-08-17. Node
`slack:C09H1QMK882:1786709371.372099` (#vat_data_ota_pk) has this body:

```
Hi @Supriya @liping
Now that we have completed all the changes for PK & if all is good with the latest
test cases, we'd like to plan the release of PK taxation.
To keep things streamlined from a filing perspective, 1st September can be targetted.
Let us know what you think.
cc @Payments Geeks @Alex
```

What reached the summarizer:

```
Surbhi Babbar (Product · Manager): Hi @Supriya @liping
```

The resulting summary describes only the thread's replies. The node is now unfindable by
any word in its own body, and the topic judge rejected 19 of 20 candidate links using
reasoning derived from the wrong summary.

Scale: 4,188 of 26,398 Slack nodes have multi-line bodies; 1,997 of those open with a
line under 60 chars (a greeting or bare @-mention — near-total content loss).

## Goal

Add one helper that flattens a multi-line body into a single transcript line without
losing content, and use it at the three LLM-transcript call sites.

## Non-goals — do NOT do these

- **Do NOT bump the summarize_thread signature prefix `v8:` → `v9:`** in
  `summarize_thread.go:115` or `channels.go:522`. That is Phase 2 and it is cost-gated:
  it regenerates 1,802 thread summaries and cascades to roughly 27,000 LLM judge calls.
  Bumping it in this change would fire that run unreviewed. Leave both prefixes at `v8:`.
- **Do NOT change `firstLine` itself.** Its title/chip/label callers are correct as they
  are. Add a new function alongside it.
- **Do NOT touch `notify_watch_channels.go:259`** (`firstLine(text, 600)`). That renders a
  user-visible Slack notification, not an LLM transcript; flattening there changes what
  humans read. Out of scope.
- Do not touch any other `firstLine` caller.
- No backfill, no migration, no job enqueue, no deploy.

## The change

### 1. New helper in `internal/graph/handlers/channels.go`, next to `firstLine`

```go
func flattenLines(s string, n int) string
```

Behaviour:

1. Trim leading/trailing whitespace.
2. Collapse every run of whitespace that **contains a newline** into the separator
   `" / "`. This keeps the structural break between lines visible to the LLM instead of
   running sentences and bullets together.
3. Collapse every remaining run of internal whitespace into a single space.
4. If the result exceeds `n` runes, truncate to `n` runes and append `"…"` — matching
   `firstLine`'s existing convention exactly.
5. Empty or whitespace-only input returns `""`.

Give it a doc comment saying why it exists and why `firstLine` was not changed.

### 2. Use it at the three transcript sites, cap 400

| file:line | current | becomes |
|---|---|---|
| `internal/graph/handlers/summarize_thread.go:92` | `firstLine(body, 280)` | `flattenLines(body, 400)` |
| `internal/graph/handlers/detect_hot_topics.go:522` | `firstLine(m.text, 280)` | `flattenLines(m.text, 400)` |
| `internal/graph/handlers/cluster_summary.go:531` and `:533` | `firstLine(m.body, 280)` / `firstLine(m.title, 280)` | `flattenLines(...)`, cap 400 on both |

400 rather than 280 because a flattened multi-line message is legitimately longer. All
three builders already have their own total-transcript ceiling (`summarize_thread.go`
caps the builder at 7,000 chars), so a larger per-message cap cannot blow the prompt
budget — on a very long thread it means fewer messages fit, which is the right trade: a
truncated tail beats a truncated head.

### 3. Unit test in `internal/graph/handlers/channels_test.go`

Pure unit test, package `handlers`, no database — follow the existing style in that file
(see `TestDefaultContinentsIgnore`). Table-driven is fine. Cover:

- **The production case**: the Surbhi body above must produce a string containing
  `"PK taxation"` and `"1st September"`. This is the test that fails if the bug returns.
- Single-line input: result is identical to `firstLine` for the same input.
- Separator: two lines become `line1 / line2`.
- Blank lines between paragraphs do not produce a doubled or empty separator.
- Leading/trailing newlines produce no leading/trailing separator.
- Over-cap input is truncated to exactly `n` runes plus `"…"`.
- Empty and whitespace-only input return `""`.
- Multi-byte input is truncated on rune boundaries, not bytes.

## Verification — run these and paste the real output

```bash
cd /Users/neocapitelo/go/src/github.com/agent-mem
go build ./...
go vet ./internal/graph/handlers/
go test ./internal/graph/handlers/ -run 'TestFlattenLines' -v
go test ./internal/graph/handlers/ -count=1
```

**Database warning.** Integration tests in this package skip themselves unless
`DATABASE_URL` is set. Leave it unset. Never point them at the live dev or production
database — those tests truncate graph tables. If any test does not skip and instead
tries to connect, stop and report it rather than setting a DSN.

## Acceptance criteria

1. `flattenLines` exists in `channels.go` with a doc comment; `firstLine` is unmodified.
2. All four call sites listed in the table use `flattenLines` with cap 400.
3. Both `v8:` prefixes are untouched — verify with
   `grep -rn 'v8:' internal/graph/handlers/` returning exactly the two pre-existing hits.
4. `notify_watch_channels.go:259` is untouched.
5. `go build ./...` and `go vet` clean.
6. The new test fails against the old `firstLine` behaviour and passes against
   `flattenLines` — state explicitly that you confirmed this, and how.
7. Full package test run passes, with DB-backed tests skipping, not failing.
8. No TODO placeholders, no `t.Skip` added to the new test, no stubbed assertions.

## Report back

The diff summary, the four changed call sites, the verbatim output of every command
above, and anything you chose not to do and why. Do not commit or push — the conductor
reviews and ships.
