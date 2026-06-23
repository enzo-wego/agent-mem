package handlers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/agent-mem/agent-mem/internal/graph/jobs"
	"github.com/rs/zerolog"
)

// TestImportBambooHR_CSVBytes_EnqueuesViaBase64 verifies the end-to-end path
// that the CLI uses: base64-encode the CSV bytes, put them in the job payload,
// and ensure the handler parses and processes the rows correctly.
func TestImportBambooHR_CSVBytes_ParsesAndUpserts(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)

	ctx := context.Background()

	deps := Deps{
		DB:        pool,
		Logger:    zerolog.Nop(),
		MachineID: "test-machine",
	}

	csvContent := `EEID,Full Name,Reports To
1,Jane Doe,
2,John Smith,Jane Doe
3,Alice Lee,John Smith
`
	encoded := base64.StdEncoding.EncodeToString([]byte(csvContent))
	payload, err := json.Marshal(importBambooHRPayload{CSVBytes: encoded})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	h := NewImportBambooHRHandler(deps)
	if err := h.Handler(ctx, payload); err != nil {
		t.Fatalf("handler error: %v", err)
	}

	// Verify graph.people rows were upserted.
	var count int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM graph.people WHERE eeid IN (1, 2, 3)`).Scan(&count); err != nil {
		t.Fatalf("count people: %v", err)
	}
	if count != 3 {
		t.Errorf("expected 3 people rows, got %d", count)
	}

	// Verify depth values.
	type depthRow struct {
		eeid  int
		depth int16
	}
	rows, err := pool.Query(ctx, `SELECT eeid, depth_from_root FROM graph.people WHERE eeid IN (1,2,3) ORDER BY eeid`)
	if err != nil {
		t.Fatalf("query depths: %v", err)
	}
	defer rows.Close()

	var depths []depthRow
	for rows.Next() {
		var r depthRow
		if err := rows.Scan(&r.eeid, &r.depth); err != nil {
			t.Fatalf("scan depth: %v", err)
		}
		depths = append(depths, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows err: %v", err)
	}

	expected := map[int]int16{1: 0, 2: 1, 3: 2}
	for _, d := range depths {
		if want, ok := expected[d.eeid]; ok {
			if d.depth != want {
				t.Errorf("eeid %d depth = %d, want %d", d.eeid, d.depth, want)
			}
		}
	}
}

// TestImportBambooHR_EnqueueJob verifies that the CLI-level enqueue path
// creates a graph.jobs row of type "import_bamboohr" with the base64 CSV bytes.
func TestImportBambooHR_EnqueueJob(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)

	ctx := context.Background()

	csvContent := `EEID,Full Name,Reports To
10,Root Person,
`
	encoded := base64.StdEncoding.EncodeToString([]byte(csvContent))

	jobID, err := jobs.Enqueue(ctx, pool, "import_bamboohr", map[string]string{
		"csv_bytes": encoded,
	}, jobs.EnqueueOptions{
		Priority:  5,
		MachineID: "test-machine",
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if jobID <= 0 {
		t.Errorf("expected valid job id, got %d", jobID)
	}

	// Verify the job row exists.
	var jobType string
	var payloadBytes []byte
	if err := pool.QueryRow(ctx,
		`SELECT type, payload FROM graph.jobs WHERE id = $1`, jobID,
	).Scan(&jobType, &payloadBytes); err != nil {
		t.Fatalf("select job: %v", err)
	}
	if jobType != "import_bamboohr" {
		t.Errorf("job type = %q, want import_bamboohr", jobType)
	}

	// Verify the payload contains csv_bytes.
	var p importBambooHRPayload
	if err := json.Unmarshal(payloadBytes, &p); err != nil {
		t.Fatalf("unmarshal job payload: %v", err)
	}
	if p.CSVBytes == "" {
		t.Error("expected csv_bytes in job payload")
	}
}
