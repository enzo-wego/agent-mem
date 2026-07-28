package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"

	"github.com/rs/zerolog"
	"github.com/spf13/cobra"

	"github.com/agent-mem/agent-mem/internal/config"
	"github.com/agent-mem/agent-mem/internal/database"
	"github.com/agent-mem/agent-mem/internal/graph/entities"
	"github.com/agent-mem/agent-mem/internal/graph/jobs"
)

// newEntitiesCmd returns the "entities" cobra parent command with its
// seed-partners, load-csv, and list subcommands.
// getCfg is a closure returning the already-loaded *config.Config so that
// PersistentPreRun has run before these commands execute.
func newEntitiesCmd(getCfg func() *config.Config) *cobra.Command {
	entitiesCmd := &cobra.Command{
		Use:   "entities",
		Short: "Manage graph entity seed data (partners, features, statuses, currencies)",
	}

	// entities seed-partners
	var seedPartnersPath string
	seedPartnersCmd := &cobra.Command{
		Use:   "seed-partners",
		Short: "Walk <path>/pkg/payment/ and seed partner entities",
		Long:  "Idempotently upserts one graph.entities row per sub-directory found in <path>/pkg/payment/.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if seedPartnersPath == "" {
				seedPartnersPath = os.Getenv("WEGO_PAYMENTS_PATH")
			}
			if seedPartnersPath == "" {
				seedPartnersPath = os.Getenv("AGENT_MEM_GRAPH_PAYMENTS_PATH")
			}
			if seedPartnersPath == "" {
				return fmt.Errorf("--path is required (or set WEGO_PAYMENTS_PATH / AGENT_MEM_GRAPH_PAYMENTS_PATH)")
			}

			ctx := context.Background()
			db, err := database.Connect(ctx, getCfg().DatabaseURL)
			if err != nil {
				return fmt.Errorf("open db: %w", err)
			}
			defer db.Close()

			log := zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr}).With().Timestamp().Logger()
			count, err := entities.SeedFromPaymentsRepo(ctx, db, seedPartnersPath, log)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "seeded %d partner entities from %s\n", count, seedPartnersPath)
			return nil
		},
	}
	seedPartnersCmd.Flags().StringVar(&seedPartnersPath, "path", "", "Path to wego/payments checkout root")

	// entities load-csv
	var loadCSVFile string
	loadCSVCmd := &cobra.Command{
		Use:   "load-csv",
		Short: "Load entities from a CSV file (kind,display_name,aliases)",
		Long: `Reads a CSV with columns kind,display_name,aliases (pipe-separated aliases).
Upserts each row into graph.entities. Idempotent.

Example CSV:
  kind,display_name,aliases
  partner,TripleA,TripleA|3A|triple a
  currency,TRY,TRY|try|Turkish Lira`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if loadCSVFile == "" {
				return fmt.Errorf("--file is required")
			}

			f, err := os.Open(loadCSVFile)
			if err != nil {
				return fmt.Errorf("open csv %s: %w", loadCSVFile, err)
			}
			defer f.Close()

			ctx := context.Background()
			db, err := database.Connect(ctx, getCfg().DatabaseURL)
			if err != nil {
				return fmt.Errorf("open db: %w", err)
			}
			defer db.Close()

			log := zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr}).With().Timestamp().Logger()
			count, err := entities.LoadFromCSV(ctx, db, f, log)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "loaded %d entities from %s\n", count, loadCSVFile)
			return nil
		},
	}
	loadCSVCmd.Flags().StringVar(&loadCSVFile, "file", "", "Path to CSV file")

	// entities list
	var listKind string
	listCmd := &cobra.Command{
		Use:   "list",
		Short: "List entities in graph.entities",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			db, err := database.Connect(ctx, getCfg().DatabaseURL)
			if err != nil {
				return fmt.Errorf("open db: %w", err)
			}
			defer db.Close()

			var query string
			var queryArgs []any
			if listKind != "" {
				query = `SELECT id, kind, display_name, source FROM graph.entities WHERE kind = $1 ORDER BY id`
				queryArgs = []any{listKind}
			} else {
				query = `SELECT id, kind, display_name, source FROM graph.entities ORDER BY kind, id`
			}

			pgRows, err := db.Query(ctx, query, queryArgs...)
			if err != nil {
				return fmt.Errorf("list entities: %w", err)
			}
			defer pgRows.Close()

			count := 0
			for pgRows.Next() {
				var id, kind, displayName, source string
				if err := pgRows.Scan(&id, &kind, &displayName, &source); err != nil {
					return fmt.Errorf("scan: %w", err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%-40s %-12s %-30s %s\n", id, kind, displayName, source)
				count++
			}
			if err := pgRows.Err(); err != nil {
				return fmt.Errorf("list entities rows: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "\n%d entities\n", count)
			return nil
		},
	}
	listCmd.Flags().StringVar(&listKind, "kind", "", "Filter by kind (partner, feature, status, currency, …)")

	// entities import-bamboohr
	var bambooHRCSVPath, bambooHRJSONPath string
	var bambooHRRetire bool
	importBambooHRCmd := &cobra.Command{
		Use:   "import-bamboohr",
		Short: "Enqueue an import_bamboohr job from the org-chart page graph (--json) or a CSV (--csv)",
		Long: `Enqueues an import_bamboohr job in graph.jobs from either source.

--json (preferred) takes the org-chart page graph: an array of
{eeid, name, job_title, department, reports_to[, email]}. Extract it from a logged-in
session with GET /employees/orgchart.php?id=<any-eeid>, whose HTML embeds the whole
tree under the "OrgChart": key — 100% job-title coverage.

--csv takes the Visio export (EEID, Full Name, Reports To [, Work Email, Department]).
That export withholds Job Title, Department and Email for everyone except the employee
who downloaded it, so it cannot populate titles.

Re-running is safe: blank incoming cells never overwrite a stored value.
--retire-missing marks anyone absent from the import inactive (never deletes them, since
their authored nodes still reference the row); it is ignored for imports under 100 rows.

Examples:
  agent-mem entities import-bamboohr --json ./bamboo_people.json --retire-missing
  agent-mem entities import-bamboohr --csv ~/Downloads/bamboohr_org_chart_for_visio.csv`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if (bambooHRCSVPath == "") == (bambooHRJSONPath == "") {
				return fmt.Errorf("provide exactly one of --json or --csv")
			}

			src := bambooHRJSONPath
			if src == "" {
				src = bambooHRCSVPath
			}
			data, err := os.ReadFile(src)
			if err != nil {
				return fmt.Errorf("read %s: %w", src, err)
			}

			payload := map[string]any{"retire_missing": bambooHRRetire}
			if bambooHRJSONPath != "" {
				if !json.Valid(data) {
					return fmt.Errorf("%s is not valid JSON", bambooHRJSONPath)
				}
				payload["people_json"] = json.RawMessage(data)
			} else {
				// Encode as base64 so the job payload is JSON-safe.
				payload["csv_bytes"] = base64.StdEncoding.EncodeToString(data)
			}

			ctx := context.Background()
			pool, err := database.Connect(ctx, getCfg().DatabaseURL)
			if err != nil {
				return fmt.Errorf("open db: %w", err)
			}
			defer pool.Close()

			cfg := getCfg()
			jobID, err := jobs.Enqueue(ctx, pool, "import_bamboohr", payload, jobs.EnqueueOptions{
				Priority:  5,
				MachineID: cfg.MachineID,
			})
			if err != nil {
				return fmt.Errorf("enqueue import_bamboohr job: %w", err)
			}

			fmt.Fprintf(cmd.OutOrStdout(), "enqueued import_bamboohr job id=%d (%s, %d bytes, retire_missing=%v)\n",
				jobID, src, len(data), bambooHRRetire)
			return nil
		},
	}
	importBambooHRCmd.Flags().StringVar(&bambooHRCSVPath, "csv", "", "Path to BambooHR Visio org-chart CSV")
	importBambooHRCmd.Flags().StringVar(&bambooHRJSONPath, "json", "", "Path to org-chart page-graph JSON (carries job titles)")
	importBambooHRCmd.Flags().BoolVar(&bambooHRRetire, "retire-missing", false, "Mark people absent from the import inactive")

	// entities refresh-slack-users
	refreshSlackUsersCmd := &cobra.Command{
		Use:   "refresh-slack-users",
		Short: "Enqueue a refresh_slack_users job (pull Slack names + emails)",
		Long: `Enqueues a refresh_slack_users job. The worker calls Slack users.list and
updates graph.slack_users and graph.people (display_name when blank, and email
when the bot has the users:read.email scope). Email is the key that merges Slack
identities with BambooHR/Jira.

Run this after people join/leave, or after granting the users:read.email scope.
Employees change rarely, so this is on-demand rather than scheduled.

Example:
  agent-mem entities refresh-slack-users`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			cfg := getCfg()
			pool, err := database.Connect(ctx, cfg.DatabaseURL)
			if err != nil {
				return fmt.Errorf("open db: %w", err)
			}
			defer pool.Close()
			jobID, err := jobs.Enqueue(ctx, pool, "refresh_slack_users", map[string]any{},
				jobs.EnqueueOptions{Priority: 5, MachineID: cfg.MachineID})
			if err != nil {
				return fmt.Errorf("enqueue refresh_slack_users job: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "enqueued refresh_slack_users job id=%d\n", jobID)
			return nil
		},
	}

	// entities refresh-slack-bots
	refreshSlackBotsCmd := &cobra.Command{
		Use:   "refresh-slack-bots",
		Short: "Enqueue a refresh_slack_bots job (resolve bot_id authors to names)",
		Long: `Enqueues a refresh_slack_bots job. The worker calls Slack bots.info for each
graph.people row whose display_name is still a raw bot_id (B…) and fills in the
real bot name (e.g. "GitHub", "PagerDuty"). Bot ids never appear in users.list,
so refresh_slack_users can't reach them.

Example:
  agent-mem entities refresh-slack-bots`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			cfg := getCfg()
			pool, err := database.Connect(ctx, cfg.DatabaseURL)
			if err != nil {
				return fmt.Errorf("open db: %w", err)
			}
			defer pool.Close()
			jobID, err := jobs.Enqueue(ctx, pool, "refresh_slack_bots", map[string]any{},
				jobs.EnqueueOptions{Priority: 5, MachineID: cfg.MachineID})
			if err != nil {
				return fmt.Errorf("enqueue refresh_slack_bots job: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "enqueued refresh_slack_bots job id=%d\n", jobID)
			return nil
		},
	}

	entitiesCmd.AddCommand(seedPartnersCmd, loadCSVCmd, listCmd, importBambooHRCmd, refreshSlackUsersCmd, refreshSlackBotsCmd)
	return entitiesCmd
}
