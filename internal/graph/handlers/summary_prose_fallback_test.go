package handlers

import (
	"context"
	"testing"
)

// TestProse verifies the salvage rule: keep non-JSON prose, reject JSON-shaped
// (malformed) or empty output.
func TestProse(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		{"The image is a screenshot shared during a Slack thread.", "The image is a screenshot shared during a Slack thread."},
		{"  padded prose  ", "padded prose"},
		{`{"overview":"truncated`, ""}, // malformed JSON — don't show raw braces
		{"", ""},
		{"   ", ""},
	} {
		if got := prose(tc.in); got != tc.want {
			t.Errorf("prose(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestGenClusterSummary_ProseFallback: when the LLM answers in prose instead of
// JSON, the prose becomes the overview (so the popup renders + caches) rather
// than being discarded and leaving the summary blank forever.
func TestGenClusterSummary_ProseFallback(t *testing.T) {
	gem := &mockGemini{}
	gem.generateResult = func() (string, error) {
		return "The image is a screenshot about Checkout payment integration.", nil
	}
	ov, hl := genClusterSummary(context.Background(), gem, "transcript")
	if ov != "The image is a screenshot about Checkout payment integration." {
		t.Errorf("overview = %q", ov)
	}
	if len(hl) != 0 {
		t.Errorf("highlights = %v, want none", hl)
	}
}

// TestGenThreadDeepSummary_ProseFallback: same salvage for thread summaries —
// prose fills the overview; topic stays empty (callers fall back to the title).
func TestGenThreadDeepSummary_ProseFallback(t *testing.T) {
	gem := &mockGemini{}
	gem.generateResult = func() (string, error) {
		return "Ross reported refunds returning none; the team is investigating.", nil
	}
	topic, ov, hl := genThreadDeepSummary(context.Background(), gem, "transcript")
	if topic != "" {
		t.Errorf("topic = %q, want empty", topic)
	}
	if ov != "Ross reported refunds returning none; the team is investigating." {
		t.Errorf("overview = %q", ov)
	}
	if len(hl) != 0 {
		t.Errorf("highlights = %v, want none", hl)
	}
}
