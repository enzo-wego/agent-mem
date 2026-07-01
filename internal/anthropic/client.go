// Package anthropic is a minimal Claude Messages API client. It exists so graph
// summaries (cluster/thread) can run on Claude instead of Gemini Flash, which
// hallucinated ticket ids and outcomes. Only text Generate is implemented;
// embeddings stay on Gemini.
package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
)

const apiURL = "https://api.anthropic.com/v1/messages"

// Client is a Claude Messages API client.
type Client struct {
	apiKey     string
	model      string
	baseURL    string // overridable in tests
	httpClient *http.Client
}

// NewClient creates a Claude client. model defaults to claude-sonnet-5 when empty.
func NewClient(apiKey, model string) *Client {
	if model == "" {
		model = "claude-sonnet-5"
	}
	return &Client{apiKey: apiKey, model: model, baseURL: apiURL, httpClient: &http.Client{Timeout: 90 * time.Second}}
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type generateRequest struct {
	Model     string    `json:"model"`
	MaxTokens int       `json:"max_tokens"`
	System    string    `json:"system,omitempty"`
	Messages  []message `json:"messages"`
}

type generateResponse struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// Generate sends system+user to Claude and returns the response text. Callers
// here expect a JSON object; this model rejects assistant prefill, so we let the
// model answer and then extract the {...} object (tolerating markdown fences or
// surrounding prose).
func (c *Client) Generate(ctx context.Context, systemPrompt, userMessage string) (string, error) {
	req := generateRequest{
		Model:     c.model,
		MaxTokens: 2048,
		System:    systemPrompt,
		Messages:  []message{{Role: "user", Content: userMessage}},
	}

	var resp generateResponse
	if err := c.doWithRetry(ctx, req, &resp); err != nil {
		return "", fmt.Errorf("generate: %w", err)
	}
	if resp.Error != nil {
		return "", fmt.Errorf("anthropic API error: %s: %s", resp.Error.Type, resp.Error.Message)
	}
	if len(resp.Content) == 0 || resp.Content[0].Text == "" {
		return "", fmt.Errorf("empty response from Claude")
	}
	// Extract the JSON object: first "{" to last "}", dropping any fences/prose.
	out := resp.Content[0].Text
	if start := strings.IndexByte(out, '{'); start >= 0 {
		if end := strings.LastIndexByte(out, '}'); end > start {
			out = out[start : end+1]
		}
	}
	return out, nil
}

func (c *Client) doWithRetry(ctx context.Context, body any, result any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}
	const maxRetries = 5
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(math.Pow(2, float64(attempt-1))) * time.Second
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL, bytes.NewReader(payload))
		if err != nil {
			return fmt.Errorf("create request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-api-key", c.apiKey)
		req.Header.Set("anthropic-version", "2023-06-01")

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
				return fmt.Errorf("anthropic API returned %d after %d retries: %s", resp.StatusCode, maxRetries, string(respBody))
			}
			log.Warn().Int("status", resp.StatusCode).Int("attempt", attempt).Msg("Anthropic API error, retrying")
			continue
		}
		if resp.StatusCode != 200 {
			return fmt.Errorf("anthropic API returned %d: %s", resp.StatusCode, string(respBody))
		}
		if err := json.Unmarshal(respBody, result); err != nil {
			return fmt.Errorf("unmarshal response: %w", err)
		}
		return nil
	}
	return fmt.Errorf("exhausted retries")
}
