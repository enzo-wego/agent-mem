package gemini

import "testing"

func TestParseObservationPreambleWrappedJSON(t *testing.T) {
	response := "Here is the observation:\n```json\n" +
		`{"type":"bugfix","title":"Tolerate JSON preambles"}` + "\n```"

	got, err := ParseObservation(response)
	if err != nil {
		t.Fatalf("ParseObservation() error = %v", err)
	}
	if got.Type != "bugfix" || got.Title != "Tolerate JSON preambles" {
		t.Errorf("ParseObservation() = %+v", got)
	}
}

func TestParseSummaryPreambleWrappedJSON(t *testing.T) {
	response := "Here is the summary:\n```json\n" +
		`{"request":"parse JSON","completed":"implemented"}` + "\n```"

	got, err := ParseSummary(response)
	if err != nil {
		t.Fatalf("ParseSummary() error = %v", err)
	}
	if got.Request != "parse JSON" || got.Completed != "implemented" {
		t.Errorf("ParseSummary() = %+v", got)
	}
}
