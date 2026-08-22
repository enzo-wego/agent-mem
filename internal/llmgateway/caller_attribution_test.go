// Package llmgateway_test holds tests that must run from outside the package
// so that callerName() sees a non-llmgateway frame as the first caller.
package llmgateway_test

import (
	"strings"
	"testing"

	"github.com/agent-mem/agent-mem/internal/llmgateway"
)

// TestCallerAttributionExternal verifies callerName returns a non-empty name
// that is NOT the llmgateway package itself when called from outside.
// The external test package is "llmgateway_test" by Go convention; that is
// acceptable — it is not the implementation package "llmgateway".
func TestCallerAttributionExternal(t *testing.T) {
	name := llmgateway.CallerNameForTesting()
	if name == "" {
		t.Error("callerName returned empty string")
	}
	// The name must not be a frame from the implementation package itself.
	// "llmgateway." (with a dot) is what an llmgateway implementation frame looks like.
	// "llmgateway_test." is the external test package — that is the expected frame here.
	if strings.Contains(name, "llmgateway.") {
		t.Errorf("callerName returned an implementation-package frame: %s", name)
	}
	// Must contain the test function — confirms attribution points at the real caller.
	if !strings.Contains(name, "TestCallerAttributionExternal") {
		t.Errorf("callerName = %q, want it to contain 'TestCallerAttributionExternal'", name)
	}
}

type GeminiAdapter struct{}

//go:noinline
func (*GeminiAdapter) Generate() string {
	return llmgateway.CallerNameForTesting()
}

func TestCallerAttributionSkipsGeminiAdapter(t *testing.T) {
	name := (&GeminiAdapter{}).Generate()
	if strings.Contains(name, "(*GeminiAdapter)") {
		t.Fatalf("callerName attributed the adapter shim: %q", name)
	}
	if !strings.Contains(name, "TestCallerAttributionSkipsGeminiAdapter") {
		t.Fatalf("callerName = %q, want outer test function", name)
	}
}
