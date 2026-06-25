package handlers

import (
	"context"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/agent-mem/agent-mem/internal/graph/jobs"
)

// importBambooHRPayload is the JSON payload for the import_bamboohr job type.
// Either csv_path or csv_bytes (base64) must be provided.
type importBambooHRPayload struct {
	CSVPath  string `json:"csv_path"`
	CSVBytes string `json:"csv_bytes"` // base64-encoded
}

// bambooRow holds one parsed row from the BambooHR CSV.
type bambooRow struct {
	EEID       string
	FullName   string
	Email      string
	ReportsTo  string
	DepthFromRoot int
}

// NewImportBambooHRHandler returns a HandlerInfo for the "import_bamboohr" job type.
func NewImportBambooHRHandler(deps Deps) jobs.Entry {
	return jobs.Entry{
		Handler:  importBambooHRHandler(deps),
		Systems:  []string{},
		PoolSize: 1,
		Lease:  600 * time.Second,
	}
}

func importBambooHRHandler(deps Deps) jobs.Handler {
	return func(ctx context.Context, payload []byte) error {
		var p importBambooHRPayload
		if err := json.Unmarshal(payload, &p); err != nil {
			return fmt.Errorf("%w: import_bamboohr unmarshal: %v", jobs.ErrFatal, err)
		}

		// Step 1: read CSV data.
		var csvData []byte
		switch {
		case p.CSVBytes != "":
			decoded, err := base64.StdEncoding.DecodeString(p.CSVBytes)
			if err != nil {
				return fmt.Errorf("%w: import_bamboohr: decode csv_bytes: %v", jobs.ErrFatal, err)
			}
			csvData = decoded
		case p.CSVPath != "":
			data, err := os.ReadFile(p.CSVPath)
			if err != nil {
				return fmt.Errorf("%w: import_bamboohr: read csv_path %q: %v", jobs.ErrFatal, p.CSVPath, err)
			}
			csvData = data
		default:
			return fmt.Errorf("%w: import_bamboohr: must provide csv_path or csv_bytes", jobs.ErrFatal)
		}

		// Parse CSV.
		reader := csv.NewReader(strings.NewReader(string(csvData)))
		reader.TrimLeadingSpace = true
		reader.LazyQuotes = true

		header, err := reader.Read()
		if err != nil {
			return fmt.Errorf("%w: import_bamboohr: read header: %v", jobs.ErrFatal, err)
		}

		// Find column indices (case-insensitive).
		colIdx := make(map[string]int)
		for i, h := range header {
			colIdx[strings.ToLower(strings.TrimSpace(h))] = i
		}
		eeidCol, hasEEID := colIdx["eeid"]
		nameCol, hasName := colIdx["full name"]
		reportsCol, hasReports := colIdx["reports to"]
		if !hasEEID || !hasName || !hasReports {
			return fmt.Errorf("%w: import_bamboohr: CSV must have columns EEID, Full Name, Reports To", jobs.ErrFatal)
		}
		// Email is optional but is the key that merges BambooHR identities with
		// Slack/Jira/etc., so org seniority attaches to those messages.
		emailCol, hasEmail := colIdx["work email"]
		if !hasEmail {
			emailCol, hasEmail = colIdx["email"]
		}

		// Parse rows.
		var rows []bambooRow
		for {
			rec, err := reader.Read()
			if err != nil {
				break // io.EOF or parse error — stop
			}
			if len(rec) <= eeidCol || len(rec) <= nameCol || len(rec) <= reportsCol {
				continue
			}
			eeid := strings.TrimSpace(rec[eeidCol])
			name := strings.TrimSpace(rec[nameCol])
			reportsTo := strings.TrimSpace(rec[reportsCol])
			if eeid == "" || name == "" {
				continue
			}
			email := ""
			if hasEmail && len(rec) > emailCol {
				email = strings.ToLower(strings.TrimSpace(rec[emailCol]))
			}
			rows = append(rows, bambooRow{EEID: eeid, FullName: name, Email: email, ReportsTo: reportsTo})
		}

		if len(rows) == 0 {
			return nil
		}

		// Step 2: first pass — upsert graph.people rows by eeid.
		for _, row := range rows {
			eeidInt, err := parseEEID(row.EEID)
			if err != nil {
				deps.Logger.Warn().Str("eeid", row.EEID).Msg("import_bamboohr: skip non-numeric EEID")
				continue
			}
			// Upsert by eeid WITHOUT email here — email is set in the reconcile step
			// below, which first merges any pre-existing Slack/etc. person that already
			// owns the email (email is UNIQUE, so a naive set would collide).
			_, execErr := deps.DB.Exec(ctx, `
				INSERT INTO graph.people (eeid, display_name, reports_to, machine_id)
				VALUES ($1, $2, $3, $4)
				ON CONFLICT (eeid) DO UPDATE SET
					display_name = EXCLUDED.display_name,
					reports_to   = EXCLUDED.reports_to`,
				eeidInt, row.FullName, nullableStringVal(row.ReportsTo), deps.MachineID,
			)
			if execErr != nil {
				deps.Logger.Warn().Err(execErr).Str("eeid", row.EEID).Msg("import_bamboohr: upsert person failed")
			}
		}

		// Step 3: compute depth_from_root by walking reports_to chains.
		// Build a map: fullName -> EEID for the reports_to resolution.
		nameToEEID := make(map[string]string, len(rows))
		for _, row := range rows {
			nameToEEID[strings.ToLower(row.FullName)] = row.EEID
		}

		for i, row := range rows {
			depth := computeDepth(row, rows, nameToEEID, 0, 20)
			rows[i].DepthFromRoot = depth
		}

		// Update depth_from_root for each person.
		for _, row := range rows {
			eeidInt, err := parseEEID(row.EEID)
			if err != nil {
				continue
			}
			_, execErr := deps.DB.Exec(ctx, `
				UPDATE graph.people SET depth_from_root = $2 WHERE eeid = $1`,
				eeidInt, row.DepthFromRoot,
			)
			if execErr != nil {
				deps.Logger.Warn().Err(execErr).Str("eeid", row.EEID).Msg("import_bamboohr: update depth failed")
			}
		}

		// Step 4: attach emails + merge with any pre-existing Slack/Jira/etc. person
		// that already owns the email, so org seniority flows onto their messages.
		merged := 0
		for _, row := range rows {
			eeidInt, err := parseEEID(row.EEID)
			if err != nil || row.Email == "" {
				continue
			}
			didMerge, e := reconcileBambooEmail(ctx, deps, eeidInt, row.Email)
			if e != nil {
				deps.Logger.Warn().Err(e).Str("eeid", row.EEID).Msg("import_bamboohr: reconcile email failed")
				continue
			}
			if didMerge {
				merged++
			}
		}
		// Step 5: bridge by exact full-name match for anyone without a shared email
		// (e.g. when the Slack bot lacks users:read.email).
		nameMerged := reconcileBambooNames(ctx, deps, rows)
		deps.Logger.Info().Int("rows", len(rows)).Int("merged_by_email", merged).Int("merged_by_name", nameMerged).Msg("import_bamboohr: done")

		return nil
	}
}

// reconcileBambooEmail attaches email to the eeid person and, if another person
// (Slack/Jira/…) already owns that email, merges it into the eeid person so a single
// row carries both org info (eeid/depth) and the source identifiers. Returns whether
// a merge happened.
func reconcileBambooEmail(ctx context.Context, deps Deps, eeid int, email string) (bool, error) {
	var canonicalID int64
	if err := deps.DB.QueryRow(ctx, `SELECT id FROM graph.people WHERE eeid=$1`, eeid).Scan(&canonicalID); err != nil {
		return false, err
	}
	var otherID int64
	err := deps.DB.QueryRow(ctx,
		`SELECT id FROM graph.people WHERE email=$1 AND merged_into IS NULL`, email).Scan(&otherID)
	if errors.Is(err, pgx.ErrNoRows) {
		// Nobody owns the email yet — safe to set it on the eeid person.
		_, e := deps.DB.Exec(ctx, `UPDATE graph.people SET email=$2 WHERE id=$1 AND email IS NULL`, canonicalID, email)
		return false, e
	}
	if err != nil {
		return false, err
	}
	if otherID == canonicalID {
		return false, nil // already attached
	}
	return true, mergePersonInto(ctx, deps, canonicalID, otherID, email)
}

// reconcileBambooNames bridges identities by exact full-name match when no shared
// email exists: a unique BambooHR Full Name that maps to exactly one un-merged
// Slack person (by real_name) merges that person into the eeid person. Conservative
// — skips any name that is ambiguous on either side. Returns merge count.
func reconcileBambooNames(ctx context.Context, deps Deps, rows []bambooRow) int {
	// Only consider full names that are unique within the BambooHR set.
	nameCount := map[string]int{}
	nameEEID := map[string]int{}
	for _, r := range rows {
		ln := strings.ToLower(strings.TrimSpace(r.FullName))
		if ln == "" {
			continue
		}
		nameCount[ln]++
		if eeid, err := parseEEID(r.EEID); err == nil {
			nameEEID[ln] = eeid
		}
	}
	merged := 0
	for ln, cnt := range nameCount {
		if cnt != 1 {
			continue // ambiguous BambooHR name
		}
		eeid, ok := nameEEID[ln]
		if !ok {
			continue
		}
		var canonicalID int64
		if err := deps.DB.QueryRow(ctx, `SELECT id FROM graph.people WHERE eeid=$1`, eeid).Scan(&canonicalID); err != nil {
			continue
		}
		// Exactly one un-merged, eeid-less Slack person with that real_name.
		var otherID int64
		var matches int
		if err := deps.DB.QueryRow(ctx, `
			SELECT count(*), COALESCE(min(p.id),0)
			FROM graph.slack_users su
			JOIN graph.people p ON p.slack_user_id = su.slack_user_id
			WHERE lower(su.real_name) = $1 AND p.merged_into IS NULL AND p.eeid IS NULL`,
			ln).Scan(&matches, &otherID); err != nil {
			continue
		}
		if matches != 1 || otherID == 0 || otherID == canonicalID {
			continue
		}
		if err := mergePersonInto(ctx, deps, canonicalID, otherID, ""); err != nil {
			deps.Logger.Warn().Err(err).Str("name", ln).Msg("import_bamboohr: name merge failed")
			continue
		}
		merged++
	}
	return merged
}

// mergePersonInto folds person otherID into canonicalID: frees the loser's UNIQUE
// columns, moves its source identifiers + (optional) email onto the canonical row,
// repoints author_person_id / identity_map, and marks the loser merged_into.
func mergePersonInto(ctx context.Context, deps Deps, canonicalID, otherID int64, email string) error {
	tx, err := deps.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var slackUID, jiraAcct, ghLogin, pdUser *string
	if err := tx.QueryRow(ctx,
		`SELECT slack_user_id, jira_account_id, github_login, pagerduty_user_id FROM graph.people WHERE id=$1`,
		otherID).Scan(&slackUID, &jiraAcct, &ghLogin, &pdUser); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE graph.people SET email=NULL, slack_user_id=NULL, jira_account_id=NULL,
		 github_login=NULL, pagerduty_user_id=NULL, merged_into=$2 WHERE id=$1`,
		otherID, canonicalID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE graph.people SET email=COALESCE($2,email),
		 slack_user_id=COALESCE(slack_user_id,$3), jira_account_id=COALESCE(jira_account_id,$4),
		 github_login=COALESCE(github_login,$5), pagerduty_user_id=COALESCE(pagerduty_user_id,$6)
		 WHERE id=$1`,
		canonicalID, nullableStringVal(email), slackUID, jiraAcct, ghLogin, pdUser); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE graph.nodes SET author_person_id=$1 WHERE author_person_id=$2`, canonicalID, otherID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE graph.identity_map SET person_id=$1 WHERE person_id=$2`, canonicalID, otherID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// computeDepth recursively computes the depth of a row from the root (person with no manager).
func computeDepth(row bambooRow, rows []bambooRow, nameToEEID map[string]string, current, maxDepth int) int {
	if current >= maxDepth {
		return current // cycle guard
	}
	if row.ReportsTo == "" {
		return current // root node
	}
	// Find parent row.
	parentEEID, ok := nameToEEID[strings.ToLower(row.ReportsTo)]
	if !ok {
		return current // parent not in dataset
	}
	for _, r := range rows {
		if r.EEID == parentEEID && r.EEID != row.EEID {
			return computeDepth(r, rows, nameToEEID, current+1, maxDepth)
		}
	}
	return current
}

// parseEEID parses an EEID string. BambooHR EEIDs are typically integers.
func parseEEID(s string) (int, error) {
	var v int
	_, err := fmt.Sscanf(s, "%d", &v)
	return v, err
}

// nullableStringVal returns nil for empty strings.
func nullableStringVal(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
