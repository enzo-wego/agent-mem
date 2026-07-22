# OpenRouter Migration — Design Spec

- **Date:** 2026-07-22
- **Status:** Approved (design), pending implementation plan
- **Initiative:** Standardize agent-mem and Claude-Code-Remote on OpenRouter (company mandate)
- **Repos:** `agent-mem` (Go), `Claude-Code-Remote` (Node.js, branch `release`)

## 1. Goal & Context

The company standardizes LLM access on **OpenRouter**. Both projects currently call the
**Google Gemini API** directly with a `GEMINI_API_KEY`. The mandate is to retire the Google
API key and route through OpenRouter's single key/account.

Key discovery that shapes this design: **OpenRouter now exposes an embeddings endpoint**
(`POST /api/v1/embeddings`) and, critically, serves **`google/gemini-embedding-001`** — the
exact embedding model agent-mem uses today. This means we keep the same embedding model
(no quality change, no vector-space migration for the bulk of data) while satisfying the
mandate. Verified live against production data (see §3).

## 2. Scope

**In scope**
- agent-mem: all direct Gemini API usage — generation (`Generate`, `Describe`) and
  embeddings (`Embed`, `EmbedWithOptions`, `EmbedBatch`).
- Claude-Code-Remote: the **3 direct Gemini API calls** in `src/channels/slack/socket.js`
  (`@google/generative-ai`, model `gemini-2.0-flash`, keyed by `GEMINI_API_KEY`).

**Out of scope**
- Claude-Code-Remote `src/services/daily-summary.js` — uses the Claude Agent SDK
  (`@anthropic-ai/claude-agent-sdk`, Anthropic). Mandate covers the Google key only; the
  Anthropic key stays. (Decision: "Only the Google/Gemini calls".)
- Claude-Code-Remote Gemini **CLI** worker — being replaced with `opencode` separately by
  the owner; not touched here.
- Any embedding-model change. We stay on `gemini-embedding-001`.

## 3. Verification (live test, 2026-07-22)

Real production text was embedded through OpenRouter's `google/gemini-embedding-001` and
compared to the stored vectors (cosine similarity, 1.0 = identical):

| What | Vectors | Cosine vs stored | Result |
|---|---|---|---|
| Core memory — `observations`, `user_prompts`, `session_summaries` (768d, no task_type) | 126,625 | **1.00** | Drop-in, zero re-embed |
| Graph — `graph.artifact_index` (3072d, built with `task_type=SEMANTIC_SIMILARITY`) | 23,433 | **0.87** | Re-embed once (see §4.3) |
| Generation — `gemini-2.5-flash` chat | — | — | Works (valid JSON) |
| `gemini-embedding-2` (rejected) | — | ~0.00 | Different vector space; would force full re-embed |

Cause of the graph 0.87: OpenRouter's OpenAI-shaped embeddings API does **not** pass through
Gemini's `task_type` hint (verified: identical result with and without the param). Same model,
so no quality loss — we re-embed the graph rows without the hint and query them the same way,
keeping the space internally consistent.

## 4. Design — agent-mem

### 4.1 Client (`internal/gemini/client.go`)
Rewrite the internals to call OpenRouter's OpenAI-compatible API; **keep all exported method
signatures** (`Generate`, `Describe`, `Embed`, `EmbedWithOptions`, `EmbedBatch`) so the adapter
(`internal/graph/handlers/gemini_adapter.go`) and all ~19 call sites are untouched.

| Aspect | From (Google native) | To (OpenRouter) |
|---|---|---|
| Base URL | `https://generativelanguage.googleapis.com/v1beta/models` | `https://openrouter.ai/api/v1` |
| Auth | `?key=<apiKey>` query param | `Authorization: Bearer <key>` header |
| Generate | `POST /{model}:generateContent` | `POST /chat/completions` |
| Generate body | `contents[]`, `systemInstruction`, `generationConfig.responseMimeType` | `messages[]` (system+user), `response_format:{type:"json_object"}`, `temperature`, `max_tokens` |
| Generate response | `candidates[0].content.parts[0].text` | `choices[0].message.content` |
| Describe | inline `inline_data:{mime_type,data}` | user `content:[{type:"text"},{type:"image_url",image_url:{url:"data:<mime>;base64,<b64>"}}]` |
| Embed | `POST /{embModel}:embedContent` / `:batchEmbedContents`; body `taskType`, `outputDimensionality` | `POST /embeddings`; body `input`, `dimensions` — **drop `taskType`** |
| Embed response | `embedding.values` | `data[0].embedding` |

- Keep `doWithRetry` (429/5xx backoff is provider-agnostic); adapt error parsing to
  OpenRouter's shape (`error` object / `choices`).
- Base URL becomes a constant (or config field) rather than hardcoded to Google.

### 4.2 Config / settings
Values change; schema does not. Model IDs get the `google/` namespace prefix.

| Setting | From | To |
|---|---|---|
| `gemini_api_key` | Google `AIza…` | OpenRouter `sk-or-…` |
| `gemini_model` | `gemini-2.5-flash` | `google/gemini-2.5-flash` |
| `graph_gemini_model` | `gemini-3.5-flash` | `google/gemini-3.5-flash` |
| `gemini_embedding_model` | `gemini-embedding-001` | `google/gemini-embedding-001` |

`internal/graph/handlers/embedding_options.go`: drop `TaskType` from `graphEmbeddingOptions()`
(OpenRouter ignores it; keeping dims 3072).

### 4.3 Data migration
- **Core memory** (`observations`, `user_prompts`, `session_summaries`, 126,625 vectors):
  untouched — verified byte-compatible (cosine 1.0). No schema change; dims stay 768.
- **Graph** (`graph.artifact_index`, 23,433 vectors, `halfvec(3072)`): re-embed via the new
  client (dims 3072, no task_type). One-off backfill reusing the existing
  `cmd/agent-mem/migrate.go` `EmbedBatch` pattern. Cost ~$0.17, runtime a few minutes.
- No `vector`/`halfvec` column or index changes (dims unchanged).

### 4.4 Model routing & cost (parity — no quality change)
| Workload | Volume/mo | Model | ~Cost/mo |
|---|---|---|---|
| Observation extraction | 15,492 | `google/gemini-2.5-flash` | ~$19.6 |
| Flat summaries | 1,213 | `google/gemini-2.5-flash` | ~$2 |
| Graph summaries / judge / describe | ~2,000 | `google/gemini-3.5-flash` | ~$16 |
| Embeddings | ~42,000 calls (~3.9M tok) | `google/gemini-embedding-001` | ~$0.6 |
| **Total** | | | **~$38/mo** |

Budget is $50/mo (can flex +$10–20). Cost lever recorded for later, not adopted now: moving
observation extraction to `gemini-2.5-flash-lite` drops the total to ~$22/mo (slight trade on
observation narratives only; it is a per-call model id, switchable anytime).

### 4.5 Testing
- Unit test the client against a recorded OpenRouter response (generation + embedding shapes).
- E2E on the **`agentmem_test` scratch DB only** — never the live dev DB (truncates graph +
  fixtures sync to prod).
- Before cutover: spot-check a sample of graph search results old-vs-new to confirm recall
  holds after the graph re-embed.

### 4.6 Rollback & deploy
- **Rollback:** config flip back to the Google key + base URL. Old graph vectors retained
  (backed up) until the re-embed is verified in production.
- **Deploy:** build amd64 image locally → push GHCR → VPS pull-only (`make deploy`). No
  dashboard rebuild (no UI change). Never build on the VPS.

## 5. Design — Claude-Code-Remote

### 5.1 Change
Replace the 3 `@google/generative-ai` call sites in `src/channels/slack/socket.js`
(lines ~1000, ~1107, ~1134 — describe attached file/image, summarize Slack thread, extract
project path) with a single shared helper:

```
openrouterComplete({ prompt | messages, model }) ->
  POST https://openrouter.ai/api/v1/chat/completions
  Authorization: Bearer OPENROUTER_API_KEY
  model default: google/gemini-2.5-flash
```
**Model note:** the current `gemini-2.0-flash` is **not available on OpenRouter**, so a model
change is required regardless. Chosen: `google/gemini-2.5-flash` — a large upgrade over 2.0,
multimodal (vision for the file-describe call), low latency for an interactive bot, ~$1/mo at
this volume. It supports reasoning; **cap reasoning effort to minimal/off** so the bot stays
snappy (a user waits on these responses in Slack). 3.5-flash was considered but rejected here
for added thinking latency; cost was not the deciding factor (volume is low, ~$1-3/mo either
way).
- Use Node's built-in `fetch` — **no new npm dependency**.
- **Mixed file types** (call `_fetchFileContents`): Gemini natively accepts any mimetype as
  binary `inlineData`, but OpenRouter's OpenAI-shaped API does not. The helper must branch:
  `image/*` → `image_url` data-URI part; text/code/log files → decode bytes to UTF-8 and
  inline as text; unsupported binary (e.g. non-image, non-text) → skip with a warning
  (matches current best-effort behavior). This is behavior, not a find-replace.

Call purposes (for reference):
- `_fetchFileContents` — describe/summarize files attached to a Slack message (vision for
  images, summary for code/text/logs); result injected into the prompt Claude receives.
- `_summarizeThreadContext` — concise text summary of a Slack thread for Claude context.
- `_detectProjectFromThread` — infer the project directory/cwd from the thread to resume the
  session in the right place.

### 5.2 Config
- Add `OPENROUTER_API_KEY` (and optional `OPENROUTER_MODEL`, default `google/gemini-2.5-flash`)
  to `.env` / `.env.example`.
- `GEMINI_API_KEY` retired for these 3 paths.

### 5.3 Out of scope (restated)
- `daily-summary.js` (Claude Agent SDK / Anthropic) — unchanged.
- Gemini CLI worker — owner replaces with `opencode` separately.

### 5.4 Testing & rollback
- Exercise all 3 features against OpenRouter: image/file describe, thread summarize, path
  extract.
- **Rollback:** revert env var / helper; the old inline `@google/generative-ai` path is a
  single-commit revert.

## 6. Open items
- Confirm the OpenRouter key/account model: one shared key for both repos vs one per repo
  (operational preference; no design impact).
- agent-mem package/name (`internal/gemini`) kept as-is to minimize diff; optional rename in
  a follow-up.

## 7. Non-goals
- No embedding-model change. No switch to OpenAI/Voyage/Qwen embeddings.
- No adoption of `gemini-embedding-2` (incompatible vector space).
- No `gemini-3.6-flash` (thinking model; extra billed tokens + JSON-truncation risk) unless
  revisited with explicit thinking-budget handling.
