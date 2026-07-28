package handlers

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/rs/zerolog"

	"github.com/agent-mem/agent-mem/internal/database"
)

// TestImportBambooHRPeopleJSON exercises the people_json path end to end: titles land,
// depth_from_root is computed from EEID chains (the bug that left all 448 people at
// depth 0), and retire_missing retires only people absent from a full-sized import.
//
// Point AGENT_MEM_TEST_DATABASE_URL at a scratch database — never the dev database, whose
// graph other tests truncate.
func TestImportBambooHRPeopleJSON(t *testing.T) {
	dsn := os.Getenv("AGENT_MEM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("AGENT_MEM_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := database.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	if _, err := pool.Exec(ctx, `DELETE FROM graph.people WHERE machine_id = 'test-people-json'`); err != nil {
		t.Fatalf("clean: %v", err)
	}
	deps := Deps{DB: pool, Logger: zerolog.Nop(), MachineID: "test-people-json"}

	people := []bambooPerson{
		{EEID: "9111", Name: "Root Boss", JobTitle: "CEO and Co-Founder", Department: "Management"},
		{EEID: "9295", Name: "Tech Chief", JobTitle: "Chief Technology Officer", Department: "Engineering", ReportsTo: "9111"},
		{EEID: "9192", Name: "Eng Director", JobTitle: "Senior Director, Engineering", Department: "Engineering", ReportsTo: "9295"},
		{EEID: "9259", Name: "Staff Eng", JobTitle: "Staff Software Engineer", Department: "Engineering", ReportsTo: "9192"},
	}
	raw, err := json.Marshal(people)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	payload, err := json.Marshal(importBambooHRPayload{PeopleJSON: raw})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	if err := importBambooHRHandler(deps)(ctx, payload); err != nil {
		t.Fatalf("import: %v", err)
	}

	for _, want := range []struct {
		eeid  int
		name  string
		title string
		depth int
	}{
		{9111, "Root Boss", "CEO and Co-Founder", 0},
		{9295, "Tech Chief", "Chief Technology Officer", 1},
		{9192, "Eng Director", "Senior Director, Engineering", 2},
		{9259, "Staff Eng", "Staff Software Engineer", 3},
	} {
		var name, title string
		var depth int
		var active bool
		if err := pool.QueryRow(ctx,
			`SELECT display_name, COALESCE(job_title,''), COALESCE(depth_from_root,-1), active
			 FROM graph.people WHERE eeid=$1`, want.eeid).Scan(&name, &title, &depth, &active); err != nil {
			t.Fatalf("eeid %d: %v", want.eeid, err)
		}
		if name != want.name || title != want.title {
			t.Errorf("eeid %d: got %q/%q, want %q/%q", want.eeid, name, title, want.name, want.title)
		}
		if depth != want.depth {
			t.Errorf("eeid %d depth: got %d, want %d", want.eeid, depth, want.depth)
		}
		if !active {
			t.Errorf("eeid %d: want active", want.eeid)
		}
	}

	// retire_missing must be ignored below retireFloor, or a truncated scrape would
	// retire the whole company.
	short, err := json.Marshal(importBambooHRPayload{
		PeopleJSON:    mustJSON(t, []bambooPerson{{EEID: "9111", Name: "Root Boss"}}),
		RetireMissing: true,
	})
	if err != nil {
		t.Fatalf("marshal short: %v", err)
	}
	if err := importBambooHRHandler(deps)(ctx, short); err != nil {
		t.Fatalf("short import: %v", err)
	}
	var stillActive bool
	if err := pool.QueryRow(ctx, `SELECT active FROM graph.people WHERE eeid=9259`).Scan(&stillActive); err != nil {
		t.Fatalf("post-retire read: %v", err)
	}
	if !stillActive {
		t.Error("eeid 9259 was retired by a 1-row import; retireFloor did not hold")
	}

	if _, err := pool.Exec(ctx, `DELETE FROM graph.people WHERE eeid IN (9111,9295,9192,9259)`); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}
