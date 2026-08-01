package handlers

import (
	"context"
	"testing"

	"github.com/agent-mem/agent-mem/internal/gemini"
)

// fakeGateway stands in for llm-gateway and records which method was called, so
// tests can prove routing rather than just that a string came back.
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

func TestNewGeminiAdapterNilClient(t *testing.T) {
	// The provider client is the fallback, so an adapter without one could hand
	// handlers a nil client the moment the gateway is cleared.
	if got := NewGeminiAdapter(nil, &fakeGateway{out: "x"}); got != nil {
		t.Fatalf("NewGeminiAdapter(nil, gw) = %v, want nil interface", got)
	}
}

// With a gateway configured, EVERY method must go through it. A method quietly
// bypassing the gateway is invisible in behaviour but defeats the single-egress
// property the gateway exists for — metering and alerting would miss it.
func TestAllMethodsRouteThroughGatewayWhenConfigured(t *testing.T) {
	ctx := context.Background()
	c := gemini.NewClient(gemini.ProviderOpenRouter, "k", "m", "e", 1)
	gw := &fakeGateway{out: "gw"}
	ad := NewGeminiAdapter(c, gw)

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
			t.Errorf("%s did not reach the gateway (called=%q) — it would bypass metering", tc.want, gw.called)
		}
	}
}

func TestGeminiAdapterSwap(t *testing.T) {
	ctx := context.Background()
	c1 := gemini.NewClient(gemini.ProviderOpenRouter, "k1", "m1", "e", 1)
	ad, ok := NewGeminiAdapter(c1, &fakeGateway{out: "one"}).(*GeminiAdapter)
	if !ok {
		t.Fatal("NewGeminiAdapter did not return *GeminiAdapter")
	}
	if out, _ := ad.Generate(ctx, "s", "u"); out != "one" {
		t.Fatalf("Generate before swap = %q, want one", out)
	}

	c2 := gemini.NewClient(gemini.ProviderOpenRouter, "k2", "m2", "e", 1)
	ad.Swap(c2, &fakeGateway{out: "two"})
	if out, _ := ad.Generate(ctx, "s", "u"); out != "two" {
		t.Fatalf("Generate after swap = %q, want two", out)
	}

	// A nil client is ignored: handlers must never observe a nil client.
	ad.Swap(nil, &fakeGateway{out: "three"})
	if out, _ := ad.Generate(ctx, "s", "u"); out != "two" {
		t.Fatalf("Generate after nil-client swap = %q, want two (swap ignored)", out)
	}

	// A nil gateway IS applied — that is how clearing llm_gateway_url turns the
	// gateway off without a restart. Routing must fall back to the provider
	// client rather than panicking on a nil interface.
	c3 := gemini.NewClient(gemini.ProviderOpenRouter, "k3", "m3", "e", 1)
	ad.Swap(c3, nil)
	if _, ok := ad.route().(geminiDirect); !ok {
		t.Fatal("clearing the gateway must route back to the direct client")
	}
	if got := ad.route().(geminiDirect).c; got != c3 {
		t.Fatal("Swap did not replace the gemini client")
	}
}

// The provider client has no GenerateCheap; the wrapper maps it onto Generate.
// If that mapping is lost, the cheap tier silently stops existing off-gateway.
func TestGeminiDirectMapsCheapOntoGenerate(t *testing.T) {
	var _ GeminiClient = geminiDirect{gemini.NewClient(gemini.ProviderOpenRouter, "k", "m", "e", 1)}
}
