package llmgateway

import (
	"context"
	"encoding/json"
	"testing"
)

func TestDescribePreambleWrappedJSON(t *testing.T) {
	inner := "Here is the description:\n```json\n" +
		`{"description":"a chart","ocr":"Q1 revenue","entities":["Q1"]}` + "\n```"
	payload, err := json.Marshal(map[string]string{"backend": "claude", "text": inner})
	if err != nil {
		t.Fatal(err)
	}
	srv, _ := serve(t, 200, string(payload))

	description, ocr, entities, err := New(srv.URL, "secret", 3072).
		Describe(context.Background(), "image/png", []byte{0x89, 0x50}, "Describe it")
	if err != nil {
		t.Fatalf("Describe() error = %v", err)
	}
	if description != "a chart" || ocr != "Q1 revenue" || len(entities) != 1 || entities[0] != "Q1" {
		t.Errorf("Describe() = %q, %q, %v", description, ocr, entities)
	}
}
