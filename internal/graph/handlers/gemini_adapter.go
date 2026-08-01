package handlers

import (
	"context"
	"sync"
	"time"

	"github.com/agent-mem/agent-mem/internal/gemini"
)

// SummaryLease is the job lease for every handler that can reach the LLM
// gateway. It must exceed the gateway's own LLM timeout
// (LLM_GATEWAY_CLAUDE_TIMEOUT_S, 180s) and the client's RequestTimeout (200s):
//
//	gateway 180s  <  client 200s  <  lease 240s
//
// A shorter lease expires mid-call, the janitor reclaims the job, and a second
// worker redoes the same work — duplicate LLM calls, the exact amplification
// shape this codebase already paid for once. Claude on a seat is slower than the
// Flash models this used to run on, so 60s was survivable before and is not now.
const SummaryLease = 240 * time.Second

// GeminiAdapter is a swappable holder for the LLM client the graph handlers use.
//
// It exists solely so a settings change reaches handlers that captured the
// interface value at RegisterAll time and never see a rebuilt Deps. There is no
// routing left to do: agent-mem speaks to exactly one LLM endpoint, the gateway.
//
// The name is historical — it once adapted a Gemini provider client. Renaming it
// touches every handler, so it stays until something else needs to move.
type GeminiAdapter struct {
	mu sync.RWMutex
	c  GeminiClient
}

// NewGeminiAdapter returns a GeminiClient wrapper, or nil when c is nil.
//
// A nil return is meaningful: handlers check `deps.Gemini == nil` and skip LLM
// work entirely, which is what should happen when no gateway is configured. It
// must be a nil INTERFACE, not a typed nil pointer inside one, or those checks
// pass and every call panics.
func NewGeminiAdapter(c GeminiClient) GeminiClient {
	if c == nil {
		return nil
	}
	return &GeminiAdapter{c: c}
}

// Swap replaces the underlying client so a settings change (gateway URL or key)
// applies to already-registered job handlers.
//
// A nil c is ignored: handlers holding a live adapter must never observe a nil
// client mid-flight. Clearing the gateway URL therefore still needs a restart to
// stop graph LLM calls — the safe direction, since the alternative is a panic.
func (a *GeminiAdapter) Swap(c GeminiClient) {
	if c == nil {
		return
	}
	a.mu.Lock()
	a.c = c
	a.mu.Unlock()
}

func (a *GeminiAdapter) client() GeminiClient {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.c
}

// Embed generates one vector at the client's configured width. This reaches
// OpenRouter on the far side of the gateway — Anthropic has no embeddings API —
// but agent-mem does not know or care which provider serves it.
func (a *GeminiAdapter) Embed(ctx context.Context, text string) ([]float32, error) {
	return a.client().Embed(ctx, text)
}

// EmbedWithOptions generates one vector at an explicit width.
func (a *GeminiAdapter) EmbedWithOptions(ctx context.Context, text string, opts gemini.EmbedOptions) ([]float32, error) {
	return a.client().EmbedWithOptions(ctx, text, opts)
}

// Generate runs the expensive tier: thread, cluster and feature summaries.
func (a *GeminiAdapter) Generate(ctx context.Context, systemPrompt, userMessage string) (string, error) {
	return a.client().Generate(ctx, systemPrompt, userMessage)
}

// GenerateCheap runs the cheap tier. The topic-link confirm gate is high-volume
// (~15 calls per node) and already receives a cosine shortlist, so it must stay
// on the cheap tier whichever model the gateway maps that to.
func (a *GeminiAdapter) GenerateCheap(ctx context.Context, systemPrompt, userMessage string) (string, error) {
	return a.client().GenerateCheap(ctx, systemPrompt, userMessage)
}

// Describe generates a multimodal attachment description.
func (a *GeminiAdapter) Describe(ctx context.Context, mimeType string, data []byte, prompt string) (string, string, []string, error) {
	return a.client().Describe(ctx, mimeType, data, prompt)
}
