package handlers

import "github.com/agent-mem/agent-mem/internal/gemini"

const graphEmbeddingDims = 3072

// graphEmbeddingOptions selects 3072-dim embeddings for graph indexing. No
// task_type: OpenRouter's embeddings API does not accept it, so we must not
// depend on it (docs and queries both go without it, keeping the space consistent).
func graphEmbeddingOptions() gemini.EmbedOptions {
	return gemini.EmbedOptions{
		OutputDimensionality: graphEmbeddingDims,
	}
}
