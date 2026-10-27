package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/agent-mem/agent-mem/internal/config"
	"github.com/jackc/pgx/v5/pgxpool"
)

type capturedEmbedRequest struct {
	Fallback *bool
	Caller   string
}

func captureEmbedRequests(t *testing.T) (*httptest.Server, *[]capturedEmbedRequest) {
	t.Helper()
	var requests []capturedEmbedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Texts    []string `json:"texts"`
			Fallback *bool    `json:"fallback"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		requests = append(requests, capturedEmbedRequest{Fallback: body.Fallback, Caller: r.Header.Get("X-LLM-Caller")})
		embeddings := make([][]float32, len(body.Texts))
		for i := range embeddings {
			embeddings[i] = []float32{1}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"embeddings": embeddings, "dims": 1})
	}))
	t.Cleanup(srv.Close)
	return srv, &requests
}

func TestNewBackfillClientRequestOptions(t *testing.T) {
	for _, tc := range []struct {
		name           string
		openrouterOnly bool
	}{
		{name: "openrouter only", openrouterOnly: true},
		{name: "default fallback", openrouterOnly: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, requests := captureEmbedRequests(t)
			client := newBackfillClient(srv.URL, "key", 1, tc.openrouterOnly)
			for range 2 {
				if _, err := client.EmbedBatch(context.Background(), []string{"one"}); err != nil {
					t.Fatal(err)
				}
			}
			for i, request := range *requests {
				if tc.openrouterOnly {
					if request.Fallback == nil || *request.Fallback {
						t.Errorf("request %d fallback = %v, want false", i, request.Fallback)
					}
					if request.Caller != "agent-mem/backfill" {
						t.Errorf("request %d caller = %q", i, request.Caller)
					}
				} else if request.Fallback != nil || request.Caller != "" {
					t.Errorf("request %d sent fallback=%v caller=%q", i, request.Fallback, request.Caller)
				}
			}
		})
	}
}

func TestBackfillCommandOpenRouterOnlyFlag(t *testing.T) {
	original := runBackfill
	t.Cleanup(func() { runBackfill = original })

	t.Setenv("LLM_GATEWAY_URL", "")
	var received []bool
	runBackfill = func(_, _, _ string, _ int, openrouterOnly bool) error {
		received = append(received, openrouterOnly)
		return nil
	}
	cfg := &config.Config{
		DatabaseURL:         "postgres://user:pass@127.0.0.1:1/agentmem",
		LLMGatewayURL:       "http://gateway.test",
		LLMGatewayAPIKey:    "test-key",
		GeminiEmbeddingDims: 1,
	}
	for _, args := range [][]string{nil, {"--openrouter-only"}} {
		cmd := newBackfillCmd(func() *config.Config { return cfg })
		flag, err := cmd.Flags().GetBool("openrouter-only")
		if err != nil || flag {
			t.Fatalf("flag default = %v, %v", flag, err)
		}
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
	}
	if len(received) != 2 || received[0] || !received[1] {
		t.Fatalf("openrouter-only values = %v, want [false true]", received)
	}
}

func TestBackfillCommandLoadsMissingGatewaySettings(t *testing.T) {
	originalLoader := loadBackfillRuntimeSettings
	originalRun := runBackfill
	t.Cleanup(func() {
		loadBackfillRuntimeSettings = originalLoader
		runBackfill = originalRun
	})

	t.Setenv("LLM_GATEWAY_URL", "")

	cfg := &config.Config{
		DatabaseURL:         "postgres://user:pass@127.0.0.1:1/agentmem",
		GeminiEmbeddingDims: 1,
	}
	loadCalls := 0
	loadBackfillRuntimeSettings = func(_ context.Context, loaded *config.Config) error {
		loadCalls++
		loaded.LLMGatewayURL = "http://gateway.test"
		loaded.LLMGatewayAPIKey = "loaded-key"
		return nil
	}
	var receivedKey string
	runBackfill = func(_, key, _ string, _ int, _ bool) error {
		receivedKey = key
		return nil
	}

	cmd := newBackfillCmd(func() *config.Config { return cfg })
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if loadCalls != 1 {
		t.Fatalf("loader calls = %d, want 1", loadCalls)
	}
	if receivedKey != "loaded-key" {
		t.Fatalf("runBackfill key = %q, want loaded key", receivedKey)
	}
	if got := os.Getenv("LLM_GATEWAY_URL"); got != "http://gateway.test" {
		t.Fatalf("LLM_GATEWAY_URL = %q, want loaded URL", got)
	}
}

func TestBackfillCommandSettingsLoaderConditions(t *testing.T) {
	for _, tc := range []struct {
		name           string
		cfg            *config.Config
		loadErr        error
		clearAfterLoad bool
		wantLoad       bool
		wantRun        bool
		wantErr        string
	}{
		{
			name: "configured values skip loader",
			cfg: &config.Config{
				LLMGatewayURL:    "http://gateway.test",
				LLMGatewayAPIKey: "configured-key",
			},
			wantRun: true,
		},
		{
			name:     "loader error is returned",
			cfg:      &config.Config{},
			loadErr:  errors.New("settings unavailable"),
			wantLoad: true,
			wantErr:  "settings unavailable",
		},
		{
			name:           "missing loaded value retains required error",
			cfg:            &config.Config{},
			clearAfterLoad: true,
			wantLoad:       true,
			wantErr:        "llm_gateway_url and llm_gateway_api_key required",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			originalLoader := loadBackfillRuntimeSettings
			originalRun := runBackfill
			t.Cleanup(func() {
				loadBackfillRuntimeSettings = originalLoader
				runBackfill = originalRun
			})

			loadCalls := 0
			loadBackfillRuntimeSettings = func(_ context.Context, cfg *config.Config) error {
				loadCalls++
				if tc.loadErr != nil {
					return tc.loadErr
				}
				cfg.LLMGatewayURL = "http://gateway.test"
				cfg.LLMGatewayAPIKey = "loaded-key"
				if tc.clearAfterLoad {
					cfg.LLMGatewayAPIKey = ""
				}
				return nil
			}
			runCalls := 0
			runBackfill = func(_, _, _ string, _ int, _ bool) error {
				runCalls++
				return nil
			}

			cmd := newBackfillCmd(func() *config.Config { return tc.cfg })
			err := cmd.Execute()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, tc.wantErr)
			}
			if loadCalls != btoi(tc.wantLoad) {
				t.Fatalf("loader calls = %d, want %d", loadCalls, btoi(tc.wantLoad))
			}
			if runCalls != btoi(tc.wantRun) {
				t.Fatalf("runBackfill calls = %d, want %d", runCalls, btoi(tc.wantRun))
			}
		})
	}
}

func btoi(value bool) int {
	if value {
		return 1
	}
	return 0
}

func TestRunBackfillEmbeddingsPropagatesRequestOptions(t *testing.T) {
	original := backfillAll
	t.Cleanup(func() { backfillAll = original })
	backfillAll = func(ctx context.Context, _ *pgxpool.Pool, client batchEmbedder) error {
		for range 3 {
			if _, err := client.EmbedBatch(ctx, []string{"one"}); err != nil {
				return err
			}
		}
		return nil
	}

	for _, openrouterOnly := range []bool{true, false} {
		srv, requests := captureEmbedRequests(t)
		t.Setenv("LLM_GATEWAY_URL", srv.URL)
		if err := runBackfillEmbeddings("postgres://user:pass@127.0.0.1:1/agentmem", "key", "", 1, openrouterOnly); err != nil {
			t.Fatal(err)
		}
		if len(*requests) != 3 {
			t.Fatalf("requests = %d, want 3", len(*requests))
		}
		for i, request := range *requests {
			if openrouterOnly {
				if request.Fallback == nil || *request.Fallback || request.Caller != "agent-mem/backfill" {
					t.Errorf("request %d = %+v, want fallback false and caller", i, request)
				}
			} else if request.Fallback != nil || request.Caller != "" {
				t.Errorf("request %d = %+v, want defaults", i, request)
			}
		}
	}
}
