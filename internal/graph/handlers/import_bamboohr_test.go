package handlers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
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

func TestComputeDepth(t *testing.T) {
	rows := []bambooRow{
		{EEID: "1", FullName: "CEO", ReportsTo: ""},
		{EEID: "2", FullName: "VP Eng", ReportsTo: "CEO"},
		{EEID: "3", FullName: "Engineer", ReportsTo: "VP Eng"},
	}
	nameToEEID := map[string]string{
		"ceo":     "1",
		"vp eng":  "2",
		"engineer": "3",
	}

	depth0 := computeDepth(rows[0], rows, nameToEEID, 0, 20)
	if depth0 != 0 {
		t.Errorf("CEO depth: got %d, want 0", depth0)
	}
	depth1 := computeDepth(rows[1], rows, nameToEEID, 0, 20)
	if depth1 != 1 {
		t.Errorf("VP Eng depth: got %d, want 1", depth1)
	}
	depth2 := computeDepth(rows[2], rows, nameToEEID, 0, 20)
	if depth2 != 2 {
		t.Errorf("Engineer depth: got %d, want 2", depth2)
	}
}
