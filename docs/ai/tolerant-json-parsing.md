# Plan: tolerate preamble-wrapped JSON in every LLM response parser

Repo: `~/go/src/github.com/agent-mem`, branch `fix/tolerant-json-parsing` off
`main` @ `6fdeddf` (already created and checked out).

## Why this is still broken after the gateway fix

llm-gateway now unwraps a response that is *entirely* one fenced block, and that
fixed the majority — production went from 0 successes to a working queue. But
five minutes of live traffic still shows:

```
7 × parse gemini response: parse observation: invalid character '`'
2 × retry budget exhausted   (permanently failed)
```

Probing `/generate` with the real observation prompt returns clean bare JSON
three times out of three, so the failures are a *minority shape*: the model
sometimes adds a preamble — `Here is the observation:` followed by a fenced
block. The gateway deliberately leaves that alone, because a response that is
prose plus a snippet is indistinguishable, from the gateway's side, from a
caller who genuinely wanted prose. Only the caller knows it asked for JSON.

So the fix belongs here. The gateway restores the contract where it can; these
parsers assert it where they know it applies.

## Change

**1. New helper in `internal/gemini`** (same package as the existing prompts, so
graph handlers and the llmgateway client can both import it — check for an
import cycle with `internal/llmgateway` before choosing the package; if one
exists, put it in a small leaf package instead and say why in the comment):

```go
// ExtractJSON returns the JSON body of an LLM response.
func ExtractJSON(response string) []byte
```

Try in order, returning the first that is valid JSON:

1. the response as-is, trimmed — the normal case, no allocation games
2. the contents of a fenced block, when exactly one ``` fence pair is present
   anywhere in the text (this is the preamble case the gateway leaves alone)
3. the substring from the first `{` to the last `}`, or first `[` to last `]`

If none parses, return the trimmed original so the caller's error message still
quotes what the model actually said. Do **not** return an error from this helper
— the caller's `json.Unmarshal` already produces a good one, and two error paths
for the same condition is how the message ends up saying nothing useful.

Comment it with *why*: the OpenRouter `json_object` contract is gone, the Agent
SDK does not replace it without a schema, and agent-mem passes no schema.

**2. Use it at all nine LLM-response parse sites:**

- `internal/gemini/prompts.go:65` (ParseObservation), `:103` (ParseSummary)
- `internal/graph/handlers/derive_feature_entity.go:71`
- `internal/graph/handlers/link_topics.go:731`
- `internal/graph/handlers/refresh_scope.go:232`
- `internal/graph/handlers/cluster_summary.go:655`
- `internal/graph/handlers/summarize_thread.go:306`
- `internal/graph/handlers/detect_hot_topics.go:369`
- `internal/llmgateway/client.go:317` (Describe)

**Do NOT touch** `notify_watch_channels.go:84` or `channel_filters.go:103`.
Those unmarshal a settings row from the database, not an LLM response. Making
them "tolerant" would paper over a genuinely corrupt setting.

**3. Tests** — table-driven, in `internal/gemini`, over `ExtractJSON` alone:

- bare object → unchanged
- fenced block, whole response → inner JSON
- preamble + fenced block → inner JSON  ← the case in production now
- fenced block + trailing prose → inner JSON
- prose with `{` and `}` and no fence → the braced substring
- two fenced blocks → returns the original untouched (ambiguous; a wrong pick is
  a silent wrong answer, which is worse than a parse error)
- not JSON at all → original returned, caller errors normally

Plus one test per parser that a preamble-wrapped response now parses —
`ParseObservation` at minimum.

## Verify

```bash
cd ~/go/src/github.com/agent-mem
unset DATABASE_URL          # integration tests TRUNCATE if set
go build ./... && go vet ./... && go test ./...
#   4 failures in internal/hooks + internal/skills are PRE-EXISTING
```

Integration tests in `internal/worker` need a scratch DB and refuse anything
whose database name lacks "test":
`DATABASE_URL='postgres://agentmem:agentmem@localhost:5433/agentmem_test'`

If `httptest` fails with `bind: operation not permitted`, ask for approval to run
outside the sandbox. Do **not** delete, skip or weaken the test.

## Rules

- **Do not deploy.** Commit to `fix/tolerant-json-parsing` and push.
- **Do not touch the VPS.** Production is currently *running*; a stray change
  there lands on live traffic.
- **Do not change `processing_paused` in either direction.** The lead owns it.
- **Do not requeue the failed rows.** 8,683 and counting; that is a separate
  decision and a bulk LLM spend.
- Do not run `bd` or `dolt`, and do not commit or push — hand back the working
  tree and the lead does both.
