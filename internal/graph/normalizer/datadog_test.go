package normalizer

import (
	"context"
	"strings"
	"testing"
)

func TestDatadogNormalizer(t *testing.T) {
	ctx := context.Background()
	n := NewDatadogNormalizer()

	if n.Source() != "datadog" {
		t.Fatalf("Source() = %q, want \"datadog\"", n.Source())
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
			name: "metric monitor",
			input: `{
				"id": 12345,
				"name": "High CPU usage on prod",
				"type": "metric alert",
				"overall_state": "Alert",
				"creator": {"name": "Enzo"},
				"query": "avg:system.cpu.user{env:prod} > 90",
				"message": "CPU too high @pagerduty-payments",
				"tags": ["env:prod", "team:payments"]
			}`,
			wantContains: []string{
				"Datadog Monitor #12345",
				"High CPU usage on prod",
				"metric alert",
				"Alert",
				"Enzo",
				"avg:system.cpu.user",
				"CPU too high",
				"env:prod",
				"team:payments",
			},
		},
		{
			name: "log monitor",
			input: `{
				"id": 99,
				"name": "Error spike in logs",
				"type": "log alert",
				"overall_state": "OK",
				"creator": {"name": "CI Bot"},
				"query": "logs(\"status:error\").index(\"*\").rollup(\"count\").last(\"5m\") > 100",
				"message": "Too many errors",
				"tags": []
			}`,
			wantContains: []string{"Datadog Monitor #99", "log alert", "OK", "CI Bot"},
		},
		{
			name: "missing fields handled gracefully",
			input: `{
				"id": 1,
				"name": "Minimal"
			}`,
			wantContains: []string{"Datadog Monitor #1", "Minimal"},
		},
		{
			name:         "malformed JSON fallback",
			input:        "{bad json",
			wantContains: []string{"{bad json"},
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
