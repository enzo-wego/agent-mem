# Plan: move flat memory back to the cheap tier (haiku)

Repo: `~/go/src/github.com/agent-mem`, branch `feat/flat-memory-cheap-tier` off
`main` @ `da0b7dc` (already created and checked out for you).

## Why

Before the gateway migration, flat memory ran on `google/gemini-2.5-flash`
(`config.go:556` at `9bd3faa~1`), and the graph had a *separate*
`graph_gemini_model`. The field comment said so explicitly: "graph judge/describe
model; empty = use GeminiModel (flat memory keeps its tuned model)".

Collapsing everything onto intent tiers put both ends on `summary`, which is
`claude-sonnet-5`. That silently promoted flat memory — one LLM call per
inbound message, the highest-volume path in the service — from a flash-class
model to the most expensive one available. Nobody asked for that; it is a
regression from the migration, not a decision.

Haiku-4.5 is the like-for-like restore, not a downgrade: same class as
gemini-2.5-flash, and the work is structured JSON extraction, which is what it
is good at.

## Change

**1. `internal/worker/server.go:61`** — add to the `flatLLM` interface:

```go
GenerateCheap(ctx context.Context, systemPrompt, userMessage string) (string, error)
```

`*llmgateway.Client` already implements it (`internal/llmgateway/client.go:216`),
so no other wiring changes. Update the interface's doc comment: flat memory now
runs on the cheap tier.

**2. `internal/worker/processor.go`** — two call sites, `Generate` →
`GenerateCheap`:

- `:146` observation extraction
- `:219` session summary

Leave a short comment at each saying *why* cheap: this is per-message volume,
it was flash-class before the gateway, and sonnet here is what made the
amplification bug expensive.

**Do not** touch `handlers.go:126` — it only calls `Embed` and is unaffected.
**Do not** add a third gateway tier. Flat memory shares `cheap` with the
topic-link confirm gate; both want haiku. Splitting them is only worth doing
when someone actually wants different models, and that is a gateway change.

**3. `dashboard/src/pages/Settings.tsx`** — the tier hint shipped in `da0b7dc`
is now wrong. It currently reads:

> summary serves thread and cluster summaries, hot-topic detection, scope
> refresh, feature-entity derivation, **and flat-memory observation
> extraction**; cheap serves **only** the high-volume topic-link confirm gate
> (one yes/no per candidate — **flat memory is not cheap**) …

Rewrite so `summary` lists graph work only (thread and cluster summaries,
hot-topic detection, scope refresh, feature-entity derivation) and `cheap` lists
flat-memory observation extraction and session summaries **plus** the topic-link
confirm gate. Drop the "flat memory is not cheap" parenthetical entirely — it
becomes the opposite of true. Keep the describe clause as-is. Same muted
`text-xs` style, still one paragraph.

Grep the file for any other copy that names which tier serves flat memory; the
hint above is the one I know about, do not leave a second stale one behind.

## Verify

```bash
cd ~/go/src/github.com/agent-mem
unset DATABASE_URL          # integration tests TRUNCATE the DB if this is set
go build ./... && go vet ./... && go test ./...
#   4 failures in internal/hooks + internal/skills are PRE-EXISTING on main

cd dashboard && npx tsc --noEmit && npm run build
cd .. && rm -rf internal/worker/dashboard && cp -R dashboard/dist internal/worker/dashboard
go build ./...
```

The embedded bundle has no Makefile step — rebuild it by hand or the deployed
dashboard serves stale JS.

## Rules

- **Do not deploy.** Commit to `feat/flat-memory-cheap-tier` and push; the lead
  reviews, merges and deploys.
- **Do not touch the VPS** — not `.env`, not the database, not systemd.
- **Do not unpause.** `processing_paused` stays `true`.
- Do not change any gateway config value. `MODEL_CHEAP` is already
  `claude-haiku-4-5`; this task routes flat memory to it, it does not set it.
