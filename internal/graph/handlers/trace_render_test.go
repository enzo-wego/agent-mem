package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	graphtrace "github.com/agent-mem/agent-mem/internal/graph/trace"
)

func traceDigest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// An untouched graph must render, per route and mode, exactly the title and
// summary digests the original response carried.
func TestRenderTraceRepresentationMatchesOriginalSearchRoutes(t *testing.T) {
	pool := compatibilityPool(t)
	resetEpicACL(t, pool)
	fixture, err := os.ReadFile(compatibilityFixture)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), string(fixture)); err != nil {
		t.Fatal(err)
	}
	// A Slack thread with a summary and an overview: the hybrid route shows
	// the thread summary as title and the overview as summary.
	if _, err := pool.Exec(context.Background(), `
UPDATE graph.nodes SET scope='slack:CGC0', title='' WHERE id='slack:CGC0:100.000001';
INSERT INTO graph.thread_summaries(channel_id,thread_ts,signature,summary,overview) VALUES ('CGC0','100.000001','1:1','hybrid thread title','hybrid overview')`); err != nil {
		t.Fatal(err)
	}
	search, err := NewSearchWithEmbedder(pool, compatibilityEmbedder{})
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"ordinary", "hybrid"} {
		target := "/api/graph/search?q=phase+zero"
		if mode == "hybrid" {
			target += "&match=hybrid"
		}
		store := &captureTraceStore{}
		recorder := graphtrace.NewRecorder(store, graphtrace.Options{})
		withContextTrace(search, "search", "test", recorder).ServeHTTP(httptest.NewRecorder(), adminRequest(http.MethodGet, target, nil))
		recorder.Close(time.Second)
		items := store.one(t).Items.([]map[string]any)
		if len(items) == 0 {
			t.Fatalf("%s: no items", mode)
		}
		for _, item := range items {
			got, err := RenderTraceRepresentation(context.Background(), pool, "search", mode, item["node_id"].(string), nil)
			if err != nil || !got.Found {
				t.Fatalf("%s %v: found=%t err=%v", mode, item["node_id"], got.Found, err)
			}
			if item["title_sha256"] != traceDigest(got.Title) {
				t.Errorf("%s %v title differs: current %q", mode, item["node_id"], got.Title)
			}
			if item["summary_sha256"] != traceDigest(got.Summary) {
				t.Errorf("%s %v summary differs: current %q", mode, item["node_id"], got.Summary)
			}
		}
	}
}
