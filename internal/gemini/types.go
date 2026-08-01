package gemini

// EmbedOptions selects the shape of an embedding request.
//
// This outlived the provider client that used to live in this package. agent-mem
// no longer speaks to any LLM provider directly — every call goes through
// internal/llmgateway — but the graph still has to state how wide a vector it
// wants, because that is a property of ITS schema, not the provider's:
// observations.embedding is vector(768) while graph.artifact_index.embedding is
// halfvec(3072).
//
// Title and TaskType are carried for callers that still set them, and are
// deliberately ignored downstream. Stored vectors were produced with no
// task_type, and a query embedded WITH one lands in a different vector space
// than the corpus — search then returns quietly worse results instead of an
// error, which is the hardest kind of regression to catch.
type EmbedOptions struct {
	Title                string
	TaskType             string
	OutputDimensionality int
}
