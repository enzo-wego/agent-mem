# Handoff: make llm-gateway agent-mem's only LLM egress

**Branch:** `chore/agent-mem-gateway-only` (WIP committed at `0c50de6`, based on `main` @ `b80761b`)
**Goal (user's words):** *"everything related need to call openrouter will move to llm-gateway… this project only process memory logic — transfer call to llm-gateway, receive data, process logic and save to db, that's all"*

## State right now

```
go build ./...   PASSES
go vet ./...     FAILS — only internal/config/config_test.go:20 (undefined: SplitKeys)
```

`main` is clean; all work is on this branch. Nothing is deployed from it.

**Do not touch llm-gateway's model/backend/fallback config.** The user handles seat-limit
optimisation in that project. The one exception already made: `app/openrouter.py` gained
`key_usage()` and `app/main.py` gained `GET /usage` — both committed here as part of the
budget-widget move. Those still need to be committed *in the llm-gateway repo* (see step 6).

## Done

- Deleted `internal/gemini/client.go` + `client_test.go` (749 + 373 lines of OpenRouter/Google transport).
  Kept `prompts.go` (memory logic) and added `types.go` holding only `EmbedOptions`.
- `internal/llmgateway/client.go` is now the whole LLM surface: `Generate`, `GenerateCheap`,
  `Embed`, `EmbedWithOptions`, `EmbedBatch`, `Describe`, plus typed errors
  (`StatusError`, `ErrUnreachable`, `IsRetryable`).
- `GeminiAdapter` reduced to a swappable holder of one client. `geminiDirect` gone.
- `internal/config`: removed `GeminiAPIKey`, `GeminiModel`, `GraphGeminiModel`,
  `GeminiEmbeddingModel`, `LLMProvider`, `GoogleAPIKeys`, `LLMKeyRotateHours`, plus
  `SplitKeys`, `normalizeProvider`, `ActiveLLMKey(s)`, `LLMKeyRotateInterval`,
  `LLMProviderOrDefault`, and all provider env reads. Kept `GeminiEmbeddingDims`,
  `LLMGatewayURL`, `LLMGatewayAPIKey`.
- `internal/worker`: dropped `Server.gemini`, `getGemini`, `newLLMClient`, `maskKeyList`,
  `handleGetLLMKeys`, `handleUnblockLLMKey`, and the `/api/llm-keys` routes.
  `reloadGemini` → `reloadLLM`.
- OpenRouter budget widget now proxies llm-gateway `GET /usage` instead of reading a key.
- `cmd/agent-mem` (`migrate-sqlite`, `backfill-embeddings`) and `cmd/reembed-graph`
  repointed at the gateway.
- Eval tests (`topic_judge_eval_test.go`, `search/eval_test.go`) repointed at the gateway.

## Remaining work

### 1. Fix `internal/config/config_test.go` (blocks everything else)
Delete the tests covering removed behaviour — `SplitKeys`, provider normalisation, key
rotation, `ActiveLLMKey(s)`. Keep and update anything covering `GeminiEmbeddingDims`,
`LLMGatewayURL`/`LLMGatewayAPIKey` round-tripping, and `processing_paused`
(`TestProcessingPausedRoundTrip` must survive).

### 2. Delete the key-block machinery
- `internal/database/llm_keys.go` and `llm_keys_smoke_test.go` — no keys left to block.
- Grep for `ActiveLLMKeyBlocks`, `ListLLMKeyBlocks`, `UnblockLLMKey`, `RecordLLMKeyBlock`.
- Migration to `DROP TABLE IF EXISTS llm_key_blocks`.

### 3. Migration to delete dead settings rows
`gemini_api_key`, `gemini_model`, `graph_gemini_model`, `gemini_embedding_model`,
`llm_provider`, `google_api_keys`, `llm_key_rotate_hours`.
Keep `gemini_embedding_dims`, `llm_gateway_url`, `llm_gateway_api_key`.
Empty `Down` — restoring them would recreate a direct-provider path.

**Note:** the OpenRouter key currently in `settings.gemini_api_key` (`…26ac`) also lives in
llm-gateway's `.env` as `OPENROUTER_API_KEY`. Confirm that before deleting the row.

### 4. Dashboard
- `dashboard/src/api.ts`: drop `gemini_api_key`, `gemini_model`, `graph_gemini_model`,
  `gemini_embedding_model`, `llm_provider`, `google_api_keys`, `llm_key_rotate_hours`
  from the settings type; drop the `llm-keys` fetchers.
- `dashboard/src/pages/Settings.tsx`: remove the provider section, the API-key fields, the
  Google-keys textarea, the rotation control, and the LLM-keys panel. Keep the
  `llm-gateway` section and embedding dims.
- **Add a read-only gateway status panel** (promised to the user): fetch the worker's
  proxy of gateway `/health` — backend per tier, models, seat availability. Editing gateway
  config stays in llm-gateway; this is visibility only.
- Rebuild the embedded bundle — there is **no Makefile step**:
  ```
  cd dashboard && npm run build
  cd .. && rm -rf internal/worker/dashboard && cp -R dashboard/dist internal/worker/dashboard
  ```

### 5. README
Update the provider table: agent-mem holds no keys, no model names; the gateway is the sole
egress. Keep the existing notes on the 768/3072 split, the timeout chain
(`gateway 180 < client 200 < lease 240`), and the transient-requeue rule.

### 6. llm-gateway repo
`app/openrouter.py` (`key_usage()`) and `app/main.py` (`GET /usage`) were edited in the
working tree at `~/go/src/github.com/llm-gateway` but **not committed**. Commit, push, and
`sudo systemctl restart llm-gateway` on the VPS so `/usage` exists — the dashboard budget
widget 404s until then.

## Verify before declaring done

```bash
unset DATABASE_URL          # integration tests skip without it; they TRUNCATE if set
go build ./... && go vet ./... && go test ./...
grep -rn "openrouter\.ai\|generativelanguage\|api.anthropic.com" --include=*.go . | grep -v _test
#   ^ must return NOTHING: no provider endpoint may remain in agent-mem
```

Four failures in `internal/hooks` and `internal/skills` are **pre-existing** on clean `main`
(filed P3) — unrelated test-lag from an install-path change. Everything else must pass.

## Deploy

```bash
make deploy      # builds amd64 locally → GHCR → VPS pulls. NEVER build on the VPS.
```
Worker auto-runs migrations on boot. Both machines are currently
`processing_paused = true` — **leave them paused**; unpausing is the user's call.

## Landmines

- **Typed nil.** `flatLLMFor` / `newGraphGateway` must return a nil *interface*, never a
  typed nil pointer inside one. Handlers gate on `deps.Gemini == nil`; a typed nil passes
  that check and panics on first call.
- **Embedding widths are not interchangeable.** `observations.embedding` is `vector(768)`,
  `graph.artifact_index.embedding` is `halfvec(3072)`. Two gateway clients, one per width.
  Wrong width fails every insert with `expected 768 dimensions, not 3072`, which reads like
  a schema fault.
- **Never send `task_type` on embeddings.** Stored vectors were produced without one;
  adding it moves queries into a different vector space and search silently degrades.
- **Transient failures must requeue, not fail.** `pending_messages.status='failed'` is
  terminal and `ClaimPendingMessage` only selects `'pending'`. Use `llmgateway.IsRetryable`
  + `RequeuePendingMessage`. Anything new that can fail transiently follows this rule.
- `ssh … docker compose exec` is blocked by the permission classifier. Read-only `psql`
  over ssh is fine. VPS db creds: `agentmem/agentmem`, host port **5433** (not 5432).
