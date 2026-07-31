// Package llmgateway is an HTTP client for llm-gateway, which fronts a Claude
// subscription seat. It implements graph handlers' TextGenerator, so graph
// summaries can run on Sonnet 5 without agent-mem holding any API credential
// that bills per token.
//
// Why a gateway instead of calling Claude directly: a metered API key has no
// spend ceiling, and a summarize_thread amplification bug once pushed ~$11/hour
// through one. A subscription seat rate-limits instead of charging, so the same
// bug degrades rather than bills. The gateway also owns model choice — callers
// ask for a tier ("summary" / "cheap"), never a model name — so switching models
// is a systemd restart there rather than a Go deploy here.
package llmgateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// RequestTimeout must sit ABOVE the gateway's own LLM_GATEWAY_CLAUDE_TIMEOUT_S
// (180s by default) and BELOW the job lease of every handler that calls us.
// The ordering is load-bearing:
//
//	gateway 180s  <  client 200s  <  lease 240s
//
// Too low and we cut off calls the gateway would have answered. Too high and the
// lease expires mid-flight, the janitor reclaims the job, and a second worker
// redoes the same summary — duplicate LLM calls, which is precisely the
// amplification pattern that made this rewrite necessary. Change one, check all
// three; see SummaryLease in the handlers package.
const RequestTimeout = 200 * time.Second

// Client talks to one llm-gateway instance at one intent tier.
type Client struct {
	baseURL string
	apiKey  string
	tier    string
	http    *http.Client
}

// New returns a client for baseURL (e.g. "http://172.18.0.1:8750"). tier is the
// gateway's intent tier — "summary" for prose worth Sonnet, "cheap" for
// high-volume judgements. An unknown tier is coerced to "cheap" so a typo
// degrades cost rather than silently buying the expensive model.
func New(baseURL, apiKey, tier string) *Client {
	if tier != "summary" && tier != "cheap" {
		tier = "cheap"
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		tier:    tier,
		http:    &http.Client{Timeout: RequestTimeout},
	}
}

type generateRequest struct {
	System string `json:"system"`
	User   string `json:"user"`
	Tier   string `json:"tier"`
}

type generateResponse struct {
	Backend string `json:"backend"`
	Text    string `json:"text"`
}

// Generate satisfies graph handlers' TextGenerator.
//
// Errors are returned, never swallowed into an empty string: callers treat ""
// as "the LLM had nothing to say" and cache that result, so a dead gateway must
// look like a failure and let the job retry. There is deliberately no fallback
// to Gemini here — the gateway already falls back to OpenRouter internally when
// the seat is out of quota, so the only case left is the gateway being down,
// and that should be loud rather than silently papered over.
func (c *Client) Generate(ctx context.Context, systemPrompt, userMessage string) (string, error) {
	body, err := json.Marshal(generateRequest{
		System: systemPrompt, User: userMessage, Tier: c.tier,
	})
	if err != nil {
		return "", fmt.Errorf("llm-gateway: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/generate", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("llm-gateway: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", c.apiKey)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("llm-gateway: post /generate: %w", err)
	}
	defer resp.Body.Close()

	// Cap the read: a wedged proxy can stream indefinitely, and no legitimate
	// summary is anywhere near this size.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("llm-gateway: read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// 503 is the gateway telling us the seat is spent and fallback is off.
		// Include the body — it carries the reset time, which is the one detail
		// that makes the failure actionable.
		return "", fmt.Errorf("llm-gateway: /generate returned %d: %s",
			resp.StatusCode, strings.TrimSpace(truncate(string(raw), 300)))
	}

	var out generateResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("llm-gateway: decode response: %w", err)
	}
	if strings.TrimSpace(out.Text) == "" {
		// A 200 with no text means the tier returned structured output or the
		// backend produced nothing. Either way there is no prose to cache, and
		// reporting success would poison the summary cache with an empty string.
		return "", fmt.Errorf("llm-gateway: /generate returned no text (backend=%s)", out.Backend)
	}
	return out.Text, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
