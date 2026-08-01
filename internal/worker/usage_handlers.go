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

// handleOpenRouterUsage reports OpenRouter budget for the dashboard.
//
// It asks llm-gateway rather than OpenRouter: the gateway holds the key, and
// agent-mem deliberately holds no provider credentials. Read-only status about
// a service this one depends on is fair to surface here; configuring that
// service is not, and stays in the gateway.
func (s *Server) handleOpenRouterUsage(w http.ResponseWriter, r *http.Request) {
	snap := s.config.Snapshot()
	if strings.TrimSpace(snap.LLMGatewayURL) == "" {
		writeOpenRouterUsage(w, openRouterUsageResponse{Available: false, Error: "llm_gateway_url not configured"})
		return
	}

	if cached, ok := openRouterCache.get(); ok {
		writeOpenRouterUsage(w, cached)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	result := fetchGatewayUsage(ctx, http.DefaultClient,
		strings.TrimRight(snap.LLMGatewayURL, "/")+"/usage", snap.LLMGatewayAPIKey)
	openRouterCache.set(result)

	writeOpenRouterUsage(w, result)
}

// gatewayUsage mirrors llm-gateway's GET /usage. Only the OpenRouter half is
// mapped here; the seat half is surfaced by the gateway status panel.
type gatewayUsage struct {
	OpenRouter struct {
		Limit          *float64 `json:"limit"`
		Usage          *float64 `json:"usage"`
		LimitRemaining *float64 `json:"limit_remaining"`
		UsageDaily     *float64 `json:"usage_daily"`
		UsageMonthly   *float64 `json:"usage_monthly"`
		Error          string   `json:"error"`
	} `json:"openrouter"`
}

func fetchGatewayUsage(ctx context.Context, hc *http.Client, url, apiKey string) openRouterUsageResponse {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return openRouterUsageResponse{Available: false, Error: err.Error()}
	}
	req.Header.Set("X-API-Key", apiKey)

	resp, err := hc.Do(req)
	if err != nil {
		return openRouterUsageResponse{Available: false, Error: "llm-gateway unreachable: " + err.Error()}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return openRouterUsageResponse{Available: false,
			Error: fmt.Sprintf("llm-gateway /usage returned %d", resp.StatusCode)}
	}

	var g gatewayUsage
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&g); err != nil {
		return openRouterUsageResponse{Available: false, Error: "decode /usage: " + err.Error()}
	}
	if g.OpenRouter.Error != "" {
		return openRouterUsageResponse{Available: false, Error: g.OpenRouter.Error}
	}
	return openRouterUsageResponse{
		Available:      true,
		Limit:          g.OpenRouter.Limit,
		Usage:          g.OpenRouter.Usage,
		LimitRemaining: g.OpenRouter.LimitRemaining,
		UsageDaily:     g.OpenRouter.UsageDaily,
		UsageMonthly:   g.OpenRouter.UsageMonthly,
		LimitReset:     "monthly",
	}
}

func writeOpenRouterUsage(w http.ResponseWriter, resp openRouterUsageResponse) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Error().Err(err).Msg("Failed to encode openrouter usage response")
	}
}
