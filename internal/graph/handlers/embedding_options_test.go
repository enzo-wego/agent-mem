package handlers

import "testing"

func TestGraphEmbeddingOptions(t *testing.T) {
	opts := graphEmbeddingOptions()
	if opts.OutputDimensionality != 3072 {
		t.Fatalf("graph embedding dims = %d, want 3072", opts.OutputDimensionality)
	}
	if opts.TaskType != "SEMANTIC_SIMILARITY" {
		t.Fatalf("graph embedding task type = %q, want SEMANTIC_SIMILARITY", opts.TaskType)
	}
}
