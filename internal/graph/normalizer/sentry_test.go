package normalizer

import (
	"context"
	"strings"
	"testing"
)

func TestSentryNormalizer(t *testing.T) {
	ctx := context.Background()
	n := NewSentryNormalizer()

	if n.Source() != "sentry" {
		t.Fatalf("Source() = %q, want \"sentry\"", n.Source())
	}

	tests := []struct {
		name         string
		input        string
		wantContains []string
		wantAbsent   []string
	}{
		{
			name:  "empty",
			input: "",
		},
		{
			name: "full issue with stack trace",
			input: `{
				"shortId": "PAY-123",
				"title": "NullPointerException in checkout",
				"status": "unresolved",
				"level": "error",
				"firstSeen": "2024-01-01T00:00:00Z",
				"lastSeen": "2024-01-15T12:00:00Z",
				"count": "42",
				"userCount": 7,
				"permalink": "https://sentry.io/organizations/wego/issues/123/",
				"project": {"slug": "payments"},
				"metadata": {"value": "NullPointerException: field is null"},
				"entries": [{
					"type": "exception",
					"data": {
						"values": [{
							"stacktrace": {
								"frames": [
									{"filename": "old.go", "lineno": 1, "function": "oldFunc"},
									{"filename": "main.go", "lineno": 42, "function": "handleCheckout"}
								]
							}
						}]
					}
				}]
			}`,
			wantContains: []string{
				"Sentry Issue PAY-123",
				"NullPointerException in checkout",
				"payments",
				"unresolved",
				"error",
				"2024-01-01T00:00:00Z",
				"Count: 42",
				"Users: 7",
				"https://sentry.io",
				"NullPointerException: field is null",
				"Latest stack frame:",
				"main.go:42 in handleCheckout",
			},
		},
		{
			name: "minimal issue without stack",
			input: `{
				"shortId": "PROJ-1",
				"title": "Something broke",
				"status": "resolved",
				"level": "warning",
				"firstSeen": "2024-01-01T00:00:00Z",
				"lastSeen": "2024-01-01T00:00:00Z",
				"count": "1",
				"userCount": 0
			}`,
			wantContains: []string{"PROJ-1", "Something broke", "resolved"},
			wantAbsent:   []string{"Latest stack frame:"},
		},
		{
			name:         "malformed JSON fallback",
			input:        "not valid json",
			wantContains: []string{"not valid json"},
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
			for _, absent := range tc.wantAbsent {
				if strings.Contains(res.Text, absent) {
					t.Errorf("Text should not contain %q\n  got: %q", absent, res.Text)
				}
			}
		})
	}
}
