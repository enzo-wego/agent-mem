package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agent-mem/agent-mem/internal/config"
	"github.com/agent-mem/agent-mem/internal/graphmcp"
)

func TestMCPCmd_UsesWorkerURLAndEnvironmentAPIKeyWithoutDatabase(t *testing.T) {
	t.Setenv("AGENT_MEM_API_KEY", "environment-secret")
	t.Setenv("DATABASE_URL", "postgresql://invalid.invalid/should-not-connect")

	var authorization string
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		authorization = request.Header.Get("Authorization")
		if request.URL.Path != "/api/settings" {
			t.Errorf("probe path = %q", request.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer worker.Close()

	runnerCalled := false
	command := newMCPCmdWithRunner(
		config.Load,
		func(_ context.Context, _ *graphmcp.Client) error {
			runnerCalled = true
			return nil
		},
	)
	command.SetArgs([]string{"--worker-url", worker.URL})
	if err := command.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !runnerCalled {
		t.Fatal("MCP runner was not called")
	}
	if authorization != "Bearer environment-secret" {
		t.Fatalf("Authorization = %q", authorization)
	}
}

func TestMCPCmd_ProbeFailureIsClear(t *testing.T) {
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "wrong key", http.StatusUnauthorized)
	}))
	defer worker.Close()

	command := newMCPCmdWithRunner(
		func() *config.Config {
			cfg := config.Load()
			cfg.APIKey = "bad-secret"
			return cfg
		},
		func(_ context.Context, _ *graphmcp.Client) error {
			t.Fatal("runner called after failed probe")
			return nil
		},
	)
	command.SetArgs([]string{"--worker-url", worker.URL})
	err := command.ExecuteContext(context.Background())
	if err == nil || !strings.Contains(err.Error(), "probe worker") || !strings.Contains(err.Error(), "401") {
		t.Fatalf("error = %v", err)
	}
}

func TestMCPCmd_RequiresAPIKeyAfterRuntimeLoad(t *testing.T) {
	command := newMCPCmdWithRuntimeLoader(
		func() *config.Config { return config.Load() },
		func(_ context.Context, cfg *config.Config) error {
			cfg.APIKey = ""
			return nil
		},
		func(_ context.Context, _ *graphmcp.Client) error {
			t.Fatal("runner called without API key")
			return nil
		},
	)
	command.SetArgs([]string{"--worker-url", "http://127.0.0.1:1"})
	err := command.ExecuteContext(context.Background())
	if err == nil || !strings.Contains(err.Error(), "--allow-unauthenticated") {
		t.Fatalf("error = %v", err)
	}
}
