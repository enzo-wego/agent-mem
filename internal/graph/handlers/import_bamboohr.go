package handlers

import (
	"bytes"
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
// Exactly one source must be provided: people_json/people_path (preferred — the
// org-chart page graph, which carries job titles) or csv_path/csv_bytes (the Visio
// export, which carries only EEID/name/manager).
type importBambooHRPayload struct {
	CSVPath  string `json:"csv_path"`
	CSVBytes string `json:"csv_bytes"` // base64-encoded
	// PeopleJSON is the array extracted from orgchart.php?id=<eeid> — see bambooPerson.
	PeopleJSON json.RawMessage `json:"people_json"`
	PeoplePath string          `json:"people_path"`
	// RetireMissing sets active=false for eeids absent from this import (leavers).
	// Off by default so a partial file can never retire the company.
	RetireMissing bool `json:"retire_missing"`
}

// bambooPerson is one entry of the people_json array, matching the field names the
// org-chart page graph exposes per node.
type bambooPerson struct {
	EEID       string `json:"eeid"`
	Name       string `json:"name"`
	JobTitle   string `json:"job_title"`
	Department string `json:"department"`
	Email      string `json:"email"`
	ReportsTo  string `json:"reports_to"`
}

// bambooRow holds one parsed row from the BambooHR CSV.
type bambooRow struct {
	EEID          string
	FullName      string
	Email         string
	ReportsTo     string
	Department    string
	JobTitle      string
	DepthFromRoot int
}

// retireFloor is the minimum row count before RetireMissing is honoured. A truncated
// or failed scrape must not be able to mark the whole company inactive.
const retireFloor = 100

// hasPeopleJSON reports whether people_json actually carries a list. An omitted field
// marshals to the literal `null`, which is non-empty as bytes — treating that as "present"
// silently turned a source-less import into a no-op success instead of an error.
func hasPeopleJSON(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) > 0 && !bytes.Equal(t, []byte("null"))
}

// NewImportBambooHRHandler returns a HandlerInfo for the "import_bamboohr" job type.
func NewImportBambooHRHandler(deps Deps) jobs.Entry {
	return jobs.Entry{
		Handler:  importBambooHRHandler(deps),
		Systems:  []string{},
		PoolSize: 1,
		Lease:    600 * time.Second,
	}
}

func importBambooHRHandler(deps Deps) jobs.Handler {
	return func(ctx context.Context, payload []byte) error {
		var p importBambooHRPayload
		if err := json.Unmarshal(payload, &p); err != nil {
			return fmt.Errorf("%w: import_bamboohr unmarshal: %v", jobs.ErrFatal, err)
		}

		// Step 1: read the people list. The page-graph JSON is preferred: it carries job
		// titles and departments for everyone, which the CSV export withholds for all but
		// the requesting employee.
		var rows []bambooRow
		switch {
		case hasPeopleJSON(p.PeopleJSON) || p.PeoplePath != "":
			data := []byte(p.PeopleJSON)
			if p.PeoplePath != "" {
				b, err := os.ReadFile(p.PeoplePath)
				if err != nil {
					return fmt.Errorf("%w: import_bamboohr: read people_path %q: %v", jobs.ErrFatal, p.PeoplePath, err)
				}
				data = b
			}
			var people []bambooPerson
			if err := json.Unmarshal(data, &people); err != nil {
				return fmt.Errorf("%w: import_bamboohr: parse people_json: %v", jobs.ErrFatal, err)
			}
			for _, x := range people {
				eeid := strings.TrimSpace(x.EEID)
				if eeid == "" {
					continue
				}
				rows = append(rows, bambooRow{
					EEID: eeid, FullName: strings.TrimSpace(x.Name),
					Email:      strings.ToLower(strings.TrimSpace(x.Email)),
					ReportsTo:  strings.TrimSpace(x.ReportsTo),
					Department: strings.TrimSpace(x.Department),
					JobTitle:   strings.TrimSpace(x.JobTitle),
				})
			}
		default:
			var err error
			if rows, err = parseBambooCSV(p); err != nil {
				return err
			}
		}

		if len(rows) == 0 {
			return nil
		}

		return applyBambooRows(ctx, deps, rows, p.RetireMissing)
	}
}

// parseBambooCSV reads the Visio-style export (EEID, Full Name, Reports To [, Work
// Email, Department]).
func parseBambooCSV(p importBambooHRPayload) ([]bambooRow, error) {
	{
		var csvData []byte
		switch {
		case p.CSVBytes != "":
			decoded, err := base64.StdEncoding.DecodeString(p.CSVBytes)
			if err != nil {
				return nil, fmt.Errorf("%w: import_bamboohr: decode csv_bytes: %v", jobs.ErrFatal, err)
			}
			csvData = decoded
		case p.CSVPath != "":
			data, err := os.ReadFile(p.CSVPath)
			if err != nil {
				return nil, fmt.Errorf("%w: import_bamboohr: read csv_path %q: %v", jobs.ErrFatal, p.CSVPath, err)
			}
			csvData = data
		default:
			return nil, fmt.Errorf("%w: import_bamboohr: must provide people_json, people_path, csv_path or csv_bytes", jobs.ErrFatal)
		}

		// Parse CSV.
		reader := csv.NewReader(strings.NewReader(string(csvData)))
		reader.TrimLeadingSpace = true
		reader.LazyQuotes = true

		header, err := reader.Read()
		if err != nil {
			return nil, fmt.Errorf("%w: import_bamboohr: read header: %v", jobs.ErrFatal, err)
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
			return nil, fmt.Errorf("%w: import_bamboohr: CSV must have columns EEID, Full Name, Reports To", jobs.ErrFatal)
		}
		// Email is optional but is the key that merges BambooHR identities with
		// Slack/Jira/etc., so org seniority attaches to those messages.
		emailCol, hasEmail := colIdx["work email"]
		if !hasEmail {
			emailCol, hasEmail = colIdx["email"]
		}
		// Department is optional; surfaced as the person's team label in summaries/alerts.
		deptCol, hasDept := colIdx["department"]

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
			dept := ""
			if hasDept && len(rec) > deptCol {
				dept = strings.TrimSpace(rec[deptCol])
			}
			rows = append(rows, bambooRow{EEID: eeid, FullName: name, Email: email, ReportsTo: reportsTo, Department: dept})
		}
		return rows, nil
	}
}

// applyBambooRows upserts people, recomputes depth, reconciles identities, and (when
// asked) retires anyone absent from the import.
func applyBambooRows(ctx context.Context, deps Deps, rows []bambooRow, retireMissing bool) error {
	{
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
				INSERT INTO graph.people (eeid, display_name, reports_to, department, job_title, active, machine_id)
				VALUES ($1, $2, $3, $4, $5, true, $6)
				ON CONFLICT (eeid) DO UPDATE SET
					-- Never wipe a known name with a blank: BambooHR's FullName is
					-- empty for some senior/cross-org people whose names we resolved
					-- from Slack. Only overwrite when the incoming name is non-blank.
					display_name = COALESCE(NULLIF(EXCLUDED.display_name, ''), graph.people.display_name),
					reports_to   = EXCLUDED.reports_to,
					-- Same blank-guard for department: keep a prior value if this row's is empty.
					department   = COALESCE(NULLIF(EXCLUDED.department, ''), graph.people.department),
					job_title    = COALESCE(NULLIF(EXCLUDED.job_title, ''), graph.people.job_title),
					-- Present in the import means currently employed: un-retire on rehire.
					active       = true`,
				eeidInt, row.FullName, nullableStringVal(row.ReportsTo), nullableStringVal(row.Department),
				nullableStringVal(row.JobTitle), deps.MachineID,
			)
			if execErr != nil {
				deps.Logger.Warn().Err(execErr).Str("eeid", row.EEID).Msg("import_bamboohr: upsert person failed")
			}
		}

		// Step 3: compute depth_from_root by walking reports_to chains.
		// Build a map: fullName -> EEID for the reports_to resolution.
		nameToEEID := make(map[string]string, len(rows))
		byEEID := make(map[string]bambooRow, len(rows))
		for _, row := range rows {
			nameToEEID[strings.ToLower(row.FullName)] = row.EEID
			byEEID[row.EEID] = row
		}

		for i, row := range rows {
			depth := computeDepth(row, byEEID, nameToEEID, 0, 20)
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

		// Step 6: retire people who are no longer in BambooHR. Never delete them — their
		// authored graph.nodes still reference the row — and never act on a short import,
		// which would mean the scrape failed rather than that the company left.
		retired := 0
		if retireMissing {
			if len(rows) < retireFloor {
				deps.Logger.Warn().Int("rows", len(rows)).Int("floor", retireFloor).
					Msg("import_bamboohr: retire_missing ignored, import too small")
			} else {
				eeids := make([]int, 0, len(rows))
				for _, row := range rows {
					if v, err := parseEEID(row.EEID); err == nil {
						eeids = append(eeids, v)
					}
				}
				tag, err := deps.DB.Exec(ctx, `
					UPDATE graph.people SET active = false
					WHERE eeid IS NOT NULL AND merged_into IS NULL
					  AND active AND NOT (eeid = ANY($1))`, eeids)
				if err != nil {
					deps.Logger.Warn().Err(err).Msg("import_bamboohr: retire missing failed")
				} else {
					retired = int(tag.RowsAffected())
				}
			}
		}

		deps.Logger.Info().Int("rows", len(rows)).Int("merged_by_email", merged).
			Int("merged_by_name", nameMerged).Int("retired", retired).Msg("import_bamboohr: done")

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
		if !plausibleFullName(ln) {
			continue // "A" is not a name to match strangers on
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

	// An eeid identifies one employee, so two rows that both carry one are two different
	// people and must never be folded together. Skipping this check once collapsed 57
	// distinct employees into chains (Pal -> Parhi -> Patel) after a name-match merge
	// accepted the single letter "A" as their full name.
	var loserEEID, winnerEEID *int
	if err := tx.QueryRow(ctx,
		`SELECT (SELECT eeid FROM graph.people WHERE id=$1), (SELECT eeid FROM graph.people WHERE id=$2)`,
		otherID, canonicalID).Scan(&loserEEID, &winnerEEID); err != nil {
		return err
	}
	if loserEEID != nil && winnerEEID != nil {
		return fmt.Errorf("refusing to merge two employees: person %d (eeid %d) into %d (eeid %d)",
			otherID, *loserEEID, canonicalID, *winnerEEID)
	}

	var slackUID, jiraAcct, ghLogin, pdUser, loserName *string
	if err := tx.QueryRow(ctx,
		`SELECT slack_user_id, jira_account_id, github_login, pagerduty_user_id, display_name
		 FROM graph.people WHERE id=$1`,
		otherID).Scan(&slackUID, &jiraAcct, &ghLogin, &pdUser, &loserName); err != nil {
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
		 github_login=COALESCE(github_login,$5), pagerduty_user_id=COALESCE(pagerduty_user_id,$6),
		 -- Inherit the loser's name when the survivor has none: BambooHR rows carry an
		 -- eeid but often no name, while the Slack row being absorbed has one. Without
		 -- this, merges silently produced nameless people (201 of them) that no
		 -- name lookup could ever find.
		 display_name=COALESCE(NULLIF(display_name,''), NULLIF($7,''))
		 WHERE id=$1`,
		canonicalID, nullableStringVal(email), slackUID, jiraAcct, ghLogin, pdUser, loserName); err != nil {
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
//
// "Reports To" holds the manager's EEID in every BambooHR export (`1126,"Swetha A",1018`),
// so it is resolved against byEEID first. The nameToEEID lookup stays as a second attempt
// for hand-built CSVs that put a manager's name in that column; resolving only by name --
// as this did originally -- never matched an EEID, so every person came out at depth 0.
func computeDepth(row bambooRow, byEEID map[string]bambooRow, nameToEEID map[string]string, current, maxDepth int) int {
	if current >= maxDepth {
		return current // cycle guard
	}
	if row.ReportsTo == "" {
		return current // root node
	}
	parent, ok := byEEID[row.ReportsTo]
	if !ok {
		parentEEID, found := nameToEEID[strings.ToLower(row.ReportsTo)]
		if !found {
			return current // parent not in dataset
		}
		if parent, ok = byEEID[parentEEID]; !ok {
			return current
		}
	}
	if parent.EEID == row.EEID {
		return current // self-reference guard
	}
	return computeDepth(parent, byEEID, nameToEEID, current+1, maxDepth)
}

// parseEEID parses an EEID string. BambooHR EEIDs are typically integers.
func parseEEID(s string) (int, error) {
	var v int
	_, err := fmt.Sscanf(s, "%d", &v)
	return v, err
}

// plausibleFullName reports whether a value is safe to merge two identities on. Merging
// strangers together requires a real first+last name; placeholder values like "A" (56 of
// which sat in graph.people) are unique enough to pass a uniqueness test while identifying
// nobody.
func plausibleFullName(s string) bool {
	s = strings.TrimSpace(s)
	first, rest, ok := strings.Cut(s, " ")
	return ok && len(first) >= 2 && len(strings.TrimSpace(rest)) >= 2
}

// nullableStringVal returns nil for empty strings.
func nullableStringVal(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
