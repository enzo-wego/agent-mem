package worker

import (
	"bytes"
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
// agent-mem owns none of the gateway's provider config. The dashboard can edit
// it through the sibling config proxy below, but llm-gateway remains the only
// process that stores and applies those values.
type gatewayHealthResponse struct {
	Available bool            `json:"available"`
	Error     string          `json:"error,omitempty"`
	Health    json.RawMessage `json:"health,omitempty"`
}

type gatewayConfigResponse struct {
	Available bool            `json:"available"`
	Error     string          `json:"error,omitempty"`
	Config    json.RawMessage `json:"config,omitempty"`
}

// gatewayHTTPClient is dedicated to the same-host llm-gateway hop. The worker's
// ambient HTTP_PROXY is required for some external traffic, but sending this
// bridge-address traffic through that relay makes the gateway unreachable.
var gatewayHTTPClient = func() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &http.Client{Transport: transport, Timeout: 15 * time.Second}
}()

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

	resp, err := gatewayHTTPClient.Do(req)
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

func (s *Server) handleGetGatewayConfig(w http.ResponseWriter, r *http.Request) {
	s.proxyGatewayConfig(w, r, http.MethodGet)
}

func (s *Server) handlePutGatewayConfig(w http.ResponseWriter, r *http.Request) {
	s.proxyGatewayConfig(w, r, http.MethodPut)
}

// proxyGatewayConfig is deliberately a pure proxy: the gateway owns these
// values and persists them. Keeping no local copy prevents agent-mem and other
// gateway clients from drifting onto different runtime configuration.
func (s *Server) proxyGatewayConfig(w http.ResponseWriter, r *http.Request, method string) {
	snap := s.config.Snapshot()
	base := strings.TrimSpace(snap.LLMGatewayURL)
	if base == "" {
		writeGatewayConfig(w, gatewayConfigResponse{Available: false, Error: "llm_gateway_url not configured"})
		return
	}

	var body []byte
	if method == http.MethodPut {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
		var err error
		body, err = io.ReadAll(r.Body)
		if err != nil {
			writeGatewayConfig(w, gatewayConfigResponse{Available: false, Error: "invalid gateway config body"})
			return
		}
		if !json.Valid(body) {
			writeGatewayConfig(w, gatewayConfigResponse{Available: false, Error: "invalid JSON"})
			return
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	result := fetchGatewayConfig(ctx, gatewayHTTPClient, method,
		strings.TrimRight(base, "/")+"/config", snap.LLMGatewayAPIKey, body)
	writeGatewayConfig(w, result)
}

func fetchGatewayConfig(
	ctx context.Context,
	hc *http.Client,
	method, url, apiKey string,
	body []byte,
) gatewayConfigResponse {
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return gatewayConfigResponse{Available: false, Error: err.Error()}
	}
	req.Header.Set("X-API-Key", apiKey)
	if method == http.MethodPut {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := hc.Do(req)
	if err != nil {
		return gatewayConfigResponse{Available: false, Error: "llm-gateway unreachable: " + err.Error()}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return gatewayConfigResponse{Available: false, Error: "read llm-gateway /config: " + err.Error()}
	}
	if resp.StatusCode != http.StatusOK {
		detail := strings.TrimSpace(string(raw))
		var problem struct {
			Detail string `json:"detail"`
		}
		if json.Unmarshal(raw, &problem) == nil && problem.Detail != "" {
			detail = problem.Detail
		}
		if detail != "" {
			detail = ": " + detail
		}
		return gatewayConfigResponse{Available: false,
			Error: fmt.Sprintf("llm-gateway /config returned %d%s", resp.StatusCode, detail)}
	}
	if !json.Valid(raw) {
		return gatewayConfigResponse{Available: false, Error: "llm-gateway /config returned non-JSON"}
	}
	return gatewayConfigResponse{Available: true, Config: json.RawMessage(raw)}
}

func writeGatewayConfig(w http.ResponseWriter, resp gatewayConfigResponse) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Error().Err(err).Msg("Failed to encode gateway config response")
	}
}
