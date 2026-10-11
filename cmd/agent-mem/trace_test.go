package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/spf13/cobra"
)

func traceTestPool(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping integration test")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil || cfg.ConnConfig.Database != "agentmem_test" {
		t.Fatalf("DATABASE_URL must select agentmem_test: %v", err)
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool, dsn
}

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func seedResolveTrace(t *testing.T, pool *pgxpool.Pool, id string) (returnedBody string) {
	t.Helper()
	ctx := context.Background()
	node := "slack:CTRACE:300.000001"
	full := strings.Repeat("body ", 80)
	returnedBody = full[:100]
	if _, err := pool.Exec(ctx, `DELETE FROM graph.context_traces WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`DELETE FROM graph.thread_summaries WHERE channel_id='CTRACE'`,
		`DELETE FROM graph.artifact_bodies WHERE node_id='slack:CTRACE:300.000001'`,
		`DELETE FROM graph.nodes WHERE id='slack:CTRACE:300.000001'`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO graph.nodes(id,type,natural_key,title,scope,metadata,machine_id) VALUES ($1,'slack',$1,'','slack:CTRACE','{"thread_ts":"300.000001"}','test')`, node); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO graph.artifact_bodies(node_id,body_full,machine_id) VALUES ($1,$2,'test')`, node, full); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO graph.thread_summaries(channel_id,thread_ts,signature,summary) VALUES ('CTRACE','300.000001','1:1','thread summary title')`); err != nil {
		t.Fatal(err)
	}
	items := fmt.Sprintf(`[{"node_id":%q,"rank":0,"title_sha256":%q,"body_included":true,"body_truncated":true,"body_bytes":%d,"body_sha256":%q}]`, node, sha("thread summary title"), len(returnedBody), sha(returnedBody))
	if _, err := pool.Exec(ctx, `INSERT INTO graph.context_traces(id,created_at,route,build_rev,caller,status,asker_input,acl_mode,request,items,items_truncated,response_sha256,latency_ms)
VALUES ($1,$2,'resolve','test','test',200,'{}','unfiltered','{}',$3,false,'x',1)`, id, time.Now(), items); err != nil {
		t.Fatal(err)
	}
	return returnedBody
}

func runTraceShow(t *testing.T, dsn, id string) string {
	t.Helper()
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	if err := traceShow(cmd, dsn, id, "writer counters: test"); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func TestTraceShowResolveUsesHydrateRepresentation(t *testing.T) {
	pool, dsn := traceTestPool(t)
	seedResolveTrace(t, pool, "ctx_trace_show_same")
	out := runTraceShow(t, dsn, "ctx_trace_show_same")
	if !strings.Contains(out, "title: same as returned") || !strings.Contains(out, "body: same as returned") {
		t.Fatalf("untouched node not like-for-like:\n%s", out)
	}
	if _, err := pool.Exec(context.Background(), `UPDATE graph.artifact_bodies SET body_full='edited '||body_full WHERE node_id='slack:CTRACE:300.000001'`); err != nil {
		t.Fatal(err)
	}
	out = runTraceShow(t, dsn, "ctx_trace_show_same")
	if !strings.Contains(out, "body: differs from returned") {
		t.Fatalf("edited node not detected:\n%s", out)
	}
}

func TestTraceShowDetectsAppendedTextOnUntruncatedItem(t *testing.T) {
	pool, dsn := traceTestPool(t)
	seedResolveTrace(t, pool, "ctx_trace_show_append")
	ctx := context.Background()
	var full string
	if err := pool.QueryRow(ctx, `SELECT body_full FROM graph.artifact_bodies WHERE node_id='slack:CTRACE:300.000001'`).Scan(&full); err != nil {
		t.Fatal(err)
	}
	// Rewrite the trace as an untruncated item whose recorded body is the full body.
	items := fmt.Sprintf(`[{"node_id":"slack:CTRACE:300.000001","rank":0,"title_sha256":%q,"body_included":true,"body_truncated":false,"body_bytes":%d,"body_sha256":%q}]`, sha("thread summary title"), len(full), sha(full))
	if _, err := pool.Exec(ctx, `UPDATE graph.context_traces SET items=$1 WHERE id='ctx_trace_show_append'`, items); err != nil {
		t.Fatal(err)
	}
	if out := runTraceShow(t, dsn, "ctx_trace_show_append"); !strings.Contains(out, "body: same as returned") {
		t.Fatalf("untouched untruncated item:\n%s", out)
	}
	if _, err := pool.Exec(ctx, `UPDATE graph.artifact_bodies SET body_full=body_full||' appended tail' WHERE node_id='slack:CTRACE:300.000001'`); err != nil {
		t.Fatal(err)
	}
	if out := runTraceShow(t, dsn, "ctx_trace_show_append"); !strings.Contains(out, "body: differs from returned") {
		t.Fatalf("appended text not detected:\n%s", out)
	}
}

func TestTraceSinceAcceptsDays(t *testing.T) {
	d, err := parseTraceSince("7d")
	if err != nil || d != 7*24*time.Hour {
		t.Fatalf("7d=%s err=%v", d, err)
	}
	if _, err := parseTraceSince("nonsense"); err == nil {
		t.Fatal("invalid duration accepted")
	}
}

func TestTraceShowNotFoundPrintsWriterCounters(t *testing.T) {
	_, dsn := traceTestPool(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/graph/traces/stats" {
			http.NotFound(w, r)
			return
		}
		_, _ = fmt.Fprint(w, `{"Attempted":7,"Written":4,"Dropped":2,"Failed":1}`)
	}))
	defer server.Close()
	out := runTraceShowWithCounters(t, dsn, "ctx_never_written", writerCountersLine(server.URL, ""))
	if !strings.Contains(out, "not found (never written, dropped, or older than 30 days)") || !strings.Contains(out, "attempted=7 written=4 dropped=2 failed=1") {
		t.Fatalf("not-found output:\n%s", out)
	}
}

func runTraceShowWithCounters(t *testing.T, dsn, id, counters string) string {
	t.Helper()
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	if err := traceShow(cmd, dsn, id, counters); err != nil {
		t.Fatal(err)
	}
	return out.String()
}
