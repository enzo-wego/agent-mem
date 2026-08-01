package handlers

import (
	"context"
	"testing"

	"github.com/agent-mem/agent-mem/internal/gemini"
)

// fakeGateway stands in for llm-gateway and records which method was called, so
// tests prove routing rather than just that a value came back.
type fakeGateway struct {
	out    string
	called string
}

func (f *fakeGateway) Embed(context.Context, string) ([]float32, error) {
	f.called = "Embed"
	return []float32{1}, nil
}

func (f *fakeGateway) EmbedWithOptions(context.Context, string, gemini.EmbedOptions) ([]float32, error) {
	f.called = "EmbedWithOptions"
	return []float32{1}, nil
}

func (f *fakeGateway) Generate(context.Context, string, string) (string, error) {
	f.called = "Generate"
	return f.out, nil
}

func (f *fakeGateway) GenerateCheap(context.Context, string, string) (string, error) {
	f.called = "GenerateCheap"
	return f.out, nil
}

func (f *fakeGateway) Describe(context.Context, string, []byte, string) (string, string, []string, error) {
	f.called = "Describe"
	return f.out, "", nil, nil
}

// A nil client must produce a nil INTERFACE. Handlers gate on
// `deps.Gemini == nil` to skip LLM work when no gateway is configured; a typed
// nil inside a non-nil interface passes that check and panics on first call.
func TestNewGeminiAdapterNilIsNilInterface(t *testing.T) {
	if got := NewGeminiAdapter(nil); got != nil {
		t.Fatalf("NewGeminiAdapter(nil) = %v, want a nil interface", got)
	}
}

// Every method must reach the single configured client. One quietly bypassing
// it is invisible in behaviour but would escape the gateway's metering — the
// whole reason agent-mem has exactly one LLM egress.
func TestAllMethodsReachTheClient(t *testing.T) {
	ctx := context.Background()
	gw := &fakeGateway{out: "gw"}
	ad := NewGeminiAdapter(gw)

	for _, tc := range []struct {
		want string
		call func()
	}{
		{"Generate", func() { _, _ = ad.Generate(ctx, "s", "u") }},
		{"GenerateCheap", func() { _, _ = ad.GenerateCheap(ctx, "s", "u") }},
		{"Embed", func() { _, _ = ad.Embed(ctx, "t") }},
		{"EmbedWithOptions", func() { _, _ = ad.EmbedWithOptions(ctx, "t", gemini.EmbedOptions{}) }},
		{"Describe", func() { _, _, _, _ = ad.Describe(ctx, "image/png", []byte{1}, "p") }},
	} {
		gw.called = ""
		tc.call()
		if gw.called != tc.want {
			t.Errorf("%s did not reach the client (called=%q) — it would bypass the gateway", tc.want, gw.called)
		}
	}
}

func TestGeminiAdapterSwap(t *testing.T) {
	ctx := context.Background()
	ad, ok := NewGeminiAdapter(&fakeGateway{out: "one"}).(*GeminiAdapter)
	if !ok {
		t.Fatal("NewGeminiAdapter did not return *GeminiAdapter")
	}
	if out, _ := ad.Generate(ctx, "s", "u"); out != "one" {
		t.Fatalf("Generate before swap = %q, want one", out)
	}

	ad.Swap(&fakeGateway{out: "two"})
	if out, _ := ad.Generate(ctx, "s", "u"); out != "two" {
		t.Fatalf("Generate after swap = %q, want two", out)
	}

	// A nil swap is ignored. Handlers hold this adapter for the life of the
	// process, so accepting nil would turn a cleared setting into a panic on the
	// next job rather than a config change.
	ad.Swap(nil)
	if out, _ := ad.Generate(ctx, "s", "u"); out != "two" {
		t.Fatalf("Generate after nil swap = %q, want two (swap ignored)", out)
	}
}
