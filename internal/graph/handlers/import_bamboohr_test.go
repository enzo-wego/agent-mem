package handlers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

func TestImportBambooHRHandler_BadPayload(t *testing.T) {
	deps := Deps{Logger: zerolog.Nop(), MachineID: "test"}
	h := NewImportBambooHRHandler(deps)

	err := h.Handler(context.Background(), []byte("not json"))
	if err == nil {
		t.Fatal("expected error for bad JSON")
	}
}

func TestImportBambooHRHandler_NoSource(t *testing.T) {
	deps := Deps{Logger: zerolog.Nop(), MachineID: "test"}
	h := NewImportBambooHRHandler(deps)

	payload, _ := json.Marshal(importBambooHRPayload{})
	err := h.Handler(context.Background(), payload)
	if err == nil {
		t.Fatal("expected error when neither csv_path nor csv_bytes provided")
	}
}

func TestImportBambooHRHandler_BadCSVHeader(t *testing.T) {
	deps := Deps{Logger: zerolog.Nop(), MachineID: "test"}
	h := NewImportBambooHRHandler(deps)

	csvContent := "WrongCol1,WrongCol2\n1,Alice\n"
	encoded := base64.StdEncoding.EncodeToString([]byte(csvContent))
	payload, _ := json.Marshal(importBambooHRPayload{CSVBytes: encoded})
	err := h.Handler(context.Background(), payload)
	if err == nil {
		t.Fatal("expected error for missing required CSV columns")
	}
}

func TestImportBambooHRHandler_MissingCSVPath(t *testing.T) {
	deps := Deps{Logger: zerolog.Nop(), MachineID: "test"}
	h := NewImportBambooHRHandler(deps)

	payload, _ := json.Marshal(importBambooHRPayload{CSVPath: "/nonexistent/path.csv"})
	err := h.Handler(context.Background(), payload)
	if err == nil {
		t.Fatal("expected error for non-existent CSV path")
	}
}

func TestImportBambooHRHandler_WithDB(t *testing.T) {
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set")
	}
	// Integration test placeholder.
}

func indexRows(rows []bambooRow) (map[string]bambooRow, map[string]string) {
	byEEID := make(map[string]bambooRow, len(rows))
	nameToEEID := make(map[string]string, len(rows))
	for _, r := range rows {
		byEEID[r.EEID] = r
		nameToEEID[strings.ToLower(r.FullName)] = r.EEID
	}
	return byEEID, nameToEEID
}

// TestComputeDepthByEEID uses the shape BambooHR actually exports: "Reports To" is the
// manager's EEID, not their name. The original fixture used names, so it passed while
// every real import produced depth 0 for all 448 people.
func TestComputeDepthByEEID(t *testing.T) {
	rows := []bambooRow{
		{EEID: "111", FullName: "Ross Veitch", ReportsTo: ""},
		{EEID: "295", FullName: "Chu Yeow Cheah", ReportsTo: "111"},
		{EEID: "1192", FullName: "Ryan Tan", ReportsTo: "295"},
		{EEID: "259", FullName: "Lei Zheng", ReportsTo: "1192"},
	}
	byEEID, nameToEEID := indexRows(rows)

	for i, want := range []int{0, 1, 2, 3} {
		if got := computeDepth(rows[i], byEEID, nameToEEID, 0, 20); got != want {
			t.Errorf("%s depth: got %d, want %d", rows[i].FullName, got, want)
		}
	}
}

// TestComputeDepthByName keeps the name-resolution path alive for hand-built CSVs.
func TestComputeDepthByName(t *testing.T) {
	rows := []bambooRow{
		{EEID: "1", FullName: "CEO", ReportsTo: ""},
		{EEID: "2", FullName: "VP Eng", ReportsTo: "CEO"},
		{EEID: "3", FullName: "Engineer", ReportsTo: "VP Eng"},
	}
	byEEID, nameToEEID := indexRows(rows)

	for i, want := range []int{0, 1, 2} {
		if got := computeDepth(rows[i], byEEID, nameToEEID, 0, 20); got != want {
			t.Errorf("%s depth: got %d, want %d", rows[i].FullName, got, want)
		}
	}
}

// TestComputeDepthCycle guards the recursion: a mutual reports_to must terminate.
func TestComputeDepthCycle(t *testing.T) {
	rows := []bambooRow{
		{EEID: "1", FullName: "A", ReportsTo: "2"},
		{EEID: "2", FullName: "B", ReportsTo: "1"},
	}
	byEEID, nameToEEID := indexRows(rows)

	if got := computeDepth(rows[0], byEEID, nameToEEID, 0, 20); got != 20 {
		t.Errorf("cycle depth: got %d, want 20 (maxDepth guard)", got)
	}
}
