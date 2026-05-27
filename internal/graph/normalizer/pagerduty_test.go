package normalizer

import (
	"context"
	"strings"
	"testing"
)

func TestPagerDutyNormalizer(t *testing.T) {
	ctx := context.Background()
	n := NewPagerDutyNormalizer()

	if n.Source() != "pagerduty" {
		t.Fatalf("Source() = %q, want \"pagerduty\"", n.Source())
	}

	tests := []struct {
		name         string
		input        string
		wantContains []string
	}{
		{
			name:  "empty",
			input: "",
		},
		{
			name: "typical incident",
			input: `{
				"incident": {
					"incident_number": 42,
					"title": "Database connection pool exhausted",
					"description": "The primary DB is out of connections.",
					"status": "triggered",
					"urgency": "high",
					"service": {"summary": "payments-api"},
					"assignments": [{"assignee": {"summary": "Alice Smith"}}],
					"created_at": "2024-01-15T10:00:00Z",
					"html_url": "https://pagerduty.example.com/incidents/P12345"
				}
			}`,
			wantContains: []string{
				"PagerDuty Incident #42",
				"Database connection pool exhausted",
				"payments-api",
				"high",
				"triggered",
				"Alice Smith",
				"2024-01-15T10:00:00Z",
				"https://pagerduty.example.com/incidents/P12345",
				"primary DB",
			},
		},
		{
			name: "resolved incident with multiple assignees",
			input: `{
				"incident_number": 7,
				"title": "High latency",
				"status": "resolved",
				"urgency": "low",
				"service": {"summary": "search-service"},
				"assignments": [
					{"assignee": {"summary": "Bob"}},
					{"assignee": {"summary": "Carol"}}
				],
				"created_at": "2024-01-10T08:00:00Z",
				"resolved_at": "2024-01-10T09:30:00Z",
				"html_url": "https://pd.example.com/i/7"
			}`,
			wantContains: []string{
				"#7", "High latency", "resolved", "Bob", "Carol",
				"2024-01-10T09:30:00Z",
			},
		},
		{
			name:         "malformed JSON fallback",
			input:        "not json",
			wantContains: []string{"not json"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, err := n.Normalize(ctx, []byte(tc.input), nil)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			for _, want := range tc.wantContains {
				if !strings.Contains(res.Text, want) {
					t.Errorf("Text missing %q\n  got: %q", want, res.Text)
				}
			}
		})
	}
}
