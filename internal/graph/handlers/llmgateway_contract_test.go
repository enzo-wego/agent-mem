package handlers_test

import (
	"testing"

	"github.com/agent-mem/agent-mem/internal/graph/handlers"
	"github.com/agent-mem/agent-mem/internal/llmgateway"
)

// The client must satisfy the whole surface it replaces. A compile-time
// assertion is cheaper than discovering a missing method at wiring time.
var _ handlers.GeminiClient = (*llmgateway.Client)(nil)

// gateway 180s < client 200s < lease 240s. Editing one must trip on the others.
func TestSummaryLeaseExceedsGatewayRequestTimeout(t *testing.T) {
	if handlers.SummaryLease <= llmgateway.RequestTimeout {
		t.Errorf("SummaryLease %v must exceed RequestTimeout %v, else leases expire mid-call",
			handlers.SummaryLease, llmgateway.RequestTimeout)
	}
}
