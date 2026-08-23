package worker

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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

	got := fetchGatewayConfig(context.Background(), server.Client(), http.MethodPut,
		server.URL+"/config", "gateway-secret", []byte(`{"BACKEND_CHEAP":"openrouter"}`))
	if !got.Available || got.Error != "" {
		t.Fatalf("response = %#v", got)
	}
	if string(got.Config) != `{"BACKEND_CHEAP":"openrouter"}` {
		t.Fatalf("config = %s", got.Config)
	}
}

func TestFetchGatewayConfigSurfacesValidationDetail(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"detail":"CLAUDE_TIMEOUT_S must be below 200"}`))
	}))
	defer server.Close()

	got := fetchGatewayConfig(context.Background(), server.Client(), http.MethodPut,
		server.URL+"/config", "gateway-secret", []byte(`{"CLAUDE_TIMEOUT_S":200}`))
	if got.Available {
		t.Fatal("invalid gateway config reported available")
	}
	if !strings.Contains(got.Error, "below 200") {
		t.Fatalf("validation detail lost: %q", got.Error)
	}
}
