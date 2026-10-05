package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-mem/agent-mem/internal/config"
	"github.com/agent-mem/agent-mem/internal/database"
	"github.com/agent-mem/agent-mem/internal/graph/handlers"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestReindexHeuristicCLI_FlagValidation(t *testing.T) {
	for _, args := range [][]string{{}, {"--since", "bad"}, {"--since", "2026-01-01T00:00:00Z", "--interval-ms", "49"}, {"--since", "2026-01-01T00:00:00Z", "--interval-ms", "5001"}, {"--since", "2026-01-01T00:00:00Z", "--max-rows", "-1"}} {
		cmd := newReindexHeuristicCmd(func() *config.Config { return &config.Config{} })
		cmd.SetArgs(args)
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&bytes.Buffer{})
		if err := cmd.Execute(); err == nil {
			t.Fatalf("accepted %v", args)
		}
		if !strings.Contains(out.String(), "final") {
			t.Fatalf("missing validation summary for %v: %s", args, &out)
		}
	}
	cmd := newReindexHeuristicCmd(func() *config.Config { return &config.Config{} })
	interval, err := cmd.Flags().GetInt("interval-ms")
	if err != nil || interval != 300 {
		t.Fatalf("interval default = %d, %v", interval, err)
	}
	maxRows, err := cmd.Flags().GetInt("max-rows")
	if err != nil || maxRows != 0 {
		t.Fatalf("max rows default = %d, %v", maxRows, err)
	}
}

func reindexCLITestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ConnConfig.Database != "agentmem_test" {
		t.Fatal("refusing to run: reindex CLI tests require database agentmem_test; tests may delete graph rows")
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `DELETE FROM graph.jobs; DELETE FROM graph.eligibility_decisions; DELETE FROM graph.artifact_index; DELETE FROM graph.artifact_bodies; DELETE FROM graph.jira_epic_map; DELETE FROM graph.pinned_threads; DELETE FROM graph.edges; DELETE FROM graph.nodes`); err != nil {
		t.Fatal(err)
	}
	return pool
}

func TestReindexHeuristicCLI_DBEnvConfigurationAndGraphDimensions(t *testing.T) {
	pool := reindexCLITestDB(t)
	ctx := context.Background()
	db := database.NewDB(pool)
	old, err := db.GetAllSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	keys := []string{"llm_gateway_url", "llm_gateway_api_key", "machine_id"}
	t.Cleanup(func() {
		for _, key := range keys {
			if value, ok := old[key]; ok {
				if err := db.SaveSetting(ctx, key, value); err != nil {
					t.Error(err)
				}
			} else if _, err := pool.Exec(ctx, `DELETE FROM public.settings WHERE key=$1`, key); err != nil {
				t.Error(err)
			}
		}
	})
	t.Setenv("AGENT_MEM_LLM_GATEWAY_URL", "")
	t.Setenv("AGENT_MEM_LLM_GATEWAY_API_KEY", "")
	t.Setenv("AGENT_MEM_MACHINE_ID", "")
	var dbCalls, envCalls atomic.Int32
	gateway := func(calls *atomic.Int32, key string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			if r.URL.Path != "/embed" {
				t.Errorf("unexpected chat path %s", r.URL.Path)
			}
			if r.Header.Get("X-API-Key") != key {
				t.Errorf("API key = %q", r.Header.Get("X-API-Key"))
			}
			var req struct {
				Texts []string `json:"texts"`
				Dims  int      `json:"dims"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Error(err)
			}
			if req.Dims != handlers.GraphEmbeddingDims {
				t.Errorf("dims = %d", req.Dims)
			}
			v := make([]float32, handlers.GraphEmbeddingDims)
			v[0] = 1
			if err := json.NewEncoder(w).Encode(map[string]any{"embeddings": [][]float32{v}}); err != nil {
				t.Error(err)
			}
		}))
	}
	dbServer := gateway(&dbCalls, "db-key")
	defer dbServer.Close()
	envServer := gateway(&envCalls, "env-key")
	defer envServer.Close()
	if err := db.SaveSettings(ctx, map[string]string{"llm_gateway_url": dbServer.URL, "llm_gateway_api_key": "db-key", "machine_id": "db-machine"}); err != nil {
		t.Fatal(err)
	}
	seed := func() time.Time {
		if _, err := pool.Exec(ctx, `DELETE FROM graph.jobs; DELETE FROM graph.artifact_index; DELETE FROM graph.nodes; INSERT INTO graph.nodes (id,type,natural_key,title,body,machine_id) VALUES ('jira:PAY-990','jira','PAY-990','Config repair','','test')`); err != nil {
			t.Fatal(err)
		}
		var cutoff time.Time
		if err := pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&cutoff); err != nil {
			t.Fatal(err)
		}
		return cutoff
	}
	for _, source := range []string{"DB", "env"} {
		if source == "env" {
			t.Setenv("AGENT_MEM_LLM_GATEWAY_URL", envServer.URL)
			t.Setenv("AGENT_MEM_LLM_GATEWAY_API_KEY", "env-key")
			t.Setenv("AGENT_MEM_MACHINE_ID", "env-machine")
		}
		opts := handlers.ReindexHeuristicOptions{Since: seed(), IntervalMS: 50, Output: &bytes.Buffer{}}
		result, err := runReindexHeuristic(ctx, config.Load(), opts)
		if err != nil || result.Done != 1 {
			t.Fatalf("%s configuration = %+v, %v", source, result, err)
		}
		var machine string
		if err := pool.QueryRow(ctx, `SELECT machine_id FROM graph.artifact_index WHERE node_id='jira:PAY-990'`).Scan(&machine); err != nil {
			t.Fatal(err)
		}
		if machine != strings.ToLower(source)+"-machine" {
			t.Fatalf("machine = %s", machine)
		}
	}
	if dbCalls.Load() != 2 || envCalls.Load() != 2 {
		t.Fatalf("DB/env embed calls = %d/%d", dbCalls.Load(), envCalls.Load())
	}
	if err := db.SaveSetting(ctx, "llm_gateway_url", ""); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_MEM_LLM_GATEWAY_URL", "")
	cutoff := seed()
	var out bytes.Buffer
	opts := handlers.ReindexHeuristicOptions{Since: cutoff, IntervalMS: 50, Output: &out}
	if _, err := runReindexHeuristic(ctx, config.Load(), opts); err == nil || !strings.Contains(err.Error(), "gateway URL") {
		t.Fatalf("empty gateway: %v", err)
	}
	if !strings.Contains(out.String(), "final") {
		t.Fatalf("missing error final output: %s", &out)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM graph.artifact_index`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("empty gateway modified index: %d, %v", count, err)
	}
	opts.DryRun = true
	result, err := runReindexHeuristic(ctx, config.Load(), opts)
	if err != nil || result.Eligible != 1 || result.Done != 0 {
		t.Fatalf("gateway-free dryrun = %+v,%v", result, err)
	}
}

func TestReindexHeuristicCLI_ProbeFailureUntouched(t *testing.T) {
	for _, response := range []string{"error", "empty vector"} {
		t.Run(response, func(t *testing.T) {
			pool := reindexCLITestDB(t)
			ctx := context.Background()
			if _, err := pool.Exec(ctx, `INSERT INTO graph.nodes (id,type,natural_key,title,body,machine_id) VALUES ('jira:PAY-991','jira','PAY-991','Untouched title','','test')`); err != nil {
				t.Fatal(err)
			}
			var cutoff time.Time
			if err := pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&cutoff); err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if response == "error" {
					http.Error(w, "probe rejected", http.StatusBadRequest)
					return
				}
				fmtResponse := `{"embeddings":[[]]}`
				if _, err := w.Write([]byte(fmtResponse)); err != nil {
					t.Error(err)
				}
			}))
			defer server.Close()
			t.Setenv("AGENT_MEM_LLM_GATEWAY_URL", server.URL)
			t.Setenv("AGENT_MEM_LLM_GATEWAY_API_KEY", "test-key")
			var out bytes.Buffer
			result, err := runReindexHeuristic(ctx, config.Load(), handlers.ReindexHeuristicOptions{Since: cutoff, IntervalMS: 50, Output: &out})
			if err == nil || result.Done != 0 || calls.Load() != 1 || !strings.Contains(out.String(), "final") {
				t.Fatalf("probe = %+v,%v,calls=%d,output=%s", result, err, calls.Load(), &out)
			}
			var indexes, jobs int
			if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM graph.artifact_index),(SELECT count(*) FROM graph.jobs)`).Scan(&indexes, &jobs); err != nil {
				t.Fatal(err)
			}
			if indexes != 0 || jobs != 0 {
				t.Fatalf("probe changed rows: indexes=%d jobs=%d", indexes, jobs)
			}
		})
	}
}
