package handlers

import (
	"context"
	"fmt"

	"github.com/agent-mem/agent-mem/internal/gemini"
	"github.com/agent-mem/agent-mem/internal/graph/jobs"
)

// geminiAdapter wraps *gemini.Client to satisfy the GeminiClient interface.
// The existing gemini.Client supports Generate + Embed; multimodal Describe is
// not yet implemented, so it returns ErrFatal so the dispatcher marks those
// jobs failed cleanly until multimodal support lands.
type geminiAdapter struct {
	c *gemini.Client
}

// NewGeminiAdapter returns a GeminiClient backed by the given *gemini.Client.
// Returns nil (safely) when c is nil.
func NewGeminiAdapter(c *gemini.Client) GeminiClient {
	if c == nil {
		return nil
	}
	return &geminiAdapter{c: c}
}

// Embed proxies directly to the underlying client.
func (a *geminiAdapter) Embed(ctx context.Context, text string) ([]float32, error) {
	return a.c.Embed(ctx, text)
}

// Generate proxies directly to the underlying client.
func (a *geminiAdapter) Generate(ctx context.Context, systemPrompt, userMessage string) (string, error) {
	return a.c.Generate(ctx, systemPrompt, userMessage)
}

// Describe is not yet implemented in the underlying REST client.
// Returns ErrFatal so the dispatcher marks the job failed cleanly instead of
// retrying indefinitely. Once multimodal support lands in gemini.Client,
// replace this stub with a real implementation.
func (a *geminiAdapter) Describe(_ context.Context, _ string, _ []byte, _ string) (string, string, []string, error) {
	return "", "", nil, fmt.Errorf("%w: Describe not yet implemented (multimodal support pending)", jobs.ErrFatal)
}
