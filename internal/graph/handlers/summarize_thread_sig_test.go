package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestThreadSummarySignature pins the cache-key format and the current version.
// The version literal is asserted directly, so a future bump has to be
// deliberate and surfaces as a failing test rather than silently invalidating
// the corpus.
func TestThreadSummarySignature(t *testing.T) {
	if threadSummarySigVersion != "v9" {
		t.Fatalf("threadSummarySigVersion = %q, want %q", threadSummarySigVersion, "v9")
	}
	if got, want := threadSummarySignature(3, 1785480000000), "v9:3:1785480000000"; got != want {
		t.Fatalf("threadSummarySignature(3, 1785480000000) = %q, want %q", got, want)
	}
}

// TestResolveStaleSummariesLimit covers the sweep handler's limit clamp, which
// is extracted from the handler so it is exercisable without a database pool:
// an empty/zero limit falls back to the default 20, and anything over the 500
// ceiling is rejected.
func TestResolveStaleSummariesLimit(t *testing.T) {
	if backfillStaleSummariesDefaultLimit != 20 {
		t.Fatalf("default limit = %d, want 20", backfillStaleSummariesDefaultLimit)
	}
	if got, ok := resolveStaleSummariesLimit(0); !ok || got != backfillStaleSummariesDefaultLimit {
		t.Fatalf("empty body limit = (%d, %v), want (%d, true)", got, ok, backfillStaleSummariesDefaultLimit)
	}
	if got, ok := resolveStaleSummariesLimit(500); !ok || got != 500 {
		t.Fatalf("limit 500 = (%d, %v), want (500, true)", got, ok)
	}
	if _, ok := resolveStaleSummariesLimit(501); ok {
		t.Fatalf("limit 501 accepted, want rejected")
	}
}

// TestBackfillStaleSummariesHandlerRejectsLargeLimit confirms limit > 500 is a
// 400 at the HTTP layer. The reject path returns before touching deps.DB, so it
// runs against a zero-value Deps with no pool — no fake DB.
func TestBackfillStaleSummariesHandlerRejectsLargeLimit(t *testing.T) {
	h := NewBackfillStaleSummariesHandler(Deps{})
	req := httptest.NewRequest(http.MethodPost, "/api/graph/backfill/stale-summaries",
		strings.NewReader(`{"limit": 501}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("limit 501 -> status %d, want %d", w.Code, http.StatusBadRequest)
	}
}
