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
// shape this codebase already paid for once. Claude on a seat is slower than
// Gemini Flash, so 60s was survivable before and is not now.
const SummaryLease = 240 * time.Second

// GeminiAdapter satisfies GeminiClient and decides, per call, whether it is
// served by llm-gateway or by the direct Gemini client.
//
// When a gateway is configured it takes ALL five methods, not a subset: the
// point of the gateway is to be the single egress for LLM traffic, so that
// metering, alerting and failover have one place to live. Splitting some calls
// past it would reintroduce the blind spot it exists to remove.
//
// Both clients are swappable via Swap so settings changes take effect without a
// worker restart; every read goes through the mutex.
type GeminiAdapter struct {
	mu sync.RWMutex
	c  *gemini.Client // direct provider client; also the fallback when gw is nil
	gw GeminiClient   // llm-gateway, or nil when llm_gateway_url is empty
}

// NewGeminiAdapter returns a GeminiClient. c is required — it remains the
// fallback whenever no gateway is configured, and clearing the gateway URL must
// never leave handlers without a client. Returns nil when c is nil.
func NewGeminiAdapter(c *gemini.Client, gw GeminiClient) GeminiClient {
	if c == nil {
		return nil
	}
	return &GeminiAdapter{c: c, gw: gw}
}

// Swap replaces the underlying clients so settings changes (graph_gemini_model,
// llm_provider, llm_gateway_url) apply to already-registered job handlers.
//
// A nil c is ignored: handlers holding a live adapter must never see a nil
// client, so clearing the LLM key still requires a restart to disable graph LLM
// calls. A nil gw IS applied — that is how clearing llm_gateway_url turns the
// gateway off without a restart.
func (a *GeminiAdapter) Swap(c *gemini.Client, gw GeminiClient) {
	if c == nil {
		return
	}
	a.mu.Lock()
	a.c, a.gw = c, gw
	a.mu.Unlock()
}

// geminiDirect adapts *gemini.Client to GeminiClient. The only gap is
// GenerateCheap: the provider client has a single Generate, and "cheap" is
// expressed by which model the client was built with (graph_gemini_model), not
// by a second method. Behind the gateway the tier is a request field instead.
type geminiDirect struct{ c *gemini.Client }

func (g geminiDirect) Embed(ctx context.Context, text string) ([]float32, error) {
	return g.c.Embed(ctx, text)
}

func (g geminiDirect) EmbedWithOptions(ctx context.Context, text string, opts gemini.EmbedOptions) ([]float32, error) {
	return g.c.EmbedWithOptions(ctx, text, opts)
}

func (g geminiDirect) Generate(ctx context.Context, systemPrompt, userMessage string) (string, error) {
	return g.c.Generate(ctx, systemPrompt, userMessage)
}

func (g geminiDirect) GenerateCheap(ctx context.Context, systemPrompt, userMessage string) (string, error) {
	return g.c.Generate(ctx, systemPrompt, userMessage)
}

func (g geminiDirect) Describe(ctx context.Context, mimeType string, data []byte, prompt string) (string, string, []string, error) {
	return g.c.Describe(ctx, mimeType, data, prompt)
}

// route returns the client that should serve this call: the gateway when one is
// configured, else the direct provider client.
func (a *GeminiAdapter) route() GeminiClient {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.gw != nil {
		return a.gw
	}
	return geminiDirect{a.c}
}

// Embed generates one vector at the client's configured width.
//
// Through the gateway this still reaches OpenRouter — Anthropic has no
// embeddings API. Routing it here buys one key and one retry path, not a
// cheaper or better provider.
func (a *GeminiAdapter) Embed(ctx context.Context, text string) ([]float32, error) {
	return a.route().Embed(ctx, text)
}

// EmbedWithOptions generates one vector at an explicit width.
func (a *GeminiAdapter) EmbedWithOptions(ctx context.Context, text string, opts gemini.EmbedOptions) ([]float32, error) {
	return a.route().EmbedWithOptions(ctx, text, opts)
}

// Generate runs the expensive tier: thread, cluster and feature summaries.
func (a *GeminiAdapter) Generate(ctx context.Context, systemPrompt, userMessage string) (string, error) {
	return a.route().Generate(ctx, systemPrompt, userMessage)
}

// GenerateCheap runs the cheap tier. The topic-link confirm gate is high-volume
// (~15 calls per node) and already receives a cosine shortlist, so it must stay
// on the cheap model whichever backend serves it — that volume on the expensive
// model is what a seat's five-hour window cannot absorb.
func (a *GeminiAdapter) GenerateCheap(ctx context.Context, systemPrompt, userMessage string) (string, error) {
	return a.route().GenerateCheap(ctx, systemPrompt, userMessage)
}

// Describe generates a multimodal attachment description.
func (a *GeminiAdapter) Describe(ctx context.Context, mimeType string, data []byte, prompt string) (string, string, []string, error) {
	return a.route().Describe(ctx, mimeType, data, prompt)
}
