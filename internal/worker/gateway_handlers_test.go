package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agent-mem/agent-mem/internal/config"
)

func TestFetchGatewayConfigProxiesAuthMethodAndBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("method = %s, want PUT", r.Method)
		}
		if r.URL.Path != "/config" {
			t.Errorf("path = %q, want /config", r.URL.Path)
		}
		if r.Header.Get("X-API-Key") != "gateway-secret" {
			t.Errorf("X-API-Key = %q", r.Header.Get("X-API-Key"))
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("Content-Type = %q", r.Header.Get("Content-Type"))
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"BACKEND_CHEAP":"openrouter"}` {
			t.Errorf("body = %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"BACKEND_CHEAP":"openrouter"}`))
	}))
	defer server.Close()

	status, raw, err := fetchGatewayConfig(context.Background(), server.Client(), http.MethodPut,
		server.URL+"/config", "gateway-secret", []byte(`{"BACKEND_CHEAP":"openrouter"}`))
	if err != nil {
		t.Fatalf("fetch gateway config: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if string(raw) != `{"BACKEND_CHEAP":"openrouter"}` {
		t.Fatalf("config = %s", raw)
	}
}

func TestFetchGatewayConfigSurfacesValidationDetail(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"detail":"CLAUDE_TIMEOUT_S must be below 200"}`))
	}))
	defer server.Close()

	status, raw, err := fetchGatewayConfig(context.Background(), server.Client(), http.MethodPut,
		server.URL+"/config", "gateway-secret", []byte(`{"CLAUDE_TIMEOUT_S":200}`))
	if err != nil {
		t.Fatalf("fetch gateway config: %v", err)
	}
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", status)
	}
	if !strings.Contains(string(raw), "below 200") {
		t.Fatalf("validation detail lost: %s", raw)
	}
}

func TestGetGatewayConfigProxiesLiveConfigWithoutLeakingKey(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if got := r.Header.Get("X-API-Key"); got != "gateway-secret" {
			t.Errorf("X-API-Key = %q, want gateway-secret", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"BACKEND_CHEAP":"claude"}`))
	}))
	t.Cleanup(upstream.Close)

	s := &Server{config: &config.Config{
		LLMGatewayURL:    upstream.URL,
		LLMGatewayAPIKey: "gateway-secret",
	}}
	req := httptest.NewRequest(http.MethodGet, "/api/gateway/config", nil)
	rec := httptest.NewRecorder()

	s.handleGetGatewayConfig(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != `{"BACKEND_CHEAP":"claude"}` {
		t.Fatalf("body = %s", got)
	}
	if strings.Contains(rec.Body.String(), "gateway-secret") {
		t.Fatal("gateway API key leaked in response")
	}
}

func TestPatchGatewayConfigForwardsOnlyAllowlistedKeys(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			t.Errorf("method = %s, want PUT", r.Method)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		if got := body["BACKEND_CHEAP"]; got != "openrouter" {
			t.Errorf("BACKEND_CHEAP = %#v, want openrouter", got)
		}
		if got := body["MAX_BUDGET_USD"]; got != float64(2.5) {
			t.Errorf("MAX_BUDGET_USD = %#v, want 2.5", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"BACKEND_CHEAP":"openrouter","MAX_BUDGET_USD":2.5}`))
	}))
	t.Cleanup(upstream.Close)

	s := &Server{config: &config.Config{
		LLMGatewayURL:    upstream.URL,
		LLMGatewayAPIKey: "gateway-secret",
	}}
	req := httptest.NewRequest(http.MethodPatch, "/api/gateway/config",
		bytes.NewBufferString(`{"BACKEND_CHEAP":"openrouter","MAX_BUDGET_USD":2.5}`))
	rec := httptest.NewRecorder()

	s.handlePatchGatewayConfig(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
}

func TestPatchGatewayConfigRejectsNonWhitelistedKey(t *testing.T) {
	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		upstreamCalls++
	}))
	t.Cleanup(upstream.Close)

	s := &Server{config: &config.Config{
		LLMGatewayURL:    upstream.URL,
		LLMGatewayAPIKey: "gateway-secret",
	}}
	req := httptest.NewRequest(http.MethodPatch, "/api/gateway/config",
		bytes.NewBufferString(`{"FALLBACK_ON_QUOTA":true}`))
	rec := httptest.NewRecorder()

	s.handlePatchGatewayConfig(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "FALLBACK_ON_QUOTA") {
		t.Fatalf("response does not name rejected key: %s", rec.Body.String())
	}
	if upstreamCalls != 0 {
		t.Fatalf("upstream calls = %d, want 0", upstreamCalls)
	}
}

func TestPatchGatewayConfigRejectsNonObjectBody(t *testing.T) {
	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		upstreamCalls++
	}))
	t.Cleanup(upstream.Close)

	s := &Server{config: &config.Config{
		LLMGatewayURL:    upstream.URL,
		LLMGatewayAPIKey: "gateway-secret",
	}}
	req := httptest.NewRequest(http.MethodPatch, "/api/gateway/config", bytes.NewBufferString(`null`))
	rec := httptest.NewRecorder()

	s.handlePatchGatewayConfig(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
	if upstreamCalls != 0 {
		t.Fatalf("upstream calls = %d, want 0", upstreamCalls)
	}
}

func TestGetGatewayConfigReportsUnreachableGateway(t *testing.T) {
	s := &Server{config: &config.Config{
		LLMGatewayURL:    "http://127.0.0.1:0",
		LLMGatewayAPIKey: "gateway-secret",
	}}
	req := httptest.NewRequest(http.MethodGet, "/api/gateway/config", nil)
	rec := httptest.NewRecorder()

	s.handleGetGatewayConfig(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "llm-gateway unreachable") {
		t.Fatalf("response does not explain gateway failure: %s", rec.Body.String())
	}
}
