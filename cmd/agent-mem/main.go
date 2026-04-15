package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/spf13/cobra"

	"github.com/agent-mem/agent-mem/internal/codexinstall"
	"github.com/agent-mem/agent-mem/internal/config"
	"github.com/agent-mem/agent-mem/internal/database"
	"github.com/agent-mem/agent-mem/internal/hooks"
	"github.com/agent-mem/agent-mem/internal/skills"
	"github.com/agent-mem/agent-mem/internal/worker"
)

var version = "dev"

func main() {
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})

	var cfg *config.Config

	rootCmd := &cobra.Command{
		Use:   "agent-mem",
		Short: "Persistent memory for coding agents",
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

	var installHooksFile string
	var installHooksProjectDir string
	var installHooksScope string
	installHooksCmd := &cobra.Command{
		Use:   "install-hooks [provider]",
		Short: "Install agent-mem hook entries into a supported agent config",
		Long:  "Safely merges the managed agent-mem hook entries into the selected coding agent's local hooks config. Supports user-global, project-local, or explicit hooks-file targets. Currently supports Codex.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			provider := hooks.ProviderCodex
			if len(args) == 1 {
				provider = strings.ToLower(strings.TrimSpace(args[0]))
			}

			result, err := hooks.InstallHooksWithOptions(provider, hooks.InstallOptions{
				HooksPath:  installHooksFile,
				ProjectDir: installHooksProjectDir,
				Scope:      installHooksScope,
			})
			if err != nil {
				return err
			}

			if result.Changed {
				fmt.Fprintf(cmd.OutOrStdout(), "installed agent-mem %s hooks in %s", provider, result.Path)
				if len(result.ChangedEvents) > 0 {
					fmt.Fprintf(cmd.OutOrStdout(), " (%s)", strings.Join(result.ChangedEvents, ", "))
				}
				fmt.Fprintln(cmd.OutOrStdout())
				return nil
			}

			fmt.Fprintf(cmd.OutOrStdout(), "agent-mem %s hooks already installed in %s\n", provider, result.Path)
			return nil
		},
	}
	installHooksCmd.Flags().StringVar(&installHooksFile, "hooks-file", "", "Override the hooks config path (defaults to the provider's standard user config)")
	installHooksCmd.Flags().StringVar(&installHooksScope, "scope", "user", "Install scope: user or project")
	installHooksCmd.Flags().StringVar(&installHooksProjectDir, "project-dir", "", "Project root to use with --scope project (defaults to the current working directory)")

	var installSkillScope string
	var installSkillProjectDir string
	var installSkillSourceDir string
	installSkillCmd := &cobra.Command{
		Use:   "install-skill [name]",
		Short: "Install a plugin skill into a Codex-recognized skills directory",
		Long:  "Copies a plugin skill directory into either the user-global or project-local .codex skills directory so Codex can discover it.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			result, err := skills.Install(args[0], skills.InstallOptions{
				Scope:      installSkillScope,
				ProjectDir: installSkillProjectDir,
				SourceDir:  installSkillSourceDir,
			})
			if err != nil {
				return err
			}

			fmt.Fprintf(
				cmd.OutOrStdout(),
				"installed skill %s from %s to %s\n",
				result.Name,
				result.Source,
				result.Target,
			)
			return nil
		},
	}
	installSkillCmd.Flags().StringVar(&installSkillScope, "scope", "project", "Install scope: user or project")
	installSkillCmd.Flags().StringVar(&installSkillProjectDir, "project-dir", "", "Project root to use with --scope project (defaults to the current working directory)")
	installSkillCmd.Flags().StringVar(&installSkillSourceDir, "source-dir", "", "Override the source skill directory (defaults to ./plugin/skills/<name>)")

	var installCodexScope string
	var installCodexProjectDir string
	var installCodexHooksFile string
	var installCodexPluginSkillsDir string
	installCodexCmd := &cobra.Command{
		Use:   "codex",
		Short: "Install agent-mem Codex hooks and plugin skills together",
		Long:  "Installs the agent-mem Codex hook config plus all plugin skills into the selected Codex scope.",
		RunE: func(cmd *cobra.Command, args []string) error {
			result, err := codexinstall.Install(codexinstall.InstallOptions{
				Scope:           installCodexScope,
				ProjectDir:      installCodexProjectDir,
				HooksPath:       installCodexHooksFile,
				PluginSkillsDir: installCodexPluginSkillsDir,
			})
			if err != nil {
				return err
			}

			fmt.Fprintf(cmd.OutOrStdout(), "installed Codex hooks in %s\n", result.Hooks.Path)
			for _, skill := range result.Skills {
				fmt.Fprintf(cmd.OutOrStdout(), "installed Codex skill %s in %s\n", skill.Name, skill.Target)
			}
			return nil
		},
	}
	installCodexCmd.Flags().StringVar(&installCodexScope, "scope", "project", "Install scope: user or project")
	installCodexCmd.Flags().StringVar(&installCodexProjectDir, "project-dir", "", "Project root to use with --scope project (defaults to the current working directory)")
	installCodexCmd.Flags().StringVar(&installCodexHooksFile, "hooks-file", "", "Override the hooks config path")
	installCodexCmd.Flags().StringVar(&installCodexPluginSkillsDir, "plugin-skills-dir", "", "Override the plugin skills directory (defaults to the skills embedded in the agent-mem binary)")

	installCmd := &cobra.Command{
		Use:   "install",
		Short: "Install agent-mem integration surfaces into supported coding agents",
	}
	installCmd.AddCommand(installCodexCmd)

	// hook <event> [provider]
	hookCmd := &cobra.Command{
		Use:   "hook [event] [provider]",
		Short: "Run an agent-mem hook adapter",
		Long:  "Reads stdin JSON from a supported coding agent hook, normalizes the payload, POSTs to the worker, and writes response to stdout. Provider defaults to claude.",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) < 1 || len(args) > 2 {
				return cobra.RangeArgs(1, 2)(cmd, args)
			}
			switch args[0] {
			case "session-start", "prompt-submit", "post-tool-use", "stop":
			default:
				return fmt.Errorf("unsupported hook event %q", args[0])
			}
			if len(args) == 2 {
				if _, err := hooks.NormalizeProvider(args[1]); err != nil {
					return err
				}
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			provider := ""
			if len(args) == 2 {
				provider = args[1]
			}
			return hooks.RunHook(args[0], provider, cfg.WorkerPort)
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
		installCmd,
		installHooksCmd,
		installSkillCmd,
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
