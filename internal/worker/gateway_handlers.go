package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/rs/zerolog/log"
)

// gatewayHealthResponse is what GET /api/llm-gateway/health returns to the
// dashboard. It is a thin proxy of llm-gateway's own /health: the browser
// cannot reach the container→host bridge address the worker uses, so the worker
// fetches it and passes the body through under Health.
//
// This is visibility only. agent-mem holds no provider config and does not edit
// the gateway's model/backend/fallback settings — those live in llm-gateway.
// The panel just surfaces which backend serves each tier and whether the Claude
// seat is available, so an operator can see the LLM egress at a glance.
type gatewayHealthResponse struct {
	Available bool            `json:"available"`
	Error     string          `json:"error,omitempty"`
	Health    json.RawMessage `json:"health,omitempty"`
}

// handleGatewayHealth proxies llm-gateway's GET /health. Failures are reported
// as {available:false,error} rather than a 500 so the dashboard can render a
// clear "gateway unreachable" state instead of a blank panel.
func (s *Server) handleGatewayHealth(w http.ResponseWriter, r *http.Request) {
	snap := s.config.Snapshot()
	base := strings.TrimSpace(snap.LLMGatewayURL)
	if base == "" {
		writeGatewayHealth(w, gatewayHealthResponse{Available: false, Error: "llm_gateway_url not configured"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/health", nil)
	if err != nil {
		writeGatewayHealth(w, gatewayHealthResponse{Available: false, Error: err.Error()})
		return
	}
	// /health is unauthenticated on the gateway today, but send the key anyway
	// so this keeps working if that ever changes — it is ignored when not needed.
	if k := strings.TrimSpace(snap.LLMGatewayAPIKey); k != "" {
		req.Header.Set("X-API-Key", k)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		writeGatewayHealth(w, gatewayHealthResponse{Available: false, Error: "llm-gateway unreachable: " + err.Error()})
		return
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		writeGatewayHealth(w, gatewayHealthResponse{Available: false, Error: err.Error()})
		return
	}
	if resp.StatusCode != http.StatusOK {
		writeGatewayHealth(w, gatewayHealthResponse{Available: false,
			Error: fmt.Sprintf("llm-gateway /health returned %d", resp.StatusCode)})
		return
	}
	if !json.Valid(body) {
		writeGatewayHealth(w, gatewayHealthResponse{Available: false, Error: "llm-gateway /health returned non-JSON"})
		return
	}

	writeGatewayHealth(w, gatewayHealthResponse{Available: true, Health: json.RawMessage(body)})
}

func writeGatewayHealth(w http.ResponseWriter, resp gatewayHealthResponse) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Error().Err(err).Msg("Failed to encode gateway health response")
	}
}
