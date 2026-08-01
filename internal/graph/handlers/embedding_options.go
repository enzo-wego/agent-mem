package handlers

import "github.com/agent-mem/agent-mem/internal/gemini"

// GraphEmbeddingDims is the width of graph.artifact_index.embedding, halfvec(3072).
// Exported because the worker must build its LLM-gateway client at this width:
// flat memory writes vector(768), and a client handed the wrong default fails
// every insert with "expected 768 dimensions, not 3072" — a message that reads
// like a schema fault rather than the config mistake it is.
const GraphEmbeddingDims = 3072

// graphEmbeddingOptions selects 3072-dim embeddings for graph indexing. No
// task_type: OpenRouter's embeddings API does not accept it, so we must not
// depend on it (docs and queries both go without it, keeping the space consistent).
func graphEmbeddingOptions() gemini.EmbedOptions {
	return gemini.EmbedOptions{
		OutputDimensionality: GraphEmbeddingDims,
	}
}
