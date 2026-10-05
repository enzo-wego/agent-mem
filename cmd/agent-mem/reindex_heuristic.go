package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"github.com/spf13/cobra"

	"github.com/agent-mem/agent-mem/internal/config"
	"github.com/agent-mem/agent-mem/internal/database"
	"github.com/agent-mem/agent-mem/internal/graph/handlers"
	"github.com/agent-mem/agent-mem/internal/llmgateway"
)

func newReindexHeuristicCmd(getCfg func() *config.Config) *cobra.Command {
	var since string
	opts := handlers.ReindexHeuristicOptions{}
	cmd := &cobra.Command{
		Use:   "reindex-heuristic",
		Short: "Repair heuristic Jira, PR and Confluence indexes without LLM judging",
		Long:  "Serially re-index heuristic resource rows and titled empty-body resources missing an index. Take --since after the new worker is running and all old index writers are stopped; resume with the same cutoff. No worker server or queue continuation is started.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			opts.Output = cmd.OutOrStdout()
			delegated := false
			started := time.Now()
			defer func() {
				if !delegated {
					fmt.Fprintf(opts.Output, "final done=0 skipped=0 eligible=0 elapsed=%s last_id=\n", time.Since(started).Round(time.Millisecond))
				}
			}()
			if since == "" {
				return fmt.Errorf("--since is required (RFC3339)")
			}
			cutoff, err := time.Parse(time.RFC3339, since)
			if err != nil {
				return fmt.Errorf("--since must be RFC3339: %w", err)
			}
			opts.Since = cutoff
			if err := opts.Validate(); err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			delegated = true
			_, err = runReindexHeuristic(ctx, getCfg(), opts)
			return err
		},
	}
	cmd.Flags().StringVar(&since, "since", "", "Required fixed cutoff (RFC3339); skip indexes refreshed at or after it")
	cmd.Flags().IntVar(&opts.IntervalMS, "interval-ms", 300, "Delay between nodes in milliseconds (50–5000)")
	cmd.Flags().IntVar(&opts.MaxRows, "max-rows", 0, "Maximum attempted rows (0 means all)")
	cmd.Flags().BoolVar(&opts.DryRun, "dry-run", false, "Count eligible rows and list the first 20 IDs without changing rows or embedding")
	return cmd
}

// runReindexHeuristic builds only indexing dependencies. The caller bootstraps
// cfg with config.Load; runtime settings and env precedence match the worker.
func runReindexHeuristic(ctx context.Context, cfg *config.Config, opts handlers.ReindexHeuristicOptions) (result handlers.ReindexHeuristicResult, runErr error) {
	started := time.Now()
	delegated := false
	defer func() {
		if !delegated && opts.Output != nil {
			result.Elapsed = time.Since(started)
			fmt.Fprintf(opts.Output, "cutoff=%s\nfinal done=0 skipped=0 eligible=0 elapsed=%s last_id=\n", opts.Since.UTC().Format(time.RFC3339Nano), result.Elapsed.Round(time.Millisecond))
		}
	}()
	if err := opts.Validate(); err != nil {
		return result, err
	}
	pool, err := database.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return result, fmt.Errorf("open database: %w", err)
	}
	defer pool.Close()
	settings, err := database.NewDB(pool).GetAllSettings(ctx)
	if err != nil {
		return result, fmt.Errorf("load runtime settings: %w", err)
	}
	if len(settings) > 0 {
		cfg.ApplyDBSettings(settings)
	}
	config.ApplyEnv(cfg)
	snap := cfg.Snapshot()
	deps := handlers.Deps{
		DB:        pool,
		Logger:    zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr}).With().Timestamp().Logger(),
		MachineID: snap.MachineID,
	}
	if !opts.DryRun {
		if strings.TrimSpace(snap.LLMGatewayURL) == "" {
			return result, fmt.Errorf("reindex-heuristic: gateway URL is empty; configure llm_gateway_url or AGENT_MEM_LLM_GATEWAY_URL")
		}
		deps.Gemini = llmgateway.New(snap.LLMGatewayURL, snap.LLMGatewayAPIKey, handlers.GraphEmbeddingDims)
	}
	delegated = true
	return handlers.RunReindexHeuristic(ctx, deps, opts)
}
