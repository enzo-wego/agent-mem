package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"time"

	"github.com/rs/zerolog/log"
)

const baseURL = "https://generativelanguage.googleapis.com/v1beta/models"

// Client is a Gemini REST API client for generation and embedding.
type Client struct {
	apiKey         string
	model          string
	embeddingModel string
	embeddingDims  int
	httpClient     *http.Client
}

// NewClient creates a new Gemini API client.
func NewClient(apiKey, model, embeddingModel string, embeddingDims int) *Client {
	return &Client{
		apiKey:         apiKey,
		model:          model,
		embeddingModel: embeddingModel,
		embeddingDims:  embeddingDims,
		httpClient:     &http.Client{Timeout: 60 * time.Second},
	}
}

// --- Generation ---

type generateRequest struct {
	Contents         []content        `json:"contents"`
	SystemInstruction *content        `json:"systemInstruction,omitempty"`
	GenerationConfig generationConfig `json:"generationConfig"`
}

type content struct {
	Role  string `json:"role,omitempty"`
	Parts []part `json:"parts"`
}

type part struct {
	Text string `json:"text"`
}

type generationConfig struct {
	Temperature      float64 `json:"temperature"`
	MaxOutputTokens  int     `json:"maxOutputTokens"`
	ResponseMimeType string  `json:"responseMimeType,omitempty"`
}

type generateResponse struct {
	Candidates []struct {
		Content struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"content"`
	} `json:"candidates"`
	Error *apiError `json:"error,omitempty"`
}

type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Status  string `json:"status"`
}

// Generate sends a prompt to Gemini and returns the response text.
// Uses responseMimeType=application/json to force valid JSON output.
func (c *Client) Generate(ctx context.Context, systemPrompt, userMessage string) (string, error) {
	req := generateRequest{
		Contents: []content{
			{Role: "user", Parts: []part{{Text: userMessage}}},
		},
		GenerationConfig: generationConfig{
			Temperature:      0.3,
			MaxOutputTokens:  4096,
			ResponseMimeType: "application/json",
		},
	}

	if systemPrompt != "" {
		req.SystemInstruction = &content{
			Parts: []part{{Text: systemPrompt}},
		}
	}

	url := fmt.Sprintf("%s/%s:generateContent?key=%s", baseURL, c.model, c.apiKey)

	var resp generateResponse
	if err := c.doWithRetry(ctx, url, req, &resp); err != nil {
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

// --- Embedding ---

type embedRequest struct {
	Model               string  `json:"model"`
	Content             content `json:"content"`
	OutputDimensionality int    `json:"outputDimensionality,omitempty"`
}

type embedResponse struct {
	Embedding *struct {
		Values []float32 `json:"values"`
	} `json:"embedding"`
	Error *apiError `json:"error,omitempty"`
}

type batchEmbedRequest struct {
	Requests []embedRequest `json:"requests"`
}

type batchEmbedResponse struct {
	Embeddings []struct {
		Values []float32 `json:"values"`
	} `json:"embeddings"`
	Error *apiError `json:"error,omitempty"`
}

// Embed generates a single embedding vector for the given text.
func (c *Client) Embed(ctx context.Context, text string) ([]float32, error) {
	modelPath := fmt.Sprintf("models/%s", c.embeddingModel)
	req := embedRequest{
		Model:               modelPath,
		Content:             content{Parts: []part{{Text: text}}},
		OutputDimensionality: c.embeddingDims,
	}

	url := fmt.Sprintf("%s/%s:embedContent?key=%s", baseURL, c.embeddingModel, c.apiKey)

	var resp embedResponse
	if err := c.doWithRetry(ctx, url, req, &resp); err != nil {
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

// EmbedBatch generates embeddings for multiple texts (up to 100 per request).
func (c *Client) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	modelPath := fmt.Sprintf("models/%s", c.embeddingModel)
	requests := make([]embedRequest, len(texts))
	for i, text := range texts {
		requests[i] = embedRequest{
			Model:               modelPath,
			Content:             content{Parts: []part{{Text: text}}},
			OutputDimensionality: c.embeddingDims,
		}
	}

	req := batchEmbedRequest{Requests: requests}
	url := fmt.Sprintf("%s/%s:batchEmbedContents?key=%s", baseURL, c.embeddingModel, c.apiKey)

	var resp batchEmbedResponse
	if err := c.doWithRetry(ctx, url, req, &resp); err != nil {
		return nil, fmt.Errorf("batch embed: %w", err)
	}

	if resp.Error != nil {
		return nil, fmt.Errorf("batch embed API error: %s (code %d)", resp.Error.Message, resp.Error.Code)
	}

	results := make([][]float32, len(resp.Embeddings))
	for i, emb := range resp.Embeddings {
		results[i] = emb.Values
	}
	return results, nil
}

// --- HTTP with retry ---

// doWithRetry executes an HTTP POST with exponential backoff on 429/500/503.
func (c *Client) doWithRetry(ctx context.Context, url string, body any, result any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	maxRetries := 5
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(math.Pow(2, float64(attempt-1))) * time.Second
			log.Debug().Int("attempt", attempt).Dur("backoff", backoff).Msg("Retrying Gemini request")
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

		// Retry on rate limit or server errors
		if resp.StatusCode == 429 || resp.StatusCode >= 500 {
			if attempt == maxRetries {
				return fmt.Errorf("gemini API returned %d after %d retries: %s", resp.StatusCode, maxRetries, string(respBody))
			}
			log.Warn().Int("status", resp.StatusCode).Int("attempt", attempt).Msg("Gemini API error, retrying")
			continue
		}

		if resp.StatusCode != 200 {
			return fmt.Errorf("gemini API returned %d: %s", resp.StatusCode, string(respBody))
		}

		if err := json.Unmarshal(respBody, result); err != nil {
			return fmt.Errorf("unmarshal response: %w", err)
		}
		return nil
	}

	return fmt.Errorf("exhausted retries")
}
