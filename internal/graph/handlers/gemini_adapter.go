package handlers

import (
	"context"
	"fmt"

	"github.com/agent-mem/agent-mem/internal/gemini"
	"github.com/agent-mem/agent-mem/internal/graph/jobs"
)

// TextGenerator is the minimal text-generation surface (Claude or Gemini).
type TextGenerator interface {
	Generate(ctx context.Context, systemPrompt, userMessage string) (string, error)
}

// geminiAdapter wraps *gemini.Client to satisfy the GeminiClient interface.
// Embed/Describe always use Gemini; Generate is routed to gen, which is Claude
// when an Anthropic key is configured (better grounding, fewer hallucinated
// ticket ids/outcomes) and the Gemini client otherwise.
type geminiAdapter struct {
	c   *gemini.Client
	gen TextGenerator
}

// NewGeminiAdapter returns a GeminiClient. Embeddings use c (required); text
// generation uses gen when non-nil, else falls back to c. Returns nil when c is nil.
func NewGeminiAdapter(c *gemini.Client, gen TextGenerator) GeminiClient {
	if c == nil {
		return nil
	}
	if gen == nil {
		gen = c
	}
	return &geminiAdapter{c: c, gen: gen}
}

// Embed proxies directly to the underlying Gemini client.
func (a *geminiAdapter) Embed(ctx context.Context, text string) ([]float32, error) {
	return a.c.Embed(ctx, text)
}

// EmbedWithOptions proxies directly to the underlying Gemini client.
func (a *geminiAdapter) EmbedWithOptions(ctx context.Context, text string, opts gemini.EmbedOptions) ([]float32, error) {
	return a.c.EmbedWithOptions(ctx, text, opts)
}

// Generate proxies to the configured text generator (Claude or Gemini).
func (a *geminiAdapter) Generate(ctx context.Context, systemPrompt, userMessage string) (string, error) {
	return a.gen.Generate(ctx, systemPrompt, userMessage)
}

// Describe is not yet implemented in the underlying REST client.
// Returns ErrFatal so the dispatcher marks the job failed cleanly instead of
// retrying indefinitely. Once multimodal support lands in gemini.Client,
// replace this stub with a real implementation.
func (a *geminiAdapter) Describe(_ context.Context, _ string, _ []byte, _ string) (string, string, []string, error) {
	return "", "", nil, fmt.Errorf("%w: Describe not yet implemented (multimodal support pending)", jobs.ErrFatal)
}
