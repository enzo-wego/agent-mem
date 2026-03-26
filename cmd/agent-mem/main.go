package main

import (
	"fmt"
	"io"
	"os"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"

	"github.com/agent-mem/agent-mem/internal/config"
	"github.com/agent-mem/agent-mem/internal/database"
	"github.com/agent-mem/agent-mem/internal/hooks"
	"github.com/agent-mem/agent-mem/internal/worker"
)

var version = "dev"

func main() {
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})

	var cfg *config.Config

	rootCmd := &cobra.Command{
		Use:   "agent-mem",
		Short: "Persistent memory for Claude Code",
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			cfg = config.Load()
			level, err := zerolog.ParseLevel(cfg.LogLevel)
			if err != nil {
				level = zerolog.InfoLevel
			}
			zerolog.SetGlobalLevel(level)
		},
	}

	// version
	versionCmd := &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Printf("agent-mem %s\n", version)
		},
	}

	// worker
	logBuf := worker.NewLogBuffer(5000) // keep last 5000 log entries in memory
	workerCmd := &cobra.Command{
		Use:   "worker",
		Short: "Start the HTTP worker server",
		RunE: func(cmd *cobra.Command, args []string) error {
			// Tee logs to both stderr and the in-memory buffer
			console := zerolog.ConsoleWriter{Out: os.Stderr}
			bufWriter := zerolog.ConsoleWriter{Out: logBuf, NoColor: true}
			log.Logger = log.Output(io.MultiWriter(console, bufWriter))

			srv, err := worker.NewServer(cfg, logBuf)
			if err != nil {
				return err
			}
			return srv.Run()
		},
	}

	// hook <event>
	hookCmd := &cobra.Command{
		Use:       "hook [event]",
		Short:     "Run a Claude Code hook",
		Long:      "Reads stdin JSON from Claude Code, POSTs to the worker, and writes response to stdout.",
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"session-start", "prompt-submit", "post-tool-use", "stop"},
		RunE: func(cmd *cobra.Command, args []string) error {
			return hooks.RunHook(args[0], cfg.WorkerPort)
		},
	}

	// migrate - run all pending goose migrations
	migrateCmd := &cobra.Command{
		Use:   "migrate",
		Short: "Run all pending database migrations",
		RunE: func(cmd *cobra.Command, args []string) error {
			return database.RunMigrations(cfg.DatabaseURL)
		},
	}

	// migrate-create <name> - create a new migration file
	migrateCreateCmd := &cobra.Command{
		Use:   "migrate-create [name]",
		Short: "Create a new migration file",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return database.MigrateCreate(args[0])
		},
	}

	// migrate-status - print migration status
	migrateStatusCmd := &cobra.Command{
		Use:   "migrate-status",
		Short: "Print the status of all migrations",
		RunE: func(cmd *cobra.Command, args []string) error {
			return database.MigrateStatus(cfg.DatabaseURL)
		},
	}

	// migrate-rollback - rollback last or specific migration
	var rollbackVersion int64
	migrateRollbackCmd := &cobra.Command{
		Use:   "migrate-rollback",
		Short: "Rollback the last migration",
		RunE: func(cmd *cobra.Command, args []string) error {
			return database.MigrateRollback(cfg.DatabaseURL, rollbackVersion)
		},
	}
	migrateRollbackCmd.Flags().Int64VarP(&rollbackVersion, "version", "v", 0, "Target version to rollback to (0 = rollback last)")

	// migrate-up-by-one - apply next pending migration
	migrateUpByOneCmd := &cobra.Command{
		Use:   "migrate-up-by-one",
		Short: "Apply the next pending migration",
		RunE: func(cmd *cobra.Command, args []string) error {
			return database.MigrateUpByOne(cfg.DatabaseURL)
		},
	}

	// migrate-fix - force-delete a failed migration record
	var fixVersion int64
	migrateFixCmd := &cobra.Command{
		Use:   "migrate-fix",
		Short: "Force-delete a failed migration record so it can be re-run",
		RunE: func(cmd *cobra.Command, args []string) error {
			if fixVersion == 0 {
				return fmt.Errorf("--version is required (e.g. --version 20260323000000)")
			}
			return database.MigrateFix(cfg.DatabaseURL, fixVersion)
		},
	}
	migrateFixCmd.Flags().Int64VarP(&fixVersion, "version", "v", 0, "Migration version to delete from goose tracking table")

	// migrate-sqlite - one-time SQLite to PostgreSQL data migration (legacy)
	var sqlitePath string
	migrateSqliteCmd := &cobra.Command{
		Use:   "migrate-sqlite",
		Short: "Migrate data from claude-mem SQLite to PostgreSQL (one-time)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if sqlitePath == "" {
				home, _ := os.UserHomeDir()
				sqlitePath = home + "/.claude-mem/claude-mem.db"
			}
			return runMigrate(sqlitePath, cfg.DatabaseURL, cfg.GeminiAPIKey)
		},
	}
	migrateSqliteCmd.Flags().StringVar(&sqlitePath, "sqlite-path", "", "Path to claude-mem SQLite database (default: ~/.claude-mem/claude-mem.db)")

	// backfill-embeddings - generate Gemini embeddings for rows missing them
	backfillCmd := &cobra.Command{
		Use:   "backfill-embeddings",
		Short: "Generate Gemini embeddings for all rows without them",
		RunE: func(cmd *cobra.Command, args []string) error {
			if cfg.GeminiAPIKey == "" {
				return fmt.Errorf("Gemini API key required (set GEMINI_API_KEY or AGENT_MEM_GEMINI_API_KEY)")
			}
			return runBackfillEmbeddings(cfg.DatabaseURL, cfg.GeminiAPIKey, cfg.GeminiEmbeddingModel, cfg.GeminiEmbeddingDims)
		},
	}

	rootCmd.AddCommand(
		versionCmd,
		workerCmd,
		hookCmd,
		migrateCmd,
		migrateCreateCmd,
		migrateStatusCmd,
		migrateRollbackCmd,
		migrateUpByOneCmd,
		migrateFixCmd,
		migrateSqliteCmd,
		backfillCmd,
	)

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
