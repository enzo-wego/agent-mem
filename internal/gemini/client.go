package gemini

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

// Provider selects the LLM backend. Both speak the same gemini models; they
// differ only in wire protocol (endpoint, auth, request/response shape).
//   - openrouter (default): OpenAI-compatible API at openrouter.ai (sk-or… key).
//   - google: the direct Google Gemini REST API (AIza… key).
//
// A single toggle (config llm_provider) plus a worker restart switches every
// call — generation, describe, and embeddings. Embeddings stay in the SAME
// vector space across providers because both use gemini-embedding-001 with NO
// task_type (verified cosine ~1.0), so switching needs no re-embed.
const (
	ProviderOpenRouter = "openrouter"
	ProviderGoogle     = "google"

	openRouterBaseURL = "https://openrouter.ai/api/v1"
	googleBaseURL     = "https://generativelanguage.googleapis.com/v1beta/models"
)

// KeyBlockStore persists a key block so a quota-exhausted or rejected key stays
// out of rotation across restarts and is visible in the dashboard. Optional:
// without one, blocks live only in this process. until is the zero time for a
// block that never expires (rejected key).
type KeyBlockStore interface {
	BlockLLMKey(ctx context.Context, fingerprint, keyTail, provider, reason string, until time.Time) error
}

// Client is a dual-mode client for generation and embedding.
type Client struct {
	provider       string
	keys           []string
	rotate         time.Duration
	model          string
	embeddingModel string
	embeddingDims  int
	baseURL        string
	httpClient     *http.Client

	// blocked holds fingerprints of keys taken out of rotation (seeded from the
	// store at construction, extended as calls fail). Guards itself; clients are
	// shared across job goroutines.
	mu      sync.RWMutex
	blocked map[string]bool
	store   KeyBlockStore
}

// NewClient creates a client for the given provider. An empty/unknown provider
// defaults to OpenRouter. model/embeddingModel may be given with or without the
// "google/" namespace prefix; it is normalized per provider at call time.
func NewClient(provider, apiKey, model, embeddingModel string, embeddingDims int) *Client {
	return NewRotatingClient(provider, []string{apiKey}, 0, model, embeddingModel, embeddingDims)
}

// NewRotatingClient is NewClient over a pool of keys: every rotate window the
// client switches to another key from keys, spreading per-key quota. rotate <= 0
// (or a single key) pins keys[0] forever.
func NewRotatingClient(provider string, keys []string, rotate time.Duration, model, embeddingModel string, embeddingDims int) *Client {
	if provider != ProviderGoogle {
		provider = ProviderOpenRouter
	}
	baseURL := openRouterBaseURL
	if provider == ProviderGoogle {
		baseURL = googleBaseURL
	}
	return &Client{
		provider:       provider,
		keys:           keys,
		rotate:         rotate,
		model:          model,
		embeddingModel: embeddingModel,
		embeddingDims:  embeddingDims,
		baseURL:        baseURL,
		httpClient:     &http.Client{Timeout: 60 * time.Second},
		blocked:        map[string]bool{},
	}
}

// WithKeyBlocks wires block persistence and seeds the already-blocked
// fingerprints (loaded from the DB at startup). Returns c for chaining.
func (c *Client) WithKeyBlocks(store KeyBlockStore, blockedFingerprints []string) *Client {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.store = store
	for _, fp := range blockedFingerprints {
		c.blocked[fp] = true
	}
	return c
}

// Provider reports the active backend (for logging/diagnostics).
func (c *Client) Provider() string { return c.provider }

// ActiveFingerprint reports which pooled key this client would use right now.
func (c *Client) ActiveFingerprint() string { return Fingerprint(c.apiKey()) }

// Fingerprint identifies a key in logs and the DB without storing the secret.
func Fingerprint(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])[:16]
}

// keyTail is the display suffix for a key ("…aB3x"), enough to tell pool members apart.
func keyTail(key string) string {
	if len(key) <= 4 {
		return key
	}
	return key[len(key)-4:]
}

// apiKey returns the key for the current rotation window, skipping blocked keys.
// The window comes from the clock alone, so every goroutine — and every process
// sharing the same config — agrees on one key per window and switches only at the
// boundary.
// ponytail: the pick is with replacement, so a key can win two windows in a row;
// use a seeded permutation if strict round-robin ever matters.
func (c *Client) apiKey() string {
	live := c.liveKeys()
	if len(live) == 0 {
		return ""
	}
	if len(live) == 1 || c.rotate <= 0 {
		return live[0]
	}
	secs := max(int64(c.rotate/time.Second), 1)
	return pick(live, time.Now().Unix()/secs)
}

// pick maps a rotation window to a key. Split out so it can be exercised over
// many windows without moving the clock.
func pick(keys []string, window int64) string {
	return keys[mix(uint64(window))%uint64(len(keys))]
}

// liveKeys returns the unblocked keys, or all keys when every one is blocked —
// trying a blocked key beats failing every call outright (quota may have reset).
func (c *Client) liveKeys() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if len(c.blocked) == 0 {
		return c.keys
	}
	live := make([]string, 0, len(c.keys))
	for _, k := range c.keys {
		if !c.blocked[Fingerprint(k)] {
			live = append(live, k)
		}
	}
	if len(live) == 0 {
		return c.keys
	}
	return live
}

// blockKey takes key out of rotation and persists the block. It reports whether
// another key is left to retry with — false means don't bother swapping.
func (c *Client) blockKey(ctx context.Context, key, reason string, until time.Time) bool {
	fp := Fingerprint(key)

	c.mu.Lock()
	already := c.blocked[fp]
	c.blocked[fp] = true
	remaining := 0
	for _, k := range c.keys {
		if !c.blocked[Fingerprint(k)] {
			remaining++
		}
	}
	store := c.store
	c.mu.Unlock()

	if !already {
		log.Warn().Str("provider", c.provider).Str("key", "…"+keyTail(key)).Str("fingerprint", fp).
			Str("reason", reason).Time("until", until).Int("keys_left", remaining).Msg("LLM key blocked")
		if store != nil {
			// Fresh context: the caller's may already be cancelled by the failure.
			sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if err := store.BlockLLMKey(sctx, fp, keyTail(key), c.provider, reason, until); err != nil {
				log.Error().Err(err).Str("fingerprint", fp).Msg("Failed to persist LLM key block")
			}
		}
	}
	return remaining > 0
}

// blockReason classifies a failed response: a non-empty reason means the key
// itself is the problem, so rotate to the next one. until is zero for a
// permanent block (key rejected) or a wall-clock expiry for a quota block.
func blockReason(status int, body []byte) (reason string, until time.Time, ok bool) {
	msg := strings.ToLower(string(body))
	switch {
	case status == 429:
		// Daily/free-tier quota: clears at the provider's next reset window.
		return "quota exhausted (429)", time.Now().Add(24 * time.Hour), true
	case status == 402:
		return "out of credits (402)", time.Now().Add(24 * time.Hour), true
	case status == 401 || status == 403:
		return fmt.Sprintf("key rejected (%d)", status), time.Time{}, true
	case status == 400 && (strings.Contains(msg, "api_key_invalid") || strings.Contains(msg, "api key not valid")):
		return "key invalid (400)", time.Time{}, true
	}
	return "", time.Time{}, false
}

// mix is splitmix64 — spreads sequential window numbers so consecutive windows
// don't land on correlated indexes.
func mix(x uint64) uint64 {
	x += 0x9E3779B97F4A7C15
	x = (x ^ (x >> 30)) * 0xBF58476D1CE4E5B9
	x = (x ^ (x >> 27)) * 0x94D049BB133111EB
	return x ^ (x >> 31)
}

// modelID normalizes a model id for the active provider. On OpenRouter, ids that
// already carry a namespace (any "…/…", e.g. "anthropic/claude-haiku-4.5") pass
// through untouched so non-Google models work; bare ids get the "google/" prefix.
// On Google, the "google/" prefix is stripped — the REST API serves only bare
// Gemini ids (a non-Google id there will 404: accepted operator error).
func (c *Client) modelID(id string) string {
	if c.provider == ProviderOpenRouter {
		if strings.Contains(id, "/") {
			return id
		}
		return "google/" + id
	}
	return strings.TrimPrefix(id, "google/")
}

// --- OpenRouter (OpenAI-compatible) wire types ---

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

type orEmbedRequest struct {
	Model      string `json:"model"`
	Input      any    `json:"input"` // string (single) or []string (batch)
	Dimensions int    `json:"dimensions,omitempty"`
}

type orEmbedResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Error *apiError `json:"error,omitempty"`
}

// --- Google Gemini REST wire types ---

type gGenerateRequest struct {
	Contents          []gContent        `json:"contents"`
	SystemInstruction *gContent         `json:"systemInstruction,omitempty"`
	GenerationConfig  gGenerationConfig `json:"generationConfig"`
}

type gContent struct {
	Role  string  `json:"role,omitempty"`
	Parts []gPart `json:"parts"`
}

type gPart struct {
	Text       string       `json:"text,omitempty"`
	InlineData *gInlineData `json:"inline_data,omitempty"`
}

type gInlineData struct {
	MimeType string `json:"mime_type"`
	Data     string `json:"data"`
}

type gGenerationConfig struct {
	Temperature      float64 `json:"temperature"`
	MaxOutputTokens  int     `json:"maxOutputTokens"`
	ResponseMimeType string  `json:"responseMimeType,omitempty"`
}

type gGenerateResponse struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"content"`
	} `json:"candidates"`
	Error *apiError `json:"error,omitempty"`
}

type gEmbedContentConfig struct {
	Title                string `json:"title,omitempty"`
	TaskType             string `json:"taskType,omitempty"`
	OutputDimensionality int    `json:"outputDimensionality,omitempty"`
}

type gEmbedRequest struct {
	Model   string   `json:"model"`
	Content gContent `json:"content"`
	// v1beta REST honors ONLY these top-level fields (a nested config is silently
	// ignored); the nested copy is sent for forward-compat and always agrees.
	Title                string               `json:"title,omitempty"`
	TaskType             string               `json:"taskType,omitempty"`
	OutputDimensionality int                  `json:"outputDimensionality,omitempty"`
	EmbedContentConfig   *gEmbedContentConfig `json:"embedContentConfig,omitempty"`
}

type gEmbedResponse struct {
	Embedding *struct {
		Values []float32 `json:"values"`
	} `json:"embedding"`
	Error *apiError `json:"error,omitempty"`
}

type gBatchEmbedRequest struct {
	Requests []gEmbedRequest `json:"requests"`
}

type gBatchEmbedResponse struct {
	Embeddings []struct {
		Values []float32 `json:"values"`
	} `json:"embeddings"`
	Error *apiError `json:"error,omitempty"`
}

type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Status  string `json:"status,omitempty"`
}

// --- Generation ---

// Generate sends a prompt and returns the response text, forced to valid JSON.
func (c *Client) Generate(ctx context.Context, systemPrompt, userMessage string) (string, error) {
	if c.provider == ProviderGoogle {
		return c.generateGoogle(ctx, systemPrompt, userMessage)
	}
	return c.generateOpenRouter(ctx, systemPrompt, userMessage)
}

func (c *Client) generateOpenRouter(ctx context.Context, systemPrompt, userMessage string) (string, error) {
	msgs := make([]chatMessage, 0, 2)
	if systemPrompt != "" {
		msgs = append(msgs, chatMessage{Role: "system", Content: systemPrompt})
	}
	msgs = append(msgs, chatMessage{Role: "user", Content: userMessage})

	req := chatRequest{
		Model:          c.modelID(c.model),
		Messages:       msgs,
		Temperature:    0.3,
		MaxTokens:      4096,
		ResponseFormat: &responseFormat{Type: "json_object"},
	}
	var resp chatResponse
	if err := c.doPost(ctx, c.baseURL+"/chat/completions", req, &resp); err != nil {
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

func (c *Client) generateGoogle(ctx context.Context, systemPrompt, userMessage string) (string, error) {
	req := gGenerateRequest{
		Contents:         []gContent{{Role: "user", Parts: []gPart{{Text: userMessage}}}},
		GenerationConfig: gGenerationConfig{Temperature: 0.3, MaxOutputTokens: 4096, ResponseMimeType: "application/json"},
	}
	if systemPrompt != "" {
		req.SystemInstruction = &gContent{Parts: []gPart{{Text: systemPrompt}}}
	}
	url := fmt.Sprintf("%s/%s:generateContent", c.baseURL, c.modelID(c.model))
	var resp gGenerateResponse
	if err := c.doPost(ctx, url, req, &resp); err != nil {
		return "", fmt.Errorf("generate: %w", err)
	}
	if resp.Error != nil {
		return "", fmt.Errorf("gemini API error: %s (code %d)", resp.Error.Message, resp.Error.Code)
	}
	if len(resp.Candidates) == 0 || len(resp.Candidates[0].Content.Parts) == 0 {
		return "", fmt.Errorf("empty response from Gemini")
	}
	return resp.Candidates[0].Content.Parts[0].Text, nil
}

// Describe sends an image attachment and returns description, OCR, and entities.
func (c *Client) Describe(ctx context.Context, mimeType string, data []byte, prompt string) (string, string, []string, error) {
	promptJSON := prompt + `\nRespond as JSON only: {"description":"...","ocr":"verbatim visible text","entities":["..."]}`
	var raw string
	var err error
	if c.provider == ProviderGoogle {
		raw, err = c.describeGoogle(ctx, mimeType, data, promptJSON)
	} else {
		raw, err = c.describeOpenRouter(ctx, mimeType, data, promptJSON)
	}
	if err != nil {
		return "", "", nil, err
	}
	var parsed struct {
		Description string   `json:"description"`
		OCR         string   `json:"ocr"`
		Entities    []string `json:"entities"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return "", "", nil, fmt.Errorf("describe: parse JSON: %w", err)
	}
	return parsed.Description, parsed.OCR, parsed.Entities, nil
}

func (c *Client) describeOpenRouter(ctx context.Context, mimeType string, data []byte, promptJSON string) (string, error) {
	dataURI := fmt.Sprintf("data:%s;base64,%s", mimeType, base64.StdEncoding.EncodeToString(data))
	parts := []contentPart{
		{Type: "text", Text: promptJSON},
		{Type: "image_url", ImageURL: &imageURL{URL: dataURI}},
	}
	req := chatRequest{
		Model:          c.modelID(c.model),
		Messages:       []chatMessage{{Role: "user", Content: parts}},
		Temperature:    0.2,
		MaxTokens:      2048,
		ResponseFormat: &responseFormat{Type: "json_object"},
	}
	var resp chatResponse
	if err := c.doPost(ctx, c.baseURL+"/chat/completions", req, &resp); err != nil {
		return "", fmt.Errorf("describe: %w", err)
	}
	if resp.Error != nil {
		return "", fmt.Errorf("openrouter describe error: %s (code %d)", resp.Error.Message, resp.Error.Code)
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("empty describe response from OpenRouter")
	}
	return resp.Choices[0].Message.Content, nil
}

func (c *Client) describeGoogle(ctx context.Context, mimeType string, data []byte, promptJSON string) (string, error) {
	req := gGenerateRequest{
		Contents: []gContent{{
			Role: "user",
			Parts: []gPart{
				{Text: promptJSON},
				{InlineData: &gInlineData{MimeType: mimeType, Data: base64.StdEncoding.EncodeToString(data)}},
			},
		}},
		GenerationConfig: gGenerationConfig{Temperature: 0.2, MaxOutputTokens: 2048, ResponseMimeType: "application/json"},
	}
	url := fmt.Sprintf("%s/%s:generateContent", c.baseURL, c.modelID(c.model))
	var resp gGenerateResponse
	if err := c.doPost(ctx, url, req, &resp); err != nil {
		return "", fmt.Errorf("describe: %w", err)
	}
	if resp.Error != nil {
		return "", fmt.Errorf("gemini describe error: %s (code %d)", resp.Error.Message, resp.Error.Code)
	}
	if len(resp.Candidates) == 0 || len(resp.Candidates[0].Content.Parts) == 0 {
		return "", fmt.Errorf("empty describe response from Gemini")
	}
	return resp.Candidates[0].Content.Parts[0].Text, nil
}

// --- Embedding ---

// EmbedOptions configures a single embedding request. TaskType is accepted for
// API stability; graph indexing deliberately leaves it EMPTY so both providers
// produce vectors in the same space as what is already stored (no re-embed).
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
	if c.provider == ProviderGoogle {
		return c.embedGoogle(ctx, text, opts, dims)
	}
	return c.embedOpenRouter(ctx, text, dims)
}

func (c *Client) embedOpenRouter(ctx context.Context, text string, dims int) ([]float32, error) {
	req := orEmbedRequest{Model: c.modelID(c.embeddingModel), Input: text, Dimensions: dims}
	var resp orEmbedResponse
	if err := c.doPost(ctx, c.baseURL+"/embeddings", req, &resp); err != nil {
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

func (c *Client) embedGoogle(ctx context.Context, text string, opts EmbedOptions, dims int) ([]float32, error) {
	bare := c.modelID(c.embeddingModel)
	req := gEmbedRequest{
		Model:   "models/" + bare,
		Content: gContent{Parts: []gPart{{Text: text}}},
	}
	if opts.Title != "" || opts.TaskType != "" || dims > 0 {
		req.Title = opts.Title
		req.TaskType = opts.TaskType
		req.OutputDimensionality = dims
		req.EmbedContentConfig = &gEmbedContentConfig{Title: opts.Title, TaskType: opts.TaskType, OutputDimensionality: dims}
	}
	url := fmt.Sprintf("%s/%s:embedContent", c.baseURL, bare)
	var resp gEmbedResponse
	if err := c.doPost(ctx, url, req, &resp); err != nil {
		return nil, fmt.Errorf("embed: %w", err)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("embed API error: %s (code %d)", resp.Error.Message, resp.Error.Code)
	}
	if resp.Embedding == nil {
		return nil, fmt.Errorf("empty embedding response")
	}
	return resp.Embedding.Values, nil
}

// maxEmbedBatch caps texts per API call. Google's batchEmbedContents rejects
// more than 100 requests per call; 96 stays safely under that for both providers.
const maxEmbedBatch = 96

// EmbedBatch generates embeddings for multiple texts at the client's default
// dims, chunking into calls of at most maxEmbedBatch so large inputs don't
// exceed provider per-call limits. Results are returned in texts order.
func (c *Client) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	results := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += maxEmbedBatch {
		end := min(start+maxEmbedBatch, len(texts))
		chunk := texts[start:end]
		var (
			embs [][]float32
			err  error
		)
		if c.provider == ProviderGoogle {
			embs, err = c.batchEmbedGoogle(ctx, chunk)
		} else {
			embs, err = c.batchEmbedOpenRouter(ctx, chunk)
		}
		if err != nil {
			return nil, err
		}
		results = append(results, embs...)
	}
	return results, nil
}

func (c *Client) batchEmbedOpenRouter(ctx context.Context, texts []string) ([][]float32, error) {
	req := orEmbedRequest{Model: c.modelID(c.embeddingModel), Input: texts, Dimensions: c.embeddingDims}
	var resp orEmbedResponse
	if err := c.doPost(ctx, c.baseURL+"/embeddings", req, &resp); err != nil {
		return nil, fmt.Errorf("batch embed: %w", err)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("batch embed API error: %s (code %d)", resp.Error.Message, resp.Error.Code)
	}
	if len(resp.Data) != len(texts) {
		return nil, fmt.Errorf("batch embed: got %d embeddings for %d inputs", len(resp.Data), len(texts))
	}
	results := make([][]float32, len(texts))
	for _, d := range resp.Data {
		if d.Index < 0 || d.Index >= len(texts) {
			return nil, fmt.Errorf("batch embed: response index %d out of range (%d inputs)", d.Index, len(texts))
		}
		results[d.Index] = d.Embedding
	}
	return results, nil
}

func (c *Client) batchEmbedGoogle(ctx context.Context, texts []string) ([][]float32, error) {
	bare := c.modelID(c.embeddingModel)
	requests := make([]gEmbedRequest, len(texts))
	for i, text := range texts {
		requests[i] = gEmbedRequest{
			Model:                "models/" + bare,
			Content:              gContent{Parts: []gPart{{Text: text}}},
			OutputDimensionality: c.embeddingDims,
			EmbedContentConfig:   &gEmbedContentConfig{OutputDimensionality: c.embeddingDims},
		}
	}
	url := fmt.Sprintf("%s/%s:batchEmbedContents", c.baseURL, bare)
	var resp gBatchEmbedResponse
	if err := c.doPost(ctx, url, gBatchEmbedRequest{Requests: requests}, &resp); err != nil {
		return nil, fmt.Errorf("batch embed: %w", err)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("batch embed API error: %s (code %d)", resp.Error.Message, resp.Error.Code)
	}
	if len(resp.Embeddings) != len(texts) {
		return nil, fmt.Errorf("batch embed: got %d embeddings for %d inputs", len(resp.Embeddings), len(texts))
	}
	results := make([][]float32, len(resp.Embeddings))
	for i, emb := range resp.Embeddings {
		results[i] = emb.Values
	}
	return results, nil
}

// --- HTTP with retry ---

// doPost POSTs JSON to url with exponential backoff on 429/5xx, authenticating
// with the current rotation key. When a response says the key itself is dead
// (quota exhausted, rejected), the key is blocked and the call is retried
// IMMEDIATELY on the next key — that swap doesn't spend the backoff budget.
func (c *Client) doPost(ctx context.Context, url string, body any, result any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	const maxRetries = 5
	attempt, swaps := 0, 0
	for {
		key := c.apiKey()
		status, respBody, reqErr := c.post(ctx, url, key, payload)

		if reqErr == nil && status == http.StatusOK {
			if err := json.Unmarshal(respBody, result); err != nil {
				return fmt.Errorf("unmarshal response: %w", err)
			}
			return nil
		}

		// Dead key: block it and swap immediately (bounded by pool size).
		if reqErr == nil && swaps < len(c.keys)-1 {
			if reason, until, ok := blockReason(status, respBody); ok && c.blockKey(ctx, key, reason, until) {
				swaps++
				continue
			}
		}

		retryable := reqErr != nil || status == 429 || status >= 500
		if !retryable {
			return fmt.Errorf("%s API returned %d: %s", c.provider, status, string(respBody))
		}

		attempt++
		if attempt > maxRetries {
			if reqErr != nil {
				return fmt.Errorf("http request failed after %d retries: %w", maxRetries, reqErr)
			}
			return fmt.Errorf("%s API returned %d after %d retries: %s", c.provider, status, maxRetries, string(respBody))
		}
		if reqErr == nil {
			log.Warn().Int("status", status).Int("attempt", attempt).Str("provider", c.provider).Msg("LLM API error, retrying")
		}

		backoff := time.Duration(math.Pow(2, float64(attempt-1))) * time.Second
		log.Debug().Int("attempt", attempt).Dur("backoff", backoff).Str("provider", c.provider).Msg("Retrying LLM request")
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
	}
}

// post makes one authenticated attempt and returns the status and body. Auth is
// per provider: OpenRouter takes a Bearer token, Google takes x-goog-api-key
// (header, not query, so the key never lands in a URL or proxy log).
func (c *Client) post(ctx context.Context, url, key string, payload []byte) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return 0, nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.provider == ProviderGoogle {
		req.Header.Set("x-goog-api-key", key)
	} else {
		req.Header.Set("Authorization", "Bearer "+key)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("read response: %w", err)
	}
	return resp.StatusCode, respBody, nil
}
