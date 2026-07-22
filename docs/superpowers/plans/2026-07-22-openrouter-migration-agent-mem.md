# agent-mem → OpenRouter Migration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Route all of agent-mem's Gemini API usage (generation + embeddings) through OpenRouter's OpenAI-compatible API, keeping the same `gemini-embedding-001` model so existing vectors stay valid.

**Architecture:** Rewrite `internal/gemini/client.go` internals to call OpenRouter (`https://openrouter.ai/api/v1`, `Authorization: Bearer`, OpenAI request/response shapes) while keeping every exported method signature identical, so the adapter and all call sites are untouched. Core-memory vectors are byte-compatible (verified cosine 1.0) and untouched; only the 23k `graph.artifact_index` vectors are re-embedded (they used a `task_type` hint OpenRouter can't send).

**Tech Stack:** Go, pgx/pgxpool, pgvector-go, zerolog, `net/http` + `net/http/httptest` for tests.

**Reference spec:** `docs/superpowers/specs/2026-07-22-openrouter-migration-design.md`

---

## File Structure

- `internal/gemini/client.go` — MODIFY (full rewrite of internals; same exported API).
- `internal/gemini/client_test.go` — MODIFY (white-box tests against `httptest` server).
- `internal/graph/handlers/embedding_options.go` — MODIFY (drop `TaskType`).
- `internal/config/config.go` — MODIFY (default model IDs get `google/` prefix).
- `cmd/reembed-graph/main.go` — CREATE (one-shot backfill for `graph.artifact_index`).

Out of scope: DB schema (dims unchanged 768/3072), the adapter, and all ~19 call sites.

---

## Task 1: Rewrite the client to OpenRouter (generation + embeddings)

**Files:**
- Modify: `internal/gemini/client.go`
- Test: `internal/gemini/client_test.go`

- [ ] **Step 1: Write failing tests**

Replace the contents of `internal/gemini/client_test.go` with:

```go
package gemini

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGenerateOpenRouter(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("auth header = %q, want Bearer test-key", got)
		}
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %q, want /chat/completions", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"ok\":true}"}}]}`))
	}))
	defer srv.Close()

	c := NewClient("test-key", "google/gemini-2.5-flash", "google/gemini-embedding-001", 768)
	c.baseURL = srv.URL

	out, err := c.Generate(context.Background(), "sys", "hello")
	if err != nil {
		t.Fatal(err)
	}
	if out != `{"ok":true}` {
		t.Errorf("Generate returned %q", out)
	}
	if gotBody["model"] != "google/gemini-2.5-flash" {
		t.Errorf("model = %v", gotBody["model"])
	}
	if _, ok := gotBody["messages"]; !ok {
		t.Error("request must use OpenAI messages[]")
	}
}

func TestEmbedOpenRouter(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embeddings" {
			t.Errorf("path = %q, want /embeddings", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		_, _ = w.Write([]byte(`{"data":[{"embedding":[0.1,0.2,0.3]}]}`))
	}))
	defer srv.Close()

	c := NewClient("test-key", "m", "google/gemini-embedding-001", 768)
	c.baseURL = srv.URL

	vec, err := c.Embed(context.Background(), "hello")
	if err != nil {
		t.Fatal(err)
	}
	if len(vec) != 3 {
		t.Fatalf("embedding len = %d, want 3", len(vec))
	}
	if gotBody["dimensions"].(float64) != 768 {
		t.Errorf("dimensions = %v, want 768", gotBody["dimensions"])
	}
	if _, ok := gotBody["task_type"]; ok {
		t.Error("task_type must NOT be sent to OpenRouter")
	}
	if gotBody["model"] != "google/gemini-embedding-001" {
		t.Errorf("model = %v", gotBody["model"])
	}
}

func TestEmbedWithOptionsDims(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		_, _ = w.Write([]byte(`{"data":[{"embedding":[0.1]}]}`))
	}))
	defer srv.Close()

	c := NewClient("k", "m", "google/gemini-embedding-001", 768)
	c.baseURL = srv.URL

	_, err := c.EmbedWithOptions(context.Background(), "x", EmbedOptions{OutputDimensionality: 3072})
	if err != nil {
		t.Fatal(err)
	}
	if gotBody["dimensions"].(float64) != 3072 {
		t.Errorf("dimensions = %v, want 3072 (per-call override)", gotBody["dimensions"])
	}
}
```

- [ ] **Step 2: Run tests to verify they fail to compile**

Run: `go test ./internal/gemini/ -run TestGenerateOpenRouter -v`
Expected: FAIL — build error (`c.baseURL` undefined; old client is Google-shaped).

- [ ] **Step 3: Rewrite `internal/gemini/client.go`**

Replace the entire file with:

```go
package gemini

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"time"

	"github.com/rs/zerolog/log"
)

const defaultBaseURL = "https://openrouter.ai/api/v1"

// Client is an OpenRouter (OpenAI-compatible) client for generation and embedding.
type Client struct {
	apiKey         string
	model          string
	embeddingModel string
	embeddingDims  int
	baseURL        string
	httpClient     *http.Client
}

// NewClient creates a new OpenRouter API client. model/embeddingModel are
// OpenRouter model ids (e.g. "google/gemini-2.5-flash", "google/gemini-embedding-001").
func NewClient(apiKey, model, embeddingModel string, embeddingDims int) *Client {
	return &Client{
		apiKey:         apiKey,
		model:          model,
		embeddingModel: embeddingModel,
		embeddingDims:  embeddingDims,
		baseURL:        defaultBaseURL,
		httpClient:     &http.Client{Timeout: 60 * time.Second},
	}
}

// --- Chat (generation) ---

type chatMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"` // string, or []contentPart for multimodal
}

type contentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
}

type imageURL struct {
	URL string `json:"url"`
}

type responseFormat struct {
	Type string `json:"type"`
}

type chatRequest struct {
	Model          string          `json:"model"`
	Messages       []chatMessage   `json:"messages"`
	Temperature    float64         `json:"temperature"`
	MaxTokens      int             `json:"max_tokens"`
	ResponseFormat *responseFormat `json:"response_format,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error *apiError `json:"error,omitempty"`
}

type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Generate sends a prompt to OpenRouter and returns the response text.
// response_format=json_object forces valid JSON output (parity with the old client).
func (c *Client) Generate(ctx context.Context, systemPrompt, userMessage string) (string, error) {
	msgs := make([]chatMessage, 0, 2)
	if systemPrompt != "" {
		msgs = append(msgs, chatMessage{Role: "system", Content: systemPrompt})
	}
	msgs = append(msgs, chatMessage{Role: "user", Content: userMessage})

	req := chatRequest{
		Model:          c.model,
		Messages:       msgs,
		Temperature:    0.3,
		MaxTokens:      4096,
		ResponseFormat: &responseFormat{Type: "json_object"},
	}

	var resp chatResponse
	if err := c.doWithRetry(ctx, "/chat/completions", req, &resp); err != nil {
		return "", fmt.Errorf("generate: %w", err)
	}
	if resp.Error != nil {
		return "", fmt.Errorf("openrouter API error: %s (code %d)", resp.Error.Message, resp.Error.Code)
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("empty response from OpenRouter")
	}
	return resp.Choices[0].Message.Content, nil
}

// Describe sends an image attachment to OpenRouter and returns a prose
// description, OCR text, and key entities as three fields.
func (c *Client) Describe(ctx context.Context, mimeType string, data []byte, prompt string) (string, string, []string, error) {
	dataURI := fmt.Sprintf("data:%s;base64,%s", mimeType, base64.StdEncoding.EncodeToString(data))
	parts := []contentPart{
		{Type: "text", Text: prompt + `\nRespond as JSON only: {"description":"...","ocr":"verbatim visible text","entities":["..."]}`},
		{Type: "image_url", ImageURL: &imageURL{URL: dataURI}},
	}
	req := chatRequest{
		Model:          c.model,
		Messages:       []chatMessage{{Role: "user", Content: parts}},
		Temperature:    0.2,
		MaxTokens:      2048,
		ResponseFormat: &responseFormat{Type: "json_object"},
	}

	var resp chatResponse
	if err := c.doWithRetry(ctx, "/chat/completions", req, &resp); err != nil {
		return "", "", nil, fmt.Errorf("describe: %w", err)
	}
	if resp.Error != nil {
		return "", "", nil, fmt.Errorf("openrouter describe error: %s (code %d)", resp.Error.Message, resp.Error.Code)
	}
	if len(resp.Choices) == 0 {
		return "", "", nil, fmt.Errorf("empty describe response from OpenRouter")
	}

	var parsed struct {
		Description string   `json:"description"`
		OCR         string   `json:"ocr"`
		Entities    []string `json:"entities"`
	}
	if err := json.Unmarshal([]byte(resp.Choices[0].Message.Content), &parsed); err != nil {
		return "", "", nil, fmt.Errorf("describe: parse JSON: %w", err)
	}
	return parsed.Description, parsed.OCR, parsed.Entities, nil
}

// --- Embedding ---

type embedRequest struct {
	Model      string `json:"model"`
	Input      any    `json:"input"` // string (single) or []string (batch)
	Dimensions int    `json:"dimensions,omitempty"`
}

type embedResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Error *apiError `json:"error,omitempty"`
}

// EmbedOptions configures a single embedding request. TaskType and Title are
// retained for API stability but IGNORED — OpenRouter's OpenAI-shaped embeddings
// API does not accept them.
type EmbedOptions struct {
	Title                string
	TaskType             string
	OutputDimensionality int
}

// Embed generates a single embedding vector using the client's default dims.
func (c *Client) Embed(ctx context.Context, text string) ([]float32, error) {
	return c.EmbedWithOptions(ctx, text, EmbedOptions{OutputDimensionality: c.embeddingDims})
}

// EmbedWithOptions generates a single embedding vector. OutputDimensionality
// overrides the client default (graph indexing uses 3072).
func (c *Client) EmbedWithOptions(ctx context.Context, text string, opts EmbedOptions) ([]float32, error) {
	dims := opts.OutputDimensionality
	if dims == 0 {
		dims = c.embeddingDims
	}
	req := embedRequest{Model: c.embeddingModel, Input: text, Dimensions: dims}

	var resp embedResponse
	if err := c.doWithRetry(ctx, "/embeddings", req, &resp); err != nil {
		return nil, fmt.Errorf("embed: %w", err)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("embed API error: %s (code %d)", resp.Error.Message, resp.Error.Code)
	}
	if len(resp.Data) == 0 {
		return nil, fmt.Errorf("empty embedding response")
	}
	return resp.Data[0].Embedding, nil
}

// EmbedBatch generates embeddings for multiple texts in one request, at the
// client's default dims.
func (c *Client) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	req := embedRequest{Model: c.embeddingModel, Input: texts, Dimensions: c.embeddingDims}

	var resp embedResponse
	if err := c.doWithRetry(ctx, "/embeddings", req, &resp); err != nil {
		return nil, fmt.Errorf("batch embed: %w", err)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("batch embed API error: %s (code %d)", resp.Error.Message, resp.Error.Code)
	}
	results := make([][]float32, len(resp.Data))
	for i, d := range resp.Data {
		results[i] = d.Embedding
	}
	return results, nil
}

// --- HTTP with retry ---

// doWithRetry POSTs to baseURL+path with Bearer auth and exponential backoff on 429/5xx.
func (c *Client) doWithRetry(ctx context.Context, path string, body any, result any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}
	url := c.baseURL + path

	maxRetries := 5
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(math.Pow(2, float64(attempt-1))) * time.Second
			log.Debug().Int("attempt", attempt).Dur("backoff", backoff).Msg("Retrying OpenRouter request")
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
		if err != nil {
			return fmt.Errorf("create request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+c.apiKey)

		resp, err := c.httpClient.Do(req)
		if err != nil {
			if attempt == maxRetries {
				return fmt.Errorf("http request failed after %d retries: %w", maxRetries, err)
			}
			continue
		}

		respBody, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return fmt.Errorf("read response: %w", err)
		}

		if resp.StatusCode == 429 || resp.StatusCode >= 500 {
			if attempt == maxRetries {
				return fmt.Errorf("openrouter API returned %d after %d retries: %s", resp.StatusCode, maxRetries, string(respBody))
			}
			log.Warn().Int("status", resp.StatusCode).Int("attempt", attempt).Msg("OpenRouter API error, retrying")
			continue
		}

		if resp.StatusCode != 200 {
			return fmt.Errorf("openrouter API returned %d: %s", resp.StatusCode, string(respBody))
		}

		if err := json.Unmarshal(respBody, result); err != nil {
			return fmt.Errorf("unmarshal response: %w", err)
		}
		return nil
	}
	return fmt.Errorf("exhausted retries")
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/gemini/ -v`
Expected: PASS (all three tests).

- [ ] **Step 5: Build the whole module**

Run: `go build ./...`
Expected: no errors (adapter and call sites compile unchanged against the same method signatures).

- [ ] **Step 6: Commit**

```bash
git add internal/gemini/client.go internal/gemini/client_test.go
git commit -m "feat(gemini): route generation + embeddings through OpenRouter (OpenAI-compatible)"
```

---

## Task 2: Drop the graph task_type; update default model IDs

**Files:**
- Modify: `internal/graph/handlers/embedding_options.go`
- Modify: `internal/config/config.go:371-372` (defaults)
- Test: `internal/gemini/client_test.go` (already asserts `task_type` is not sent — Task 1)

- [ ] **Step 1: Edit `embedding_options.go`**

Replace the file body with (drop `graphEmbeddingTaskType` and the `TaskType` field):

```go
package handlers

import "github.com/agent-mem/agent-mem/internal/gemini"

const graphEmbeddingDims = 3072

// graphEmbeddingOptions selects 3072-dim embeddings for graph indexing. No
// task_type: OpenRouter's embeddings API does not accept it, so we must not
// depend on it (docs and queries both go without it, keeping the space consistent).
func graphEmbeddingOptions() gemini.EmbedOptions {
	return gemini.EmbedOptions{
		OutputDimensionality: graphEmbeddingDims,
	}
}
```

- [ ] **Step 2: Edit config defaults `internal/config/config.go`**

Change lines 371-372 from:

```go
		GeminiModel:          "gemini-2.5-flash",
		GeminiEmbeddingModel: "gemini-embedding-001",
```

to:

```go
		GeminiModel:          "google/gemini-2.5-flash",
		GeminiEmbeddingModel: "google/gemini-embedding-001",
```

- [ ] **Step 3: Build + test**

Run: `go build ./... && go test ./internal/gemini/ ./internal/graph/handlers/ -run 'OpenRouter|Embedding' -v`
Expected: PASS, no build errors.

- [ ] **Step 4: Commit**

```bash
git add internal/graph/handlers/embedding_options.go internal/config/config.go
git commit -m "feat(graph): drop unsupported task_type; default to google/ model ids"
```

---

## Task 3: One-shot graph re-embed command

**Files:**
- Create: `cmd/reembed-graph/main.go`

Re-embeds `graph.artifact_index.embedding` (23,433 rows, `halfvec(3072)`) through OpenRouter
`google/gemini-embedding-001` at 3072 dims, no task_type — matching what the deployed worker
now produces for queries. Mirrors the batch pattern in `cmd/agent-mem/migrate.go`.

- [ ] **Step 1: Create `cmd/reembed-graph/main.go`**

```go
package main

import (
	"context"
	"os"
	"time"

	"github.com/agent-mem/agent-mem/internal/gemini"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

func main() {
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.RFC3339})
	ctx := context.Background()

	dsn := os.Getenv("DATABASE_URL")
	apiKey := os.Getenv("AGENT_MEM_GEMINI_API_KEY")
	if apiKey == "" {
		apiKey = os.Getenv("GEMINI_API_KEY")
	}
	if dsn == "" || apiKey == "" {
		log.Fatal().Msg("DATABASE_URL and AGENT_MEM_GEMINI_API_KEY (OpenRouter key) are required")
	}

	pg, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Fatal().Err(err).Msg("connect pg")
	}
	defer pg.Close()

	// 3072-dim client, no task_type — matches the worker's graph query embeddings.
	client := gemini.NewClient(apiKey, "", "google/gemini-embedding-001", 3072)

	rows, err := pg.Query(ctx, `SELECT node_id, summary FROM graph.artifact_index WHERE summary <> '' ORDER BY node_id`)
	if err != nil {
		log.Fatal().Err(err).Msg("select artifact_index")
	}
	var ids, texts []string
	for rows.Next() {
		var id, summary string
		if err := rows.Scan(&id, &summary); err != nil {
			continue
		}
		ids = append(ids, id)
		texts = append(texts, summary)
	}
	rows.Close()

	log.Info().Int("count", len(texts)).Msg("Re-embedding graph.artifact_index via OpenRouter")

	for i := 0; i < len(texts); i += 100 {
		end := i + 100
		if end > len(texts) {
			end = len(texts)
		}
		embeddings, err := client.EmbedBatch(ctx, texts[i:end])
		if err != nil {
			log.Warn().Err(err).Int("batch_start", i).Msg("Batch embed failed; skipping batch")
			continue
		}
		for j, emb := range embeddings {
			v := pgvector.NewVector(emb)
			if _, err := pg.Exec(ctx,
				`UPDATE graph.artifact_index SET embedding = $1, refreshed_at = NOW() WHERE node_id = $2`,
				&v, ids[i+j]); err != nil {
				log.Warn().Err(err).Str("node_id", ids[i+j]).Msg("update failed")
			}
		}
		log.Info().Int("progress", end).Int("total", len(texts)).Msg("progress")
	}
	log.Info().Msg("Done")
}
```

- [ ] **Step 2: Build the command**

Run: `go build ./cmd/reembed-graph/`
Expected: no errors. (Do NOT run it yet — it mutates data; it runs during cutover, Task 5.)

- [ ] **Step 3: Commit**

```bash
git add cmd/reembed-graph/main.go
git commit -m "feat(cmd): add reembed-graph one-shot backfill for artifact_index"
```

---

## Task 4: Verify on the scratch DB (never the live DB)

Per project rule: handler/e2e runs use the `agentmem_test` scratch DB only — never the live
dev DB (it truncates the graph and fixtures sync to prod).

- [ ] **Step 1: Point tests/config at the scratch DB and a real OpenRouter key**

```bash
export DATABASE_URL='postgres://agentmem:agentmem@localhost:5432/agentmem_test'
export AGENT_MEM_GEMINI_API_KEY="$(cat ~/.openrouter_key)"
export AGENT_MEM_GEMINI_MODEL='google/gemini-2.5-flash'
```

- [ ] **Step 2: Run the full unit suite**

Run: `go test ./... 2>&1 | tail -30`
Expected: PASS (or pre-existing unrelated failures only — compare against `main`).

- [ ] **Step 3: Confirm a live round-trip against OpenRouter**

Run this one-off Go check (uses the exported client + real key) to prove generation and a
3072-dim embedding both succeed against OpenRouter before touching prod:

```bash
cat > /tmp/or_live_check.go <<'EOF'
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/agent-mem/agent-mem/internal/gemini"
)

func main() {
	key := os.Getenv("AGENT_MEM_GEMINI_API_KEY")
	c := gemini.NewClient(key, "google/gemini-2.5-flash", "google/gemini-embedding-001", 3072)
	out, err := c.Generate(context.Background(), "", `Reply as JSON: {"ok":true}`)
	fmt.Println("generate:", out, err)
	v, err := c.Embed(context.Background(), "hello world")
	fmt.Println("embed dims:", len(v), err)
}
EOF
go run /tmp/or_live_check.go && rm /tmp/or_live_check.go
```
Expected: `generate: {"ok":true} <nil>` and `embed dims: 3072 <nil>`.

- [ ] **Step 4: Commit (if any test fixtures changed)**

```bash
git add -A && git commit -m "test: verify OpenRouter client on scratch DB" || echo "nothing to commit"
```

---

## Task 5: Cutover on the VPS (production)

Deploy is pull-only on the VPS: build amd64 locally → push GHCR → VPS pulls (`make deploy`).
Never build on the VPS.

- [ ] **Step 1: Back up the graph embeddings (rollback safety)**

```bash
ssh enzo@enzogo.io.vn "cd /var/go/src/github.com/agent-mem && sudo docker compose exec -T postgres \
  pg_dump -U agentmem -d agentmem -t graph.artifact_index --data-only -Fc > /tmp/artifact_index_backup.dump"
```

- [ ] **Step 2: Update the production settings to OpenRouter values**

Paste the OpenRouter key without echoing it into history (use the `! ` prompt prefix, or run on
the box). Update all four settings:

```bash
ssh enzo@enzogo.io.vn "cd /var/go/src/github.com/agent-mem && sudo docker compose exec -T postgres \
  psql -U agentmem -d agentmem -c \"UPDATE settings SET value='OPENROUTER_KEY_HERE' WHERE key='gemini_api_key';\" \
  -c \"UPDATE settings SET value='google/gemini-2.5-flash' WHERE key='gemini_model';\" \
  -c \"UPDATE settings SET value='google/gemini-3.5-flash' WHERE key='graph_gemini_model';\" \
  -c \"UPDATE settings SET value='google/gemini-embedding-001' WHERE key='gemini_embedding_model';\""
```

- [ ] **Step 3: Build, push, deploy the new worker**

Run (locally): `make deploy`
Expected: image built amd64, pushed to GHCR, VPS pulls and restarts `worker`. The worker
hot-reloads the client from settings on startup (`internal/worker/settings_handlers.go`).

- [ ] **Step 4: Smoke-test generation + embedding in prod**

```bash
# Watch worker logs for a successful generation/embedding after a new event, no auth/4xx errors:
ssh enzo@enzogo.io.vn "cd /var/go/src/github.com/agent-mem && sudo docker compose logs --tail=50 worker"
```
Expected: no `openrouter API returned 401/404` errors; observations/summaries process normally.

- [ ] **Step 5: Run the graph re-embed backfill (prod)**

```bash
# Build the one-shot binary for amd64 and run it on the box, or run via a one-off compose exec.
# Reads DATABASE_URL + AGENT_MEM_GEMINI_API_KEY (OpenRouter key) from the worker's env/settings.
ssh enzo@enzogo.io.vn "cd /var/go/src/github.com/agent-mem && \
  DATABASE_URL='postgres://agentmem:agentmem@localhost:5432/agentmem' \
  AGENT_MEM_GEMINI_API_KEY='OPENROUTER_KEY_HERE' \
  ./bin/reembed-graph"
```
Expected: logs `Re-embedding graph.artifact_index ... count=~23433`, progresses to done in minutes.

- [ ] **Step 6: Verify graph search recall**

Query a few known topics on `/live` search (or the search API) and confirm results look
correct vs. before. If recall looks wrong, roll back (below).

- [ ] **Step 7: Retire the Google key**

Once generation, embeddings, and graph search are all confirmed healthy in prod, revoke the old
Google `AIza…` key in the Google console. Keep the backup dump from Step 1 for a few days.

**Rollback:** redeploy the previous worker image tag (`sudo docker compose pull` a prior tag or
`git revert` + `make deploy`), restore the four `settings` values to their Google originals, and
if the backfill ran, restore `graph.artifact_index` from `/tmp/artifact_index_backup.dump`
(`pg_restore --data-only -t graph.artifact_index`). The previous image speaks Google-native, so
it needs the Google key + Google model ids back.

---

## Self-Review Notes
- Spec coverage: client rewrite (T1), task_type drop + model ids (T2), graph re-embed (T3),
  scratch-DB testing (T4), cutover/rollback/deploy (T5). Core-memory vectors intentionally
  untouched (verified cosine 1.0) — no task for them, by design.
- `EmbedOptions.TaskType`/`Title` retained as ignored fields for API stability; only
  `graphEmbeddingOptions` stops setting `TaskType`.
- Method signatures unchanged → adapter and call sites need no edits (confirmed by `go build ./...` in T1 Step 5).
