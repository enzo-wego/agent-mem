package handlers

import "github.com/agent-mem/agent-mem/internal/gemini"

const (
	graphEmbeddingDims     = 3072
	graphEmbeddingTaskType = "SEMANTIC_SIMILARITY"
)

func graphEmbeddingOptions() gemini.EmbedOptions {
	return gemini.EmbedOptions{
		OutputDimensionality: graphEmbeddingDims,
		TaskType:             graphEmbeddingTaskType,
	}
}
