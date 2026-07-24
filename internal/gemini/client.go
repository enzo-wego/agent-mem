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
	"strings"
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

// Client is a dual-mode client for generation and embedding.
type Client struct {
	provider       string
	apiKey         string
	model          string
	embeddingModel string
	embeddingDims  int
	baseURL        string
	httpClient     *http.Client
}

// NewClient creates a client for the given provider. An empty/unknown provider
// defaults to OpenRouter. model/embeddingModel may be given with or without the
// "google/" namespace prefix; it is normalized per provider at call time.
func NewClient(provider, apiKey, model, embeddingModel string, embeddingDims int) *Client {
	if provider != ProviderGoogle {
		provider = ProviderOpenRouter
	}
	baseURL := openRouterBaseURL
	if provider == ProviderGoogle {
		baseURL = googleBaseURL
	}
	return &Client{
		provider:       provider,
		apiKey:         apiKey,
		model:          model,
		embeddingModel: embeddingModel,
		embeddingDims:  embeddingDims,
		baseURL:        baseURL,
		httpClient:     &http.Client{Timeout: 60 * time.Second},
	}
}

// Provider reports the active backend (for logging/diagnostics).
func (c *Client) Provider() string { return c.provider }

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
	if err := c.doPost(ctx, c.baseURL+"/chat/completions", "Bearer "+c.apiKey, req, &resp); err != nil {
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
	url := fmt.Sprintf("%s/%s:generateContent?key=%s", c.baseURL, c.modelID(c.model), c.apiKey)
	var resp gGenerateResponse
	if err := c.doPost(ctx, url, "", req, &resp); err != nil {
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
	if err := c.doPost(ctx, c.baseURL+"/chat/completions", "Bearer "+c.apiKey, req, &resp); err != nil {
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
	url := fmt.Sprintf("%s/%s:generateContent?key=%s", c.baseURL, c.modelID(c.model), c.apiKey)
	var resp gGenerateResponse
	if err := c.doPost(ctx, url, "", req, &resp); err != nil {
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
	if err := c.doPost(ctx, c.baseURL+"/embeddings", "Bearer "+c.apiKey, req, &resp); err != nil {
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
	url := fmt.Sprintf("%s/%s:embedContent?key=%s", c.baseURL, bare, c.apiKey)
	var resp gEmbedResponse
	if err := c.doPost(ctx, url, "", req, &resp); err != nil {
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
	if err := c.doPost(ctx, c.baseURL+"/embeddings", "Bearer "+c.apiKey, req, &resp); err != nil {
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
	url := fmt.Sprintf("%s/%s:batchEmbedContents?key=%s", c.baseURL, bare, c.apiKey)
	var resp gBatchEmbedResponse
	if err := c.doPost(ctx, url, "", gBatchEmbedRequest{Requests: requests}, &resp); err != nil {
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

// doPost POSTs JSON to url with exponential backoff on 429/5xx. authHeader, when
// non-empty, is sent as the Authorization header (OpenRouter Bearer); Google
// carries its key in the URL query and passes "".
func (c *Client) doPost(ctx context.Context, url, authHeader string, body any, result any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	maxRetries := 5
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(math.Pow(2, float64(attempt-1))) * time.Second
			log.Debug().Int("attempt", attempt).Dur("backoff", backoff).Str("provider", c.provider).Msg("Retrying LLM request")
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
		if authHeader != "" {
			req.Header.Set("Authorization", authHeader)
		}

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
				return fmt.Errorf("%s API returned %d after %d retries: %s", c.provider, resp.StatusCode, maxRetries, string(respBody))
			}
			log.Warn().Int("status", resp.StatusCode).Int("attempt", attempt).Str("provider", c.provider).Msg("LLM API error, retrying")
			continue
		}

		if resp.StatusCode != 200 {
			return fmt.Errorf("%s API returned %d: %s", c.provider, resp.StatusCode, string(respBody))
		}

		if err := json.Unmarshal(respBody, result); err != nil {
			return fmt.Errorf("unmarshal response: %w", err)
		}
		return nil
	}
	return fmt.Errorf("exhausted retries")
}
