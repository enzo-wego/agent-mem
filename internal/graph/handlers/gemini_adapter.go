package handlers

import (
	"context"
	"sync"
	"time"

	"github.com/agent-mem/agent-mem/internal/gemini"
)

// TextGenerator is the minimal text-generation surface for summaries.
// internal/llmgateway implements it, putting summaries on a Claude subscription
// seat. The previous implementation called the Anthropic API directly and is
// gone for good: per-token billing with no ceiling is how an amplification bug
// quietly spent ~$11/hour. A seat rate-limits instead.
type TextGenerator interface {
	Generate(ctx context.Context, systemPrompt, userMessage string) (string, error)
}

// SummaryLease is the job lease for every handler whose work can reach
// TextGenerator, i.e. the gateway. It must exceed the gateway's own LLM timeout
// (LLM_GATEWAY_CLAUDE_TIMEOUT_S, 180s) and the client's RequestTimeout (200s):
//
//	gateway 180s  <  client 200s  <  lease 240s
//
// A shorter lease expires mid-call, the janitor reclaims the job, and a second
// worker redoes the same summary — duplicate LLM calls, the exact amplification
// shape this codebase already paid for once. Sonnet on a seat is slower than
// Gemini Flash, so 60s was survivable before and is not now.
const SummaryLease = 240 * time.Second

// GeminiAdapter wraps *gemini.Client to satisfy the GeminiClient interface.
// Embed/Describe always use Gemini; Generate is routed to gen when one is
// supplied, and to the Gemini client otherwise.
//
// The underlying clients are swappable via Swap so settings changes take
// effect without a worker restart; all reads go through the mutex.
type GeminiAdapter struct {
	mu  sync.RWMutex
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
	return &GeminiAdapter{c: c, gen: gen}
}

// Swap replaces the underlying clients so settings changes (graph_gemini_model,
// llm_provider) apply to already-registered job handlers.
// A nil gen falls back to c, mirroring NewGeminiAdapter. A nil c is ignored:
// handlers holding a live adapter must never see a nil client, so clearing the
// LLM key still requires a worker restart to disable graph LLM calls.
func (a *GeminiAdapter) Swap(c *gemini.Client, gen TextGenerator) {
	if c == nil {
		return
	}
	if gen == nil {
		gen = c
	}
	a.mu.Lock()
	a.c, a.gen = c, gen
	a.mu.Unlock()
}

func (a *GeminiAdapter) clients() (*gemini.Client, TextGenerator) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.c, a.gen
}

// Embed proxies directly to the underlying Gemini client.
func (a *GeminiAdapter) Embed(ctx context.Context, text string) ([]float32, error) {
	c, _ := a.clients()
	return c.Embed(ctx, text)
}

// EmbedWithOptions proxies directly to the underlying Gemini client.
func (a *GeminiAdapter) EmbedWithOptions(ctx context.Context, text string, opts gemini.EmbedOptions) ([]float32, error) {
	c, _ := a.clients()
	return c.EmbedWithOptions(ctx, text, opts)
}

// Generate proxies to the configured text generator (Claude or Gemini).
func (a *GeminiAdapter) Generate(ctx context.Context, systemPrompt, userMessage string) (string, error) {
	_, gen := a.clients()
	return gen.Generate(ctx, systemPrompt, userMessage)
}

// GenerateCheap always uses Gemini Flash instead of the optional Claude summary
// generator. The topic-link confirm gate is high-volume and already receives a
// cosine shortlist, so it should stay on the cheap model.
func (a *GeminiAdapter) GenerateCheap(ctx context.Context, systemPrompt, userMessage string) (string, error) {
	c, _ := a.clients()
	return c.Generate(ctx, systemPrompt, userMessage)
}

// Describe proxies multimodal attachment description to the Gemini client.
func (a *GeminiAdapter) Describe(ctx context.Context, mimeType string, data []byte, prompt string) (string, string, []string, error) {
	c, _ := a.clients()
	return c.Describe(ctx, mimeType, data, prompt)
}
