package handlers_test

import (
	"testing"

	"github.com/agent-mem/agent-mem/internal/graph/handlers"
	"github.com/agent-mem/agent-mem/internal/llmgateway"
)

// The client must satisfy the whole surface it replaces. A compile-time
// assertion is cheaper than discovering a missing method at wiring time.
var _ handlers.GeminiClient = (*llmgateway.Client)(nil)

// Permit wait plus the HTTP timeout must fit inside the lease. Editing any of
// the three values must trip this contract before jobs can be reclaimed mid-call.
func TestSummaryLeaseExceedsGatewayAdmissionAndRequestTimeout(t *testing.T) {
	total := llmgateway.PermitWait + llmgateway.RequestTimeout
	if handlers.SummaryLease <= total {
		t.Errorf("SummaryLease %v must exceed PermitWait + RequestTimeout %v, else leases expire mid-call",
			handlers.SummaryLease, total)
	}
}
