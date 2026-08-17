package handlers

import (
	"encoding/json"
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
// an empty/zero limit falls back to the default 100, and anything over the 500
// ceiling is rejected.
func TestResolveStaleSummariesLimit(t *testing.T) {
	if backfillStaleSummariesDefaultLimit != 100 {
		t.Fatalf("default limit = %d, want 100", backfillStaleSummariesDefaultLimit)
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

func TestSkipJudgingPayloadJSONRoundTrip(t *testing.T) {
	linkTopicsTarget := &linkTopicsPayload{}
	indexArtifactTarget := &indexArtifactPayload{}
	summarizeThreadTarget := &summarizeThreadPayload{}
	tests := []struct {
		name        string
		payload     any
		target      any
		skipJudging func() bool
	}{
		{
			name:        "link_topics",
			payload:     linkTopicsPayload{SkipJudging: true},
			target:      linkTopicsTarget,
			skipJudging: func() bool { return linkTopicsTarget.SkipJudging },
		},
		{
			name:        "index_artifact",
			payload:     indexArtifactPayload{SkipJudging: true},
			target:      indexArtifactTarget,
			skipJudging: func() bool { return indexArtifactTarget.SkipJudging },
		},
		{
			name:        "summarize_thread",
			payload:     summarizeThreadPayload{SkipJudging: true},
			target:      summarizeThreadTarget,
			skipJudging: func() bool { return summarizeThreadTarget.SkipJudging },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := json.Marshal(tt.payload)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var wire map[string]any
			if err := json.Unmarshal(data, &wire); err != nil {
				t.Fatalf("unmarshal wire object: %v", err)
			}
			if got, ok := wire["skip_judging"].(bool); !ok || !got {
				t.Fatalf("skip_judging = %#v, want true: %s", wire["skip_judging"], data)
			}
			if err := json.Unmarshal(data, tt.target); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if !tt.skipJudging() {
				t.Fatalf("SkipJudging lost in JSON round-trip: %s", data)
			}
		})
	}
}

// TestStaleSummaryQueriesShareLiveNodeGuard pins the agent-mem-9ll fix: the
// sweep's capped SELECT and its remaining COUNT(*) must carry the identical
// live-node predicate, so an orphaned row (Slack nodes gone) is never selected
// again. They share staleSummaryWhereSQL for exactly that reason. This asserts
// the fragment still carries the `deleted_at IS NULL` / EXISTS guard, and that
// both full statements embed it — so a future edit that inlines one query and
// drops the guard, or weakens the shared fragment, fails here instead of
// silently reopening the wall.
func TestStaleSummaryQueriesShareLiveNodeGuard(t *testing.T) {
	if !strings.Contains(staleSummaryWhereSQL, "deleted_at IS NULL") {
		t.Fatalf("staleSummaryWhereSQL missing live-node guard \"deleted_at IS NULL\":\n%s", staleSummaryWhereSQL)
	}
	if !strings.Contains(staleSummaryWhereSQL, "EXISTS") {
		t.Fatalf("staleSummaryWhereSQL missing EXISTS live-node guard:\n%s", staleSummaryWhereSQL)
	}
	if !strings.Contains(staleSummaryCountSQL, staleSummaryWhereSQL) {
		t.Fatalf("staleSummaryCountSQL does not embed the shared WHERE fragment:\n%s", staleSummaryCountSQL)
	}
	if !strings.Contains(staleSummarySelectSQL, staleSummaryWhereSQL) {
		t.Fatalf("staleSummarySelectSQL does not embed the shared WHERE fragment:\n%s", staleSummarySelectSQL)
	}
}
