package handlers

import (
	"strings"
	"testing"
)

func TestTopicRulesLoadAndDigest(t *testing.T) {
	if len(loadedTopicRules.Tags) < 4 {
		t.Fatalf("expected at least 4 tags, got %d", len(loadedTopicRules.Tags))
	}
	digest := topicRulesPromptDigest()
	for _, want := range []string{"bug_incident", "feature_business", "TIE-BREAKERS", "PAYMENTS PARTNERS"} {
		if !strings.Contains(digest, want) {
			t.Fatalf("prompt digest missing %q:\n%s", want, digest)
		}
	}
	for _, tag := range loadedTopicRules.Tags {
		if tag.SameWhen == "" || tag.DifferentWhen == "" {
			t.Fatalf("tag %s missing same/different criteria", tag.Tag)
		}
	}
}
