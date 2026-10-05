package worker

import (
	"context"
	"os"
	"testing"

	"github.com/agent-mem/agent-mem/internal/config"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Construct the server without starting its HTTP server or dispatchers. Startup
// must not seed fixed-interval jobs outside the ticker's transaction.
func TestStartup_NoSeedingForPeriodic(t *testing.T) {
	t.Chdir("../..")
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Fatal("DATABASE_URL is required for periodic startup integration tests")
	}
	poolConfig, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	if poolConfig.ConnConfig.Host != "127.0.0.1" || poolConfig.ConnConfig.Port != 5446 || poolConfig.ConnConfig.Database != "agentmem_test" {
		t.Fatal("periodic startup tests require the isolated 127.0.0.1:5446/agentmem_test database")
	}
	ctx := context.Background()
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `DELETE FROM graph.jobs`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(ctx, `DELETE FROM graph.jobs`); err != nil {
			t.Error(err)
		}
	})
	for _, key := range []string{
		"AGENT_MEM_LLM_GATEWAY_URL", "AGENT_MEM_LLM_GATEWAY_API_KEY",
		"AGENT_MEM_SLACK_BOT_TOKEN", "AGENT_MEM_SLACK_DM_USER",
		"AGENT_MEM_JIRA_EMAIL", "AGENT_MEM_JIRA_TOKEN",
	} {
		t.Setenv(key, "")
	}
	t.Setenv("AGENT_MEM_SYNC_ENABLED", "false")
	t.Setenv("AGENT_MEM_GRAPH_RUNNER", "local")
	cfg := &config.Config{DatabaseURL: dsn, DataDir: t.TempDir(), MachineID: "periodic-startup-test"}
	cfg.Graph.Runner = "local"
	server, err := NewServer(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer server.db.Pool.Close()
	var pending int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM graph.jobs
		WHERE type IN ('notify_watch_channels','detect_hot_topics','derive_person_roles','refresh_jira_board')`).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 0 {
		t.Fatalf("constructor seeded %d periodic jobs; ticker must own first enqueue", pending)
	}
}
