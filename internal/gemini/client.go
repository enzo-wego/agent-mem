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
		Index     int       `json:"index"`
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
