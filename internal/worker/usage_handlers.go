package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

// openRouterKeyEndpoint is OpenRouter's key-introspection endpoint. It
// reports the remaining quota/usage for the configured API key.
const openRouterKeyEndpoint = "https://openrouter.ai/api/v1/key"

// openRouterUsageCacheTTL controls how long a successful upstream lookup is
// reused before the dashboard's polling triggers another OpenRouter call.
const openRouterUsageCacheTTL = 60 * time.Second

// openRouterUsageResponse is the normalized JSON shape served by
// GET /api/openrouter/usage.
type openRouterUsageResponse struct {
	Available bool   `json:"available"`
	Error     string `json:"error,omitempty"`

	Label          string   `json:"label,omitempty"`
	Usage          *float64 `json:"usage,omitempty"`
	Limit          *float64 `json:"limit"`
	LimitRemaining *float64 `json:"limit_remaining,omitempty"`
	LimitReset     string   `json:"limit_reset,omitempty"`
	UsageDaily     *float64 `json:"usage_daily,omitempty"`
	UsageMonthly   *float64 `json:"usage_monthly,omitempty"`
	IsFreeTier     *bool    `json:"is_free_tier,omitempty"`
}

// openRouterKeyData mirrors the "data" object of OpenRouter's
// GET /api/v1/key response.
type openRouterKeyData struct {
	Label          string   `json:"label"`
	Limit          *float64 `json:"limit"`
	LimitReset     string   `json:"limit_reset"`
	LimitRemaining *float64 `json:"limit_remaining"`
	Usage          *float64 `json:"usage"`
	UsageDaily     *float64 `json:"usage_daily"`
	UsageWeekly    *float64 `json:"usage_weekly"`
	UsageMonthly   *float64 `json:"usage_monthly"`
	IsFreeTier     *bool    `json:"is_free_tier"`
}

type openRouterKeyEnvelope struct {
	Data openRouterKeyData `json:"data"`
}

// fetchOpenRouterUsage fetches and normalizes OpenRouter key usage. It has
// no dependency on Server so it can be unit tested directly against an
// httptest server.
func fetchOpenRouterUsage(ctx context.Context, client *http.Client, baseURL, key string) openRouterUsageResponse {
	if key == "" || !strings.HasPrefix(key, "sk-or-") {
		return openRouterUsageResponse{Available: false, Error: "OpenRouter key not configured"}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL, nil)
	if err != nil {
		return openRouterUsageResponse{Available: false, Error: err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+key)

	resp, err := client.Do(req)
	if err != nil {
		return openRouterUsageResponse{Available: false, Error: err.Error()}
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return openRouterUsageResponse{Available: false, Error: err.Error()}
	}

	if resp.StatusCode != http.StatusOK {
		return openRouterUsageResponse{
			Available: false,
			Error:     fmt.Sprintf("upstream returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body))),
		}
	}

	var envelope openRouterKeyEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return openRouterUsageResponse{Available: false, Error: err.Error()}
	}

	return openRouterUsageResponse{
		Available:      true,
		Label:          envelope.Data.Label,
		Usage:          envelope.Data.Usage,
		Limit:          envelope.Data.Limit,
		LimitRemaining: envelope.Data.LimitRemaining,
		LimitReset:     envelope.Data.LimitReset,
		UsageDaily:     envelope.Data.UsageDaily,
		UsageMonthly:   envelope.Data.UsageMonthly,
		IsFreeTier:     envelope.Data.IsFreeTier,
	}
}

// openRouterUsageCache holds the last successful upstream lookup so repeated
// dashboard polling doesn't hit OpenRouter on every request.
type openRouterUsageCache struct {
	mu      sync.Mutex
	resp    openRouterUsageResponse
	fetched time.Time
}

func (c *openRouterUsageCache) get() (openRouterUsageResponse, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.resp.Available && time.Since(c.fetched) < openRouterUsageCacheTTL {
		return c.resp, true
	}
	return openRouterUsageResponse{}, false
}

func (c *openRouterUsageCache) set(resp openRouterUsageResponse) {
	if !resp.Available {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.resp = resp
	c.fetched = time.Now()
}

// openRouterCache is the package-level 60s cache shared across requests.
var openRouterCache openRouterUsageCache

// handleOpenRouterUsage proxies OpenRouter's key-usage endpoint so the
// dashboard can show quota/usage without exposing the raw API key.
func (s *Server) handleOpenRouterUsage(w http.ResponseWriter, r *http.Request) {
	snap := s.config.Snapshot()
	key := snap.GeminiAPIKey

	if key == "" || !strings.HasPrefix(key, "sk-or-") {
		writeOpenRouterUsage(w, openRouterUsageResponse{Available: false, Error: "OpenRouter key not configured"})
		return
	}

	if cached, ok := openRouterCache.get(); ok {
		writeOpenRouterUsage(w, cached)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	result := fetchOpenRouterUsage(ctx, http.DefaultClient, openRouterKeyEndpoint, key)
	openRouterCache.set(result)

	writeOpenRouterUsage(w, result)
}

func writeOpenRouterUsage(w http.ResponseWriter, resp openRouterUsageResponse) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Error().Err(err).Msg("Failed to encode openrouter usage response")
	}
}
