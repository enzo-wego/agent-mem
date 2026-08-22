package handlers

import (
	"testing"

	"github.com/agent-mem/agent-mem/internal/graph/jobs"
)

func TestRegisterAllMarksLLMJobHandlers(t *testing.T) {
	reg := jobs.NewRegistry()
	RegisterAll(reg, Deps{})

	expected := map[string]bool{
		"describe_attachment":   true,
		"index_artifact":        true,
		"summarize_thread":      true,
		"link_topics":           true,
		"derive_feature_entity": true,
		"detect_hot_topics":     true,
		"refresh_topic_scope":   true,
	}

	for _, jobType := range reg.Types() {
		entry, ok := reg.Get(jobType)
		if !ok {
			t.Fatalf("registered job %q not found", jobType)
		}
		if entry.UsesLLM != expected[jobType] {
			t.Errorf("job %q UsesLLM = %v, want %v", jobType, entry.UsesLLM, expected[jobType])
		}
	}
	for jobType := range expected {
		if _, ok := reg.Get(jobType); !ok {
			t.Errorf("expected LLM job %q is not registered", jobType)
		}
	}
}
