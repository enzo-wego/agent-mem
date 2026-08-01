# Plan: fix gateway proxy bug + gateway config section

Repos: `~/go/src/github.com/agent-mem` (branch off `main` @ `8e0f392`) and
`~/go/src/github.com/llm-gateway` (branch off `main` @ `c4ed072`).

Three items. **Item 1 is a release blocker** — do it first and independently of the rest.

---

## 1. P0 — proxy env silently breaks EVERY gateway call

### Evidence

```
VPS /var/go/src/github.com/agent-mem/.env
  HTTP_PROXY=http://172.18.0.1:8888
  HTTPS_PROXY=http://172.18.0.1:8888
  NO_PROXY=localhost,127.0.0.1,postgres          <-- gateway host NOT listed

worker container    172.18.0.3   (agent-mem_default)
gateway listening   172.18.0.1:8750
ufw                 8750/tcp ALLOW from 172.18.0.0/16
```

A fresh container on the same network reaches `http://172.18.0.1:8750/health`
fine. The worker cannot:

```
GET /api/llm-gateway/health
{"available":false,"error":"llm-gateway unreachable: Get \"http://172.18.0.1:8750/health\": context deadline exceeded"}
```

`internal/llmgateway/client.go:127` builds `&http.Client{Timeout: RequestTimeout}`
with the **default transport**, which honours `HTTP_PROXY`. So gateway traffic is
sent to the :8888 relay, which does not forward it, and every call times out
after the full timeout.

### Why it matters more than the panel

The panel is the only thing exercising the gateway right now because
`processing_paused = true`. The same broken path serves **generation, cheap
judge, describe, and all embeddings**. Unpausing before this is fixed fails 100%
of LLM work — and each failure burns the full 200s client timeout first.

### Fix

1. `internal/llmgateway/client.go` — give the client an explicit transport that
   never proxies. The gateway is a same-host bridge address; a proxy is never
   correct for it, and inheriting ambient proxy env is how this broke silently.
   Clone `http.DefaultTransport`, set `Proxy: nil`, keep the timeout.
   Comment it so nobody "helpfully" restores the default.
2. `internal/worker/gateway_handlers.go` and `internal/worker/usage_handlers.go`
   — both use `http.DefaultClient` for gateway calls. Same treatment; do not
   share a package-level client with anything that legitimately needs the proxy.
3. VPS `.env` — add the gateway host to `NO_PROXY` as defence in depth:
   `NO_PROXY=localhost,127.0.0.1,postgres,172.18.0.1`. This is config, not code:
   flag it in the report, **do not** edit the VPS yourself.
4. Regression test: construct the client with `HTTP_PROXY` set in the
   environment and assert the request still goes direct (an `httptest` server on
   127.0.0.1 that receives the request proves it bypassed the proxy).

**Do not** "fix" this by widening the timeout or adding retries — it is a
routing bug, not a slowness bug.

---

## 2. Embedding Dimensions — should it move to llm-gateway?

**No, and it should stop being editable.**

It cannot move: agent-mem has **two** widths, and they are properties of its own
schema — `observations.embedding` is `vector(768)`, `graph.artifact_index.embedding`
is `halfvec(3072)`. A single gateway-side setting cannot serve both, and the
gateway has no way to know which caller is asking.

But the current UI is inconsistent and a footgun: the graph width is a Go
constant (`handlers.GraphEmbeddingDims`) while the flat width is a dropdown.
Changing the dropdown does not re-embed anything — it just makes every
observation insert fail with `expected 768 dimensions, not 3072`, which reads
like a schema fault.

### Change

- Render Embedding Dimensions **read-only** in Settings: show `768 (flat) /
  3072 (graph)` as informational text, not a `<select>`.
- Keep `gemini_embedding_dims` in the settings table and the API response — the
  client still needs it — just remove the ability to change it from the GUI.
- Update the hint to say it is fixed by the database schema and changing it
  requires a migration plus a full re-embed.

---

## 3. Gateway configuration section (view + edit)

The user wants gateway config editable from the agent-mem dashboard.

**The gateway stays the owner.** agent-mem must NOT store a copy of these values
— that creates two sources of truth and they will drift, and other clients
(Claude-Code-Remote) share this gateway. The dashboard is a *client UI* that
reads and writes the gateway's own config through it.

```
dashboard  ->  agent-mem  GET/PUT /api/llm-gateway/config  ->  llm-gateway  GET/PUT /config
                (pure proxy, stores nothing)                    (owns + persists)
```

### 3a. llm-gateway: `GET /config` and `PUT /config`

- Authenticated with the existing `require_key` dependency, like `/usage`.
- `GET` returns the editable runtime knobs with current values:
  `BACKEND_SUMMARY`, `BACKEND_CHEAP`, `BACKEND_DESCRIBE` (`claude|openrouter`),
  `MODEL_SUMMARY`, `MODEL_CHEAP`, `OR_MODEL_SUMMARY`, `OR_MODEL_CHEAP`,
  `EFFORT_SUMMARY`, `EFFORT_CHEAP`, `FALLBACK_ON_QUOTA`, `MAX_BUDGET_USD`,
  `CLAUDE_TIMEOUT_S`.
  Never return `API_KEY`, `OPENROUTER_API_KEY`, or any secret.
- `PUT` accepts a partial object, validates, applies to the live process, and
  **persists to `.env`** so a restart keeps it. Rewrite only the keys given;
  preserve comments and unrelated lines. Write to a temp file and rename, so a
  crash mid-write cannot truncate `.env` and take the gateway down on next boot.
- Validate hard: backends must be `claude|openrouter`, efforts must be a known
  tier, numbers must parse and be positive. Reject unknown keys rather than
  silently ignoring them — a typo that no-ops looks identical to a setting that
  did not take.
- `CLAUDE_TIMEOUT_S` is load-bearing: it must stay **below**
  `llmgateway.RequestTimeout` (200s) which must stay below
  `handlers.SummaryLease` (240s). Reject a value >= 200 with an explanatory
  error rather than accepting a config that causes duplicate LLM calls.

### 3b. agent-mem: proxy endpoints

- `GET /api/llm-gateway/config` and `PUT /api/llm-gateway/config` in
  `internal/worker/gateway_handlers.go`, mirroring the existing health proxy:
  same non-proxied client (item 1), same `{available,error,...}` envelope so the
  dashboard renders a clear unreachable state instead of a blank panel.
- Stores nothing. No new settings rows, no new config fields.

### 3c. Dashboard: "Gateway Configuration" section

- Below the existing read-only Gateway Status panel.
- Editable controls with sensible input types: selects for the three backends
  and the two effort tiers, a checkbox for fallback, text for model ids, numbers
  for budget and timeout.
- Save calls the PUT proxy, then refetches both config and health so the
  displayed state is what the gateway actually reports back — never optimistic
  local state.
- Show a clear error banner when the gateway is unreachable, and disable the
  controls rather than letting a save silently fail.
- Label `FALLBACK_ON_QUOTA` honestly: when the Claude seat's window closes,
  generation falls back to OpenRouter and spends the embedding budget. That is
  the single most consequential toggle on the page.

---

## Verify before reporting done

```bash
cd ~/go/src/github.com/agent-mem
unset DATABASE_URL          # integration tests TRUNCATE if it is set
go build ./... && go vet ./... && go test ./...
#   4 failures in internal/hooks + internal/skills are PRE-EXISTING on main, ignore

grep -rn "http.DefaultClient" --include=*.go internal/worker internal/llmgateway
#   must not appear on any gateway call path

cd dashboard && npx tsc --noEmit && npm run build
cd .. && rm -rf internal/worker/dashboard && cp -R dashboard/dist internal/worker/dashboard
#   ^ no Makefile step for the embedded bundle; it must be rebuilt by hand

cd ~/go/src/github.com/llm-gateway
python3 -c "import ast;[ast.parse(open(f).read()) for f in ['app/main.py','app/config.py','app/openrouter.py']]"
python3 -m pytest test_gateway.py -q      # if pytest is available
```

## Rules

- **Do not deploy.** Commit and push branches only; the lead reviews, merges and
  deploys.
- **Do not edit anything on the VPS** — not `.env`, not the database, not
  systemd. Report what needs changing there.
- **Do not unpause.** `processing_paused` stays `true` on both machines.
- Do not touch the gateway's *current* model/backend values; item 3 builds the
  means to change them, it does not change them.

## Landmines

- Two embedding widths, not interchangeable (768 flat / 3072 graph). Wrong width
  fails every insert with a message that looks like a schema bug.
- Never send `task_type` on embeddings — stored vectors were produced without
  one; adding it moves queries to a different vector space and search silently
  degrades instead of failing.
- Transient LLM failures must requeue, not fail: `pending_messages.status='failed'`
  is terminal and `ClaimPendingMessage` only selects `'pending'`. Use
  `llmgateway.IsRetryable` + `RequeuePendingMessage`.
- `ssh … docker compose exec` is blocked by the permission classifier. Read-only
  `psql` over ssh is fine. VPS db: `agentmem/agentmem`, port **5433**.
