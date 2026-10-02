package handlers_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/agent-mem/agent-mem/internal/gemini"
	"github.com/agent-mem/agent-mem/internal/graph/handlers"
	"github.com/agent-mem/agent-mem/internal/llmgateway"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type subjectLLM struct {
	calls int
	reply string
	err   error
}

func (f *subjectLLM) GenerateCheap(context.Context, string, string) (string, error) {
	f.calls++
	return f.reply, f.err
}
func (*subjectLLM) Embed(context.Context, string) ([]float32, error) { panic("unused") }
func (*subjectLLM) EmbedWithOptions(context.Context, string, gemini.EmbedOptions) ([]float32, error) {
	panic("unused")
}
func (*subjectLLM) Describe(context.Context, string, []byte, string) (string, string, []string, error) {
	panic("unused")
}
func (*subjectLLM) Generate(context.Context, string, string) (string, error) { panic("unused") }

type subjectResponse struct {
	Queries []string `json:"queries"`
	Cached  bool     `json:"cached"`
	Error   string   `json:"error"`
}

func subjectDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := testDB(t)
	clear := func() {
		t.Helper()
		if _, err := pool.Exec(context.Background(), `DELETE FROM graph.subject_queries`); err != nil {
			t.Fatal(err)
		}
	}
	clear()
	t.Cleanup(clear)
	return pool
}
func subjectRouter(pool *pgxpool.Pool, llm handlers.GeminiClient) http.Handler {
	r := chi.NewRouter()
	r.Get("/api/graph/node/{id}/subject-queries", handlers.NewSubjectQueries(handlers.Deps{DB: pool, Gemini: llm}))
	return r
}
func subjectGet(t *testing.T, r http.Handler, id string, wantStatus int) subjectResponse {
	t.Helper()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/api/graph/node/"+id+"/subject-queries", nil))
	if w.Code != wantStatus {
		t.Fatalf("status %d, want %d: %s", w.Code, wantStatus, w.Body.String())
	}
	var resp subjectResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}
func subjectFixture(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	spNode(t, pool, "jira:PAY-9", "jira", "IN GST", "Finance requirements", "", "", "{}", 0, false)
}
func subjectNoCache(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM graph.subject_queries`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("cached %d rows on failure", n)
	}
}
func subjectWantQueries(t *testing.T, resp subjectResponse, want []string) {
	t.Helper()
	if !reflect.DeepEqual(resp.Queries, want) {
		t.Fatalf("queries %#v, want %#v", resp.Queries, want)
	}
}
func TestSubjectQueries_GeneratesAndCaches(t *testing.T) {
	pool := subjectDB(t)
	subjectFixture(t, pool)
	f := &subjectLLM{reply: `["IN GST test cases finance","Deloitte test case PDFs review"]`}
	r := subjectRouter(pool, f)
	want := []string{"IN GST test cases finance", "Deloitte test case PDFs review"}
	first := subjectGet(t, r, "jira:PAY-9", 200)
	subjectWantQueries(t, first, want)
	if first.Cached || f.calls != 1 {
		t.Fatalf("first cached=%v calls=%d", first.Cached, f.calls)
	}
	second := subjectGet(t, r, "jira:PAY-9", 200)
	subjectWantQueries(t, second, want)
	if !second.Cached || f.calls != 1 {
		t.Fatalf("second cached=%v calls=%d", second.Cached, f.calls)
	}
}
func TestSubjectQueries_BodyChangeRegenerates(t *testing.T) {
	pool := subjectDB(t)
	subjectFixture(t, pool)
	f := &subjectLLM{reply: `["IN GST test cases finance"]`}
	r := subjectRouter(pool, f)
	subjectGet(t, r, "jira:PAY-9", 200)
	if _, err := pool.Exec(context.Background(), `UPDATE graph.nodes SET body='Changed requirement' WHERE id='jira:PAY-9'`); err != nil {
		t.Fatal(err)
	}
	f.reply = `["GST invoice rounding review"]`
	resp := subjectGet(t, r, "jira:PAY-9", 200)
	subjectWantQueries(t, resp, []string{"GST invoice rounding review"})
	if resp.Cached || f.calls != 2 {
		t.Fatalf("cached=%v calls=%d", resp.Cached, f.calls)
	}
}
func TestSubjectQueries_FiltersBadQueries(t *testing.T) {
	pool := subjectDB(t)
	subjectFixture(t, pool)
	f := &subjectLLM{reply: "```json\n[\"PAY-9 status\",\"a\",\"IN GST test cases finance\",\"in gst test cases finance\",\"x y z w\"]\n```"}
	subjectWantQueries(t, subjectGet(t, subjectRouter(pool, f), "jira:PAY-9", 200), []string{"IN GST test cases finance", "x y z w"})
}
func TestSubjectQueries_LLMErrorIsEmptyNotCached(t *testing.T) {
	for _, llmErr := range []error{errors.New("LLM failed"), context.DeadlineExceeded, fmt.Errorf("wrapped: %w", llmgateway.ErrCapped)} {
		t.Run(llmErr.Error(), func(t *testing.T) {
			pool := subjectDB(t)
			subjectFixture(t, pool)
			f := &subjectLLM{err: llmErr}
			r := subjectRouter(pool, f)
			resp := subjectGet(t, r, "jira:PAY-9", 200)
			subjectWantQueries(t, resp, []string{})
			if resp.Error == "" {
				t.Fatal("missing error")
			}
			subjectNoCache(t, pool)
			f.err = nil
			f.reply = `["IN GST test cases finance"]`
			subjectWantQueries(t, subjectGet(t, r, "jira:PAY-9", 200), []string{"IN GST test cases finance"})
			if f.calls != 2 {
				t.Fatalf("calls=%d", f.calls)
			}
		})
	}
}
func TestSubjectQueries_NonJiraIs400(t *testing.T) {
	pool := subjectDB(t)
	f := &subjectLLM{}
	resp := subjectGet(t, subjectRouter(pool, f), "slack:C1:1", 400)
	if resp.Error != "subject queries are only built for jira nodes" || f.calls != 0 {
		t.Fatalf("response=%+v calls=%d", resp, f.calls)
	}
}
func TestSubjectQueries_UnparseableIsEmptyNotCached(t *testing.T) {
	pool := subjectDB(t)
	subjectFixture(t, pool)
	f := &subjectLLM{reply: "sure! here you go"}
	r := subjectRouter(pool, f)
	resp := subjectGet(t, r, "jira:PAY-9", 200)
	subjectWantQueries(t, resp, []string{})
	if resp.Cached {
		t.Fatal("invalid answer cached")
	}
	subjectNoCache(t, pool)
	f.reply = `["PAY-9 status","a"]`
	subjectWantQueries(t, subjectGet(t, r, "jira:PAY-9", 200), []string{})
	subjectNoCache(t, pool)
}
func TestSubjectQueries_MissingNodeIs404(t *testing.T) {
	pool := subjectDB(t)
	f := &subjectLLM{}
	r := subjectRouter(pool, f)
	spNode(t, pool, "jira:GONE-1", "jira", "Gone", "", "", "", "{}", 0, true)
	subjectGet(t, r, "jira:NOPE-1", 404)
	subjectGet(t, r, "jira:GONE-1", 404)
	if f.calls != 0 {
		t.Fatalf("calls=%d", f.calls)
	}
}
func TestSubjectQueries_NilLLM(t *testing.T) {
	pool := subjectDB(t)
	subjectFixture(t, pool)
	r := subjectRouter(pool, nil)
	resp := subjectGet(t, r, "jira:PAY-9", 200)
	subjectWantQueries(t, resp, []string{})
	if resp.Error != "llm not configured" {
		t.Fatalf("error=%q", resp.Error)
	}
	subjectNoCache(t, pool)
	sum := sha256.Sum256([]byte("IN GST\nFinance requirements"))
	sig := "v1:" + hex.EncodeToString(sum[:])[:16]
	if _, err := pool.Exec(context.Background(), `INSERT INTO graph.subject_queries (node_id, signature, queries) VALUES ('jira:PAY-9',$1,'["IN GST test cases finance"]')`, sig); err != nil {
		t.Fatal(err)
	}
	resp = subjectGet(t, r, "jira:PAY-9", 200)
	subjectWantQueries(t, resp, []string{"IN GST test cases finance"})
	if !resp.Cached {
		t.Fatal("nil LLM did not serve cache")
	}
	subjectGet(t, r, "jira:NOPE-1", 404)
	subjectGet(t, r, "slack:C1:1", 400)
}
