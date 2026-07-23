package handlers

import (
	"context"
	"testing"

	"github.com/agent-mem/agent-mem/internal/gemini"
)

type fakeGen struct{ out string }

func (f fakeGen) Generate(_ context.Context, _, _ string) (string, error) { return f.out, nil }

func TestNewGeminiAdapterNilClient(t *testing.T) {
	if got := NewGeminiAdapter(nil, fakeGen{"x"}); got != nil {
		t.Fatalf("NewGeminiAdapter(nil, gen) = %v, want nil interface", got)
	}
}

func TestGeminiAdapterSwap(t *testing.T) {
	ctx := context.Background()
	c1 := gemini.NewClient(gemini.ProviderOpenRouter, "k1", "m1", "e", 1)
	ad, ok := NewGeminiAdapter(c1, fakeGen{"one"}).(*GeminiAdapter)
	if !ok {
		t.Fatal("NewGeminiAdapter did not return *GeminiAdapter")
	}

	if out, _ := ad.Generate(ctx, "s", "u"); out != "one" {
		t.Fatalf("Generate before swap = %q, want one", out)
	}

	c2 := gemini.NewClient(gemini.ProviderOpenRouter, "k2", "m2", "e", 1)
	ad.Swap(c2, fakeGen{"two"})
	if out, _ := ad.Generate(ctx, "s", "u"); out != "two" {
		t.Fatalf("Generate after swap = %q, want two", out)
	}
	if c, _ := ad.clients(); c != c2 {
		t.Fatal("Swap did not replace the gemini client")
	}

	// nil client is ignored: handlers must never observe a nil client.
	ad.Swap(nil, fakeGen{"three"})
	if out, _ := ad.Generate(ctx, "s", "u"); out != "two" {
		t.Fatalf("Generate after nil-client swap = %q, want two (swap ignored)", out)
	}

	// nil gen falls back to the gemini client, mirroring the constructor.
	c3 := gemini.NewClient(gemini.ProviderOpenRouter, "k3", "m3", "e", 1)
	ad.Swap(c3, nil)
	if c, gen := ad.clients(); c != c3 || gen != any(c3) {
		t.Fatal("Swap(nil gen) must fall back to the gemini client")
	}
}
