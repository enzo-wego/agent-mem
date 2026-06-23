package handlers

import (
	"context"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

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
			rows = append(rows, bambooRow{EEID: eeid, FullName: name, ReportsTo: reportsTo})
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

		return nil
	}
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
