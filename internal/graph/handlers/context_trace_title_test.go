package handlers

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	graphtrace "github.com/agent-mem/agent-mem/internal/graph/trace"
)

func TestResolveTraceTitleSourceRequiresActualThreadSummary(t *testing.T) {
	pool := compatibilityPool(t)
	fixture, err := os.ReadFile(compatibilityFixture)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), string(fixture)); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(), `
INSERT INTO graph.nodes(id,type,natural_key,title,body,scope,metadata,machine_id) VALUES
 ('jira_attachment:empty','jira_attachment','jira_attachment:empty','','','public','{}','test'),
 ('slack:CGC0:200.000001','slack','slack:CGC0:200.000001','','','slack:CGC0','{"thread_ts":"200.000001"}','test');
INSERT INTO graph.artifact_bodies(node_id,body_full,machine_id) VALUES ('slack:CGC0:200.000001','body without summary','test')`); err != nil {
		t.Fatal(err)
	}
	resolve, err := NewResolve(pool)
	if err != nil {
		t.Fatal(err)
	}
	for _, seed := range []string{"jira_attachment:empty", "slack:CGC0:200.000001"} {
		store := &captureTraceStore{}
		recorder := graphtrace.NewRecorder(store, graphtrace.Options{})
		r := adminRequest(http.MethodPost, "/api/graph/resolve", bytes.NewBufferString(`{"seeds":["`+seed+`"]}`))
		withContextTrace(resolve, "resolve", "test", recorder).ServeHTTP(httptest.NewRecorder(), r)
		recorder.Close(time.Second)
		items := store.one(t).Items.([]map[string]any)
		if len(items) == 0 || items[0]["title_source"] != "node" {
			t.Fatalf("seed=%s items=%v", seed, items)
		}
	}
}
