package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/agent-mem/agent-mem/internal/config"
	"github.com/agent-mem/agent-mem/internal/database"
	"github.com/agent-mem/agent-mem/internal/graphmcp"
)

type mcpRuntimeLoader func(context.Context, *config.Config) error
type mcpRunner func(context.Context, *graphmcp.Client) error

// newMCPCmd creates the stdio MCP command. getCfg is evaluated from RunE, after
// the root command's PersistentPreRun has loaded environment configuration.
func newMCPCmd(getCfg func() *config.Config) *cobra.Command {
	return newMCPCmdWithRunner(getCfg, func(ctx context.Context, client *graphmcp.Client) error {
		server := graphmcp.NewServer(client, version)
		return server.Run(ctx, &mcp.StdioTransport{})
	})
}

func newMCPCmdWithRunner(getCfg func() *config.Config, runner mcpRunner) *cobra.Command {
	return newMCPCmdWithRuntimeLoader(getCfg, loadMCPRuntimeSettings, runner)
}

func newMCPCmdWithRuntimeLoader(
	getCfg func() *config.Config,
	loadRuntime mcpRuntimeLoader,
	runner mcpRunner,
) *cobra.Command {
	var workerURL string
	var allowUnauthenticated bool

	command := &cobra.Command{
		Use:   "mcp",
		Short: "Serve graph read tools over MCP stdio",
		Long: "Serves five trusted operator graph tools over stdin/stdout. " +
			"Protocol frames are the only output written to stdout.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg := getCfg()
			if cfg == nil {
				return fmt.Errorf("configuration is not loaded")
			}
			if err := loadRuntime(cmd.Context(), cfg); err != nil {
				return err
			}
			if strings.TrimSpace(cfg.APIKey) == "" && !allowUnauthenticated {
				return fmt.Errorf("worker API key is empty; set AGENT_MEM_API_KEY or use --allow-unauthenticated for local development")
			}

			targetURL := workerURL
			if targetURL == "" {
				targetURL = fmt.Sprintf("http://127.0.0.1:%d", cfg.WorkerPort)
			}
			client, err := graphmcp.NewClient(targetURL, cfg.APIKey, nil)
			if err != nil {
				return err
			}

			probeContext, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
			defer cancel()
			if err := client.Probe(probeContext); err != nil {
				return fmt.Errorf("probe worker %s: %w", targetURL, err)
			}
			return runner(cmd.Context(), client)
		},
	}
	command.Flags().StringVar(&workerURL, "worker-url", "", "Worker HTTP base URL (defaults to the configured localhost worker port)")
	command.Flags().BoolVar(&allowUnauthenticated, "allow-unauthenticated", false, "Allow an empty worker API key for local development")
	return command
}

func loadMCPRuntimeSettings(ctx context.Context, cfg *config.Config) error {
	if strings.TrimSpace(cfg.APIKey) != "" {
		return nil
	}

	pool, err := database.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("load MCP runtime settings: connect database: %w", err)
	}
	defer pool.Close()

	settings, err := database.NewDB(pool).GetAllSettings(ctx)
	if err != nil {
		return fmt.Errorf("load MCP runtime settings: %w", err)
	}
	cfg.ApplyDBSettings(settings)
	config.ApplyEnv(cfg)
	return nil
}
