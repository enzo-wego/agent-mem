package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
)

func setPurposeSettings(t *testing.T, pool *pgxpool.Pool, enabled bool, allowlist string) {
	t.Helper()
	ctx := context.Background()
	clear := func() {
		_, _ = pool.Exec(ctx, `DELETE FROM settings WHERE key IN ('graph.purpose.enabled','graph.purpose.allowlist')`)
	}
	clear()
	t.Cleanup(clear)
	if enabled {
		saveSetting(ctx, pool, purposeEnabledKey, "true")
	}
	if allowlist != "" {
		saveSetting(ctx, pool, purposeAllowlistKey, allowlist)
	}
}

func ppSetup(t *testing.T, enabled bool) (*pgxpool.Pool, *mockGemini, Deps) {
	t.Helper()
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)
	t.Cleanup(func() { truncateGraphHandlerTables(t, pool) })
	setPurposeSettings(t, pool, enabled, "")
	mg := &mockGemini{embedResult: func() ([]float32, error) { return make([]float32, 3072), nil }, cheapGenerateResult: func() (string, error) { return `{"purpose":"Decide X."}`, nil }}
	return pool, mg, Deps{DB: pool, Logger: zerolog.Nop(), MachineID: "test", Gemini: mg}
}

// ppNode inserts a node; nodesBody lands in nodes.body, abBody (when non-nil) in artifact_bodies.
func ppNode(t *testing.T, pool *pgxpool.Pool, id, typ, title, nodesBody string, abBody *string) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO graph.nodes (id, type, natural_key, title, body, machine_id)
VALUES ($1,$2,$1,$3,$4,'test')`, id, typ, title, nodesBody); err != nil {
		t.Fatalf("ppNode %s: %v", id, err)
	}
	if abBody != nil {
		ppBody(t, pool, id, *abBody)
	}
}

func ppBody(t *testing.T, pool *pgxpool.Pool, id, body string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `INSERT INTO graph.artifact_bodies (node_id, body_full, machine_id)
VALUES ($1,$2,'test') ON CONFLICT (node_id) DO UPDATE SET body_full = EXCLUDED.body_full`, id, body); err != nil {
		t.Fatalf("ppBody: %v", err)
	}
}

func sp(s string) *string { return &s }

func runPurpose(deps Deps, id string) error {
	p, _ := json.Marshal(summarizePurposePayload{NodeID: id})
	return NewSummarizePurposeHandler(deps).Handler(context.Background(), p)
}

type purposeRow struct {
	purpose, sig, failed string
	exists               bool
}

func readPurpose(t *testing.T, pool *pgxpool.Pool, id string) purposeRow {
	t.Helper()
	var r purposeRow
	err := pool.QueryRow(context.Background(),
		`SELECT purpose, signature, failed_signature FROM graph.artifact_purposes WHERE node_id=$1`, id).
		Scan(&r.purpose, &r.sig, &r.failed)
	r.exists = err == nil
	return r
}

func queuedPurposeJobs(t *testing.T, pool *pgxpool.Pool, id string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM graph.jobs
WHERE type='summarize_purpose' AND status='queued' AND payload->>'node_id'=$1`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPurpose_SettingOff(t *testing.T) {
	pool, mg, deps := ppSetup(t, false)
	ppNode(t, pool, "jira:P-1", "jira", "T", "", sp("body text"))
	if err := runPurpose(deps, "jira:P-1"); err != nil {
		t.Fatal(err)
	}
	if mg.cheapGenerateCalls.Load() != 0 || readPurpose(t, pool, "jira:P-1").exists {
		t.Fatal("setting off must do nothing")
	}
}

func TestPurpose_JiraStored(t *testing.T) {
	pool, mg, deps := ppSetup(t, true)
	mg.cheapGenerateResult = func() (string, error) { return `{"purpose":"  Decide   whether\tto X.  "}`, nil }
	ppNode(t, pool, "jira:P-1", "jira", "T", "", sp("body text"))
	if err := runPurpose(deps, "jira:P-1"); err != nil {
		t.Fatal(err)
	}
	r := readPurpose(t, pool, "jira:P-1")
	if mg.cheapGenerateCalls.Load() != 1 || mg.generateCalls.Load() != 0 {
		t.Fatalf("cheap=%d generate=%d", mg.cheapGenerateCalls.Load(), mg.generateCalls.Load())
	}
	if r.purpose != "Decide whether to X." || !strings.HasPrefix(r.sig, "v3:") || r.failed != "" {
		t.Fatalf("row=%+v", r)
	}
}

func TestPurpose_ConfluenceAndPR(t *testing.T) {
	pool, mg, deps := ppSetup(t, true)
	ppNode(t, pool, "cf:1", "cf", "Page", "", sp("page body"))
	ppNode(t, pool, "gh_pr:o/r#1", "gh_pr", "PR", "", sp("pr body"))
	for i, id := range []string{"cf:1", "gh_pr:o/r#1"} {
		if err := runPurpose(deps, id); err != nil {
			t.Fatal(err)
		}
		if got := mg.cheapGenerateCalls.Load(); int(got) != i+1 {
			t.Fatalf("calls=%d after %s", got, id)
		}
		if r := readPurpose(t, pool, id); r.purpose != "Decide X." {
			t.Fatalf("%s row=%+v", id, r)
		}
	}
}

func TestPurpose_UnchangedNoCall(t *testing.T) {
	pool, mg, deps := ppSetup(t, true)
	ppNode(t, pool, "jira:P-1", "jira", "T", "", sp("body"))
	for range 2 {
		if err := runPurpose(deps, "jira:P-1"); err != nil {
			t.Fatal(err)
		}
	}
	if mg.cheapGenerateCalls.Load() != 1 {
		t.Fatalf("calls=%d", mg.cheapGenerateCalls.Load())
	}
}

func TestPurpose_TextChanged(t *testing.T) {
	for name, change := range map[string]string{
		"body":  `UPDATE graph.artifact_bodies SET body_full='new body' WHERE node_id='jira:P-1'`,
		"title": `UPDATE graph.nodes SET title='New title' WHERE id='jira:P-1'`,
	} {
		t.Run(name, func(t *testing.T) {
			pool, mg, deps := ppSetup(t, true)
			ppNode(t, pool, "jira:P-1", "jira", "T", "", sp("body"))
			if err := runPurpose(deps, "jira:P-1"); err != nil {
				t.Fatal(err)
			}
			before := readPurpose(t, pool, "jira:P-1")
			mg.cheapGenerateResult = func() (string, error) { return `{"purpose":"Second."}`, nil }
			if _, err := pool.Exec(context.Background(), change); err != nil {
				t.Fatal(err)
			}
			if err := runPurpose(deps, "jira:P-1"); err != nil {
				t.Fatal(err)
			}
			after := readPurpose(t, pool, "jira:P-1")
			if mg.cheapGenerateCalls.Load() != 2 || after.purpose != "Second." || after.sig == before.sig {
				t.Fatalf("calls=%d before=%+v after=%+v", mg.cheapGenerateCalls.Load(), before, after)
			}
		})
	}
}

func TestPurpose_TruncationSignature(t *testing.T) {
	pool, mg, deps := ppSetup(t, true)
	head := strings.Repeat("a", 5999) + "é" // the 6,000th code point is multi-byte
	ppNode(t, pool, "jira:P-1", "jira", "T", "", sp(head+"tail one"))
	if err := runPurpose(deps, "jira:P-1"); err != nil {
		t.Fatal(err)
	}
	ppBody(t, pool, "jira:P-1", head+"completely different tail")
	if err := runPurpose(deps, "jira:P-1"); err != nil {
		t.Fatal(err)
	}
	if mg.cheapGenerateCalls.Load() != 1 {
		t.Fatalf("change after 6,000 code points made a call: %d", mg.cheapGenerateCalls.Load())
	}
	ppBody(t, pool, "jira:P-1", "b"+head[1:]+"tail one")
	if err := runPurpose(deps, "jira:P-1"); err != nil {
		t.Fatal(err)
	}
	if mg.cheapGenerateCalls.Load() != 2 {
		t.Fatalf("change inside 6,000 code points made no call: %d", mg.cheapGenerateCalls.Load())
	}
}

func TestPurpose_EmptyPurposeStored(t *testing.T) {
	pool, mg, deps := ppSetup(t, true)
	mg.cheapGenerateResult = func() (string, error) { return `{"purpose": ""}`, nil }
	ppNode(t, pool, "jira:P-1", "jira", "T", "", sp("body"))
	for range 2 {
		if err := runPurpose(deps, "jira:P-1"); err != nil {
			t.Fatal(err)
		}
	}
	r := readPurpose(t, pool, "jira:P-1")
	if !r.exists || r.purpose != "" || r.sig == "" || mg.cheapGenerateCalls.Load() != 1 {
		t.Fatalf("row=%+v calls=%d", r, mg.cheapGenerateCalls.Load())
	}
}

func TestPurpose_InvalidOutput(t *testing.T) {
	cases := map[string]string{
		"invalid json":  `not json`,
		"empty object":  `{}`,
		"null":          `null`,
		"null purpose":  `{"purpose": null}`,
		"number":        `{"purpose": 5}`,
		"two lines":     `{"purpose": "a\nb"}`,
		"161 code pts":  `{"purpose": "` + strings.Repeat("x", 161) + `"}`,
		"161 multibyte": `{"purpose": "` + strings.Repeat("é", 161) + `"}`,
	}
	for name, bad := range cases {
		t.Run(name, func(t *testing.T) {
			pool, mg, deps := ppSetup(t, true)
			// No prior row.
			ppNode(t, pool, "jira:N-1", "jira", "T", "", sp("body one"))
			mg.cheapGenerateResult = func() (string, error) { return bad, nil }
			if err := runPurpose(deps, "jira:N-1"); err != nil {
				t.Fatal(err)
			}
			sig := purposeSignature("T", "body one")
			if r := readPurpose(t, pool, "jira:N-1"); !r.exists || r.purpose != "" || r.sig != "" || r.failed != sig {
				t.Fatalf("no-prior row=%+v", r)
			}
			calls := mg.cheapGenerateCalls.Load()
			if err := runPurpose(deps, "jira:N-1"); err != nil || mg.cheapGenerateCalls.Load() != calls {
				t.Fatalf("rerun err=%v calls=%d want %d", err, mg.cheapGenerateCalls.Load(), calls)
			}
			ppBody(t, pool, "jira:N-1", "body two")
			if err := runPurpose(deps, "jira:N-1"); err != nil || mg.cheapGenerateCalls.Load() != calls+1 {
				t.Fatalf("after change err=%v calls=%d want %d", err, mg.cheapGenerateCalls.Load(), calls+1)
			}

			// Prior valid row.
			ppNode(t, pool, "jira:V-1", "jira", "T", "", sp("v zero"))
			mg.cheapGenerateResult = func() (string, error) { return `{"purpose":"Good."}`, nil }
			if err := runPurpose(deps, "jira:V-1"); err != nil {
				t.Fatal(err)
			}
			valid := readPurpose(t, pool, "jira:V-1")
			ppBody(t, pool, "jira:V-1", "v one")
			mg.cheapGenerateResult = func() (string, error) { return bad, nil }
			if err := runPurpose(deps, "jira:V-1"); err != nil {
				t.Fatal(err)
			}
			r := readPurpose(t, pool, "jira:V-1")
			if r.purpose != "Good." || r.sig != valid.sig || r.failed != purposeSignature("T", "v one") {
				t.Fatalf("prior-valid row=%+v", r)
			}
		})
	}
	t.Run("160 multibyte is valid", func(t *testing.T) {
		pool, mg, deps := ppSetup(t, true)
		s := strings.Repeat("é", 160)
		mg.cheapGenerateResult = func() (string, error) { return `{"purpose":"` + s + `"}`, nil }
		ppNode(t, pool, "jira:P-1", "jira", "T", "", sp("body"))
		if err := runPurpose(deps, "jira:P-1"); err != nil {
			t.Fatal(err)
		}
		if r := readPurpose(t, pool, "jira:P-1"); r.purpose != s || r.failed != "" {
			t.Fatalf("row=%+v", r)
		}
	})
}

func TestPurpose_ModelError(t *testing.T) {
	pool, mg, deps := ppSetup(t, true)
	boom := errors.New("gateway down")
	mg.cheapGenerateResult = func() (string, error) { return "", boom }
	ppNode(t, pool, "jira:P-1", "jira", "T", "", sp("body"))
	if err := runPurpose(deps, "jira:P-1"); !errors.Is(err, boom) {
		t.Fatalf("err=%v", err)
	}
	if readPurpose(t, pool, "jira:P-1").exists {
		t.Fatal("row written on model error")
	}
}

// retiredSig is the real signature of (title, body) under the retired v2 prefix.
func retiredSig(title, body string) string {
	return "v2:" + strings.TrimPrefix(purposeSignature(title, body), purposeSigVersion+":")
}

func seedV2Purpose(t *testing.T, pool *pgxpool.Pool, id, title, body string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO graph.artifact_purposes (node_id, purpose, signature) VALUES ($1,'old',$2)`,
		id, retiredSig(title, body)); err != nil {
		t.Fatal(err)
	}
}

func TestPurpose_VersionBumpRetriesFailedV2(t *testing.T) {
	pool, mg, deps := ppSetup(t, true)
	ppNode(t, pool, "jira:B-4", "jira", "T", "", sp("body"))
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO graph.artifact_purposes (node_id, purpose, signature, failed_signature) VALUES ($1,'','',$2)`,
		"jira:B-4", retiredSig("T", "body")); err != nil {
		t.Fatal(err)
	}
	if err := runPurpose(deps, "jira:B-4"); err != nil {
		t.Fatal(err)
	}
	r := readPurpose(t, pool, "jira:B-4")
	if mg.cheapGenerateCalls.Load() != 1 || r.purpose != "Decide X." || r.sig != purposeSignature("T", "body") || !strings.HasPrefix(r.sig, "v3:") || r.failed != "" {
		t.Fatalf("row=%+v calls=%d", r, mg.cheapGenerateCalls.Load())
	}
}

func TestPurpose_VersionBumpRegenerates(t *testing.T) {
	pool, mg, deps := ppSetup(t, true)
	ppNode(t, pool, "jira:B-1", "jira", "T", "", sp("body"))
	seedV2Purpose(t, pool, "jira:B-1", "T", "body")
	if err := runPurpose(deps, "jira:B-1"); err != nil {
		t.Fatal(err)
	}
	r := readPurpose(t, pool, "jira:B-1")
	if mg.cheapGenerateCalls.Load() != 1 || r.purpose != "Decide X." || r.sig != purposeSignature("T", "body") || !strings.HasPrefix(r.sig, "v3:") {
		t.Fatalf("row=%+v calls=%d", r, mg.cheapGenerateCalls.Load())
	}
}

func TestPurpose_VersionBumpInvalidClears(t *testing.T) {
	pool, mg, deps := ppSetup(t, true)
	ppNode(t, pool, "jira:B-2", "jira", "zebracrossing title", "", sp("zebracrossing body"))
	if _, err := pool.Exec(context.Background(), `INSERT INTO graph.artifact_index (node_id, summary, summary_kind, identifiers, machine_id)
VALUES ('jira:B-2','zebracrossing summary','heuristic','{}','test')`); err != nil {
		t.Fatal(err)
	}
	seedV2Purpose(t, pool, "jira:B-2", "zebracrossing title", "zebracrossing body")
	h, err := NewSearch(pool)
	if err != nil {
		t.Fatal(err)
	}
	search := func() map[string]any {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/api/graph/search?q=zebracrossing&match=hybrid", nil))
		if w.Code != 200 {
			t.Fatalf("status %d: %s", w.Code, w.Body)
		}
		var top map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &top); err != nil {
			t.Fatal(err)
		}
		rs := top["results"].([]any)
		if len(rs) != 1 {
			t.Fatalf("want node once, got %d", len(rs))
		}
		return rs[0].(map[string]any)
	}
	if m := search(); m["id"] != "jira:B-2" || m["purpose"] != "old" {
		t.Fatalf("before: %v", m)
	}
	mg.cheapGenerateResult = func() (string, error) { return `not json`, nil }
	if err := runPurpose(deps, "jira:B-2"); err != nil {
		t.Fatal(err)
	}
	r := readPurpose(t, pool, "jira:B-2")
	if r.purpose != "" || r.sig != "" || r.failed != purposeSignature("zebracrossing title", "zebracrossing body") || !strings.HasPrefix(r.failed, "v3:") {
		t.Fatalf("row=%+v", r)
	}
	if m := search(); m["id"] != "jira:B-2" {
		t.Fatalf("after: %v", m)
	} else if _, ok := m["purpose"]; ok {
		t.Fatalf("purpose key survived: %v", m)
	}
}

func TestPurpose_VersionBumpModelErrorKeeps(t *testing.T) {
	pool, mg, deps := ppSetup(t, true)
	boom := errors.New("gateway down")
	mg.cheapGenerateResult = func() (string, error) { return "", boom }
	ppNode(t, pool, "jira:B-3", "jira", "T", "", sp("body"))
	seedV2Purpose(t, pool, "jira:B-3", "T", "body")
	before := readPurpose(t, pool, "jira:B-3")
	if err := runPurpose(deps, "jira:B-3"); !errors.Is(err, boom) {
		t.Fatalf("err=%v", err)
	}
	if after := readPurpose(t, pool, "jira:B-3"); after != before || after.purpose != "old" {
		t.Fatalf("row changed: %+v -> %+v", before, after)
	}
}

func TestPurpose_PromptExact(t *testing.T) {
	const want = `You write the purpose line for a Jira ticket, Confluence page or GitHub pull request.
Reply with JSON only: {"purpose": "..."}
The purpose is ONE plain English sentence, ideally at most 20 words and 140 characters.
It says WHY the item exists: the outcome, decision or effect it is for. The reader already sees the title, so do not repeat or paraphrase it; say what the title does not.
Start with a verb or "So that". No markdown, no line breaks, no ticket keys.
Use only facts stated in the text. Do not add numbers, counts, money amounts, team or audience names, or goals that the text does not state. If unsure, leave the detail out.
If the text gives no purpose beyond the title, return {"purpose": ""}.`
	if purposeSystemPrompt != want {
		t.Fatalf("prompt differs:\n%s", purposeSystemPrompt)
	}
}

func TestPurpose_NoWork(t *testing.T) {
	type tc struct {
		name   string
		nilGem bool
		seed   func(t *testing.T, pool *pgxpool.Pool)
		id     string
	}
	node := func(id, typ, nb string, ab *string) func(*testing.T, *pgxpool.Pool) {
		return func(t *testing.T, pool *pgxpool.Pool) { ppNode(t, pool, id, typ, "T", nb, ab) }
	}
	cases := []tc{
		{"nil gemini", true, node("jira:X", "jira", "", sp("body")), "jira:X"},
		{"missing node", false, func(*testing.T, *pgxpool.Pool) {}, "jira:X"},
		{"deleted node", false, func(t *testing.T, pool *pgxpool.Pool) {
			ppNode(t, pool, "jira:X", "jira", "T", "", sp("body"))
			if _, err := pool.Exec(context.Background(), `UPDATE graph.nodes SET deleted_at=now() WHERE id='jira:X'`); err != nil {
				t.Fatal(err)
			}
		}, "jira:X"},
		{"slack node", false, node("slack:C1:1.0", "slack", "", sp("body")), "slack:C1:1.0"},
		{"jira_attachment node", false, node("jira_attachment:1", "jira_attachment", "", sp("body")), "jira_attachment:1"},
		{"no body anywhere", false, node("jira:X", "jira", "", nil), "jira:X"},
		{"whitespace nodes.body", false, node("jira:X", "jira", "  \n\t", nil), "jira:X"},
		{"empty body_full", false, node("jira:X", "jira", "", sp("")), "jira:X"},
		{"whitespace body_full over nodes.body", false, node("jira:X", "jira", "real text", sp(" \n")), "jira:X"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pool, mg, deps := ppSetup(t, true)
			if c.nilGem {
				deps.Gemini = nil
			}
			c.seed(t, pool)
			if err := runPurpose(deps, c.id); err != nil {
				t.Fatal(err)
			}
			if mg.cheapGenerateCalls.Load() != 0 || readPurpose(t, pool, c.id).exists {
				t.Fatal("expected no call and no row")
			}
		})
	}
}

func TestPurpose_BodyFallback(t *testing.T) {
	pool, mg, deps := ppSetup(t, true)
	ppNode(t, pool, "jira:P-1", "jira", "T", "node body text", nil)
	if err := runPurpose(deps, "jira:P-1"); err != nil {
		t.Fatal(err)
	}
	r := readPurpose(t, pool, "jira:P-1")
	if mg.cheapGenerateCalls.Load() != 1 || r.sig != purposeSignature("T", "node body text") {
		t.Fatalf("calls=%d row=%+v", mg.cheapGenerateCalls.Load(), r)
	}
}

func TestPurpose_StaleWriteNewerFinishesFirst(t *testing.T) {
	pool, mg, deps := ppSetup(t, true)
	ppNode(t, pool, "jira:P-1", "jira", "T", "", sp("body A"))
	started, release := make(chan struct{}), make(chan struct{})
	var n atomic.Int32
	mg.cheapGenerateResult = func() (string, error) {
		if n.Add(1) == 1 {
			close(started)
			<-release
			return `{"purpose":"Purpose A."}`, nil
		}
		return `{"purpose":"Purpose B."}`, nil
	}
	done := make(chan error, 1)
	go func() { done <- runPurpose(deps, "jira:P-1") }()
	<-started
	ppBody(t, pool, "jira:P-1", "body B")
	if err := runPurpose(deps, "jira:P-1"); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("A returned %v", err)
	}
	r := readPurpose(t, pool, "jira:P-1")
	if r.purpose != "Purpose B." || r.sig != purposeSignature("T", "body B") {
		t.Fatalf("row=%+v", r)
	}
}

func TestPurpose_StaleWriteDuringCall(t *testing.T) {
	pool, mg, deps := ppSetup(t, true)
	ppNode(t, pool, "jira:P-1", "jira", "T", "", sp("body A"))
	mg.cheapGenerateResult = func() (string, error) {
		ppBody(t, pool, "jira:P-1", "body changed")
		return `{"purpose":"Stale."}`, nil
	}
	if err := runPurpose(deps, "jira:P-1"); err != nil {
		t.Fatal(err)
	}
	if readPurpose(t, pool, "jira:P-1").exists {
		t.Fatal("stale result stored")
	}
}

func TestPurpose_ConcurrentIdenticalInput(t *testing.T) {
	pool, mg, deps := ppSetup(t, true)
	ppNode(t, pool, "jira:P-1", "jira", "T", "", sp("body"))
	var wg sync.WaitGroup
	wg.Add(2)
	mg.cheapGenerateResult = func() (string, error) {
		wg.Done()
		wg.Wait() // both calls in flight before either returns
		return `{"purpose":"Same."}`, nil
	}
	errs := make(chan error, 2)
	for range 2 {
		go func() { errs <- runPurpose(deps, "jira:P-1") }()
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	var n int
	_ = pool.QueryRow(context.Background(), `SELECT count(*) FROM graph.artifact_purposes WHERE node_id='jira:P-1'`).Scan(&n)
	r := readPurpose(t, pool, "jira:P-1")
	if n != 1 || r.purpose != "Same." || r.sig != purposeSignature("T", "body") {
		t.Fatalf("rows=%d row=%+v", n, r)
	}
}

func queuedByType(t *testing.T, pool *pgxpool.Pool, typ string) int {
	t.Helper()
	var n int
	_ = pool.QueryRow(context.Background(), `SELECT count(*) FROM graph.jobs WHERE type=$1 AND status='queued'`, typ).Scan(&n)
	return n
}

func TestPurpose_EnqueueAfterIndex(t *testing.T) {
	pool, _, deps := ppSetup(t, true)
	ctx := context.Background()
	ppNode(t, pool, "jira:P-1", "jira", "Ticket", "", sp("body text"))
	ppNode(t, pool, "slack:C1:1.0", "slack", "Msg", "hello world", sp("hello world"))
	if err := indexArtifactNode(ctx, deps, "jira:P-1", true, true); err != nil {
		t.Fatal(err)
	}
	if n := queuedPurposeJobs(t, pool, "jira:P-1"); n != 1 {
		t.Fatalf("queued=%d", n)
	}
	if err := indexArtifactNode(ctx, deps, "jira:P-1", true, true); err != nil {
		t.Fatal(err)
	}
	if n := queuedPurposeJobs(t, pool, "jira:P-1"); n != 1 {
		t.Fatalf("queued while one queued=%d", n)
	}
	if err := indexArtifactNode(ctx, deps, "slack:C1:1.0", true, true); err != nil {
		t.Fatal(err)
	}
	if n := queuedByType(t, pool, "summarize_purpose"); n != 1 {
		t.Fatalf("slack node queued a purpose job: total=%d", n)
	}
	t.Run("setting off", func(t *testing.T) {
		pool, _, deps := ppSetup(t, false)
		ppNode(t, pool, "jira:P-1", "jira", "Ticket", "", sp("body text"))
		if err := indexArtifactNode(ctx, deps, "jira:P-1", true, true); err != nil {
			t.Fatal(err)
		}
		if n := queuedByType(t, pool, "summarize_purpose"); n != 0 {
			t.Fatalf("queued=%d", n)
		}
	})
}

func TestPurpose_EnqueueOnFreshPath(t *testing.T) {
	pool, _, deps := ppSetup(t, false)
	ctx := context.Background()
	ppNode(t, pool, "jira:P-1", "jira", "Ticket", "", sp("body text"))
	if err := indexArtifactNode(ctx, deps, "jira:P-1", true, true); err != nil { // artifact_index now fresh
		t.Fatal(err)
	}
	setPurposeSettings(t, pool, true, "")
	if _, err := pool.Exec(ctx, `DELETE FROM graph.jobs WHERE type='summarize_purpose'`); err != nil {
		t.Fatal(err)
	}
	if err := indexArtifactNode(ctx, deps, "jira:P-1", false, true); err != nil { // fresh early return
		t.Fatal(err)
	}
	if n := queuedPurposeJobs(t, pool, "jira:P-1"); n != 1 {
		t.Fatalf("fresh path queued=%d", n)
	}
}

func TestPurpose_Registration(t *testing.T) {
	e := NewSummarizePurposeHandler(Deps{})
	if !e.UsesLLM || e.Lease != SummaryLease {
		t.Fatalf("UsesLLM=%v Lease=%v", e.UsesLLM, e.Lease)
	}
}

func TestPurpose_ConfigAPI(t *testing.T) {
	pool := openTestDB(t)
	setPurposeSettings(t, pool, false, "")
	h := NewChannels(pool)
	do := func(method, body string, fn func(http.ResponseWriter, *http.Request)) string {
		w := httptest.NewRecorder()
		fn(w, httptest.NewRequest(method, "/api/graph/purpose", bytes.NewBufferString(body)))
		if w.Code != 200 {
			t.Fatalf("%s status %d: %s", method, w.Code, w.Body)
		}
		return strings.TrimSpace(w.Body.String())
	}
	if got := do("GET", "", h.getPurposeConfig); got != `{"enabled":false,"allowlist":""}` {
		t.Fatalf("fresh GET=%s", got)
	}
	do("PUT", `{"enabled":true,"allowlist":" a , ,b"}`, h.putPurposeConfig)
	if got := do("GET", "", h.getPurposeConfig); got != `{"enabled":true,"allowlist":"a,b"}` {
		t.Fatalf("GET=%s", got)
	}
	if loadSetting(context.Background(), pool, purposeEnabledKey) != "true" || loadSetting(context.Background(), pool, purposeAllowlistKey) != "a,b" {
		t.Fatal("settings rows differ")
	}
	do("PUT", `{"enabled":false,"allowlist":""}`, h.putPurposeConfig)
	if got := do("GET", "", h.getPurposeConfig); got != `{"enabled":false,"allowlist":""}` {
		t.Fatalf("GET=%s", got)
	}
	if loadSetting(context.Background(), pool, purposeEnabledKey) != "false" || loadSetting(context.Background(), pool, purposeAllowlistKey) != "" {
		t.Fatal("settings rows differ after clear")
	}
}

func TestPurpose_DisplayOnly(t *testing.T) {
	pool, _, _ := ppSetup(t, false)
	ctx := context.Background()
	ppNode(t, pool, "jira:D-1", "jira", "zebracrossing alpha", "zebracrossing body", sp("zebracrossing body"))
	ppNode(t, pool, "jira:D-2", "jira", "zebracrossing beta", "zebracrossing body", sp("zebracrossing body"))
	for _, id := range []string{"jira:D-1", "jira:D-2"} {
		if _, err := pool.Exec(ctx, `INSERT INTO graph.artifact_index (node_id, summary, summary_kind, identifiers, machine_id)
VALUES ($1,'zebracrossing summary','heuristic','{}','test')`, id); err != nil {
			t.Fatal(err)
		}
	}
	h, err := NewSearch(pool)
	if err != nil {
		t.Fatal(err)
	}
	fixed := time.Now()
	h.now = func() time.Time { return fixed } // recency must not drift between the two runs
	get := func(q string) map[string]any {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/api/graph/search?"+q, nil))
		if w.Code != 200 {
			t.Fatalf("status %d: %s", w.Code, w.Body)
		}
		var top map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &top); err != nil {
			t.Fatal(err)
		}
		return top
	}
	snapshot := func() (map[string]any, string) {
		var s string
		_ = pool.QueryRow(ctx, `SELECT string_agg(node_id||'|'||summary||'|'||COALESCE(embedding::text,''), ',' ORDER BY node_id) FROM graph.artifact_index`).Scan(&s)
		return get("q=zebracrossing&match=hybrid"), s
	}
	without, idxWithout := snapshot()
	rs := without["results"].([]any)
	if len(rs) != 2 {
		t.Fatalf("fixture returned %d rows", len(rs))
	}
	for _, r := range rs {
		if _, ok := r.(map[string]any)["purpose"]; ok {
			t.Fatal("purpose key without a row")
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO graph.artifact_purposes (node_id, purpose) VALUES ('jira:D-1','Decide the zebra.'), ('jira:D-2','')`); err != nil {
		t.Fatal(err)
	}
	with, idxWith := snapshot()
	if idxWith != idxWithout {
		t.Fatal("artifact_index changed")
	}
	strip := func(top map[string]any) []map[string]any {
		var out []map[string]any
		for _, r := range top["results"].([]any) {
			m := r.(map[string]any)
			c := map[string]any{}
			for k, v := range m {
				if k != "purpose" {
					c[k] = v
				}
			}
			out = append(out, c)
		}
		return out
	}
	if !reflect.DeepEqual(strip(with), strip(without)) {
		t.Fatalf("results differ beyond purpose:\nwith=%v\nwithout=%v", strip(with), strip(without))
	}
	for _, r := range with["results"].([]any) {
		m := r.(map[string]any)
		p, ok := m["purpose"]
		switch m["id"] {
		case "jira:D-1":
			if p != "Decide the zebra." {
				t.Fatalf("D-1 purpose=%v", p)
			}
		case "jira:D-2":
			if ok {
				t.Fatalf("empty purpose serialised: %v", p)
			}
		}
	}
	for _, r := range get("q=zebracrossing")["results"].([]any) {
		if _, ok := r.(map[string]any)["purpose"]; ok {
			t.Fatal("default-mode JSON has a purpose key")
		}
	}
}

func TestPurpose_Allowlist(t *testing.T) {
	pool, mg, deps := ppSetup(t, true)
	ctx := context.Background()
	setPurposeSettings(t, pool, true, "jira:PAY-1")
	ppNode(t, pool, "jira:PAY-1", "jira", "One", "", sp("body one"))
	ppNode(t, pool, "jira:PAY-2", "jira", "Two", "", sp("body two"))
	if err := runPurpose(deps, "jira:PAY-1"); err != nil {
		t.Fatal(err)
	}
	if err := runPurpose(deps, "jira:PAY-2"); err != nil {
		t.Fatal(err)
	}
	if mg.cheapGenerateCalls.Load() != 1 || !readPurpose(t, pool, "jira:PAY-1").exists || readPurpose(t, pool, "jira:PAY-2").exists {
		t.Fatalf("calls=%d", mg.cheapGenerateCalls.Load())
	}
	for _, id := range []string{"jira:PAY-1", "jira:PAY-2"} {
		if err := indexArtifactNode(ctx, deps, id, true, true); err != nil {
			t.Fatal(err)
		}
	}
	if queuedPurposeJobs(t, pool, "jira:PAY-1") != 1 || queuedPurposeJobs(t, pool, "jira:PAY-2") != 0 {
		t.Fatal("allowlist not honoured by index_artifact")
	}
	setPurposeSettings(t, pool, true, "")
	for _, id := range []string{"jira:PAY-1", "jira:PAY-2"} {
		if err := indexArtifactNode(ctx, deps, id, true, true); err != nil {
			t.Fatal(err)
		}
	}
	if queuedPurposeJobs(t, pool, "jira:PAY-1") != 1 || queuedPurposeJobs(t, pool, "jira:PAY-2") != 1 {
		t.Fatal("cleared allowlist should queue both")
	}
}

func TestPurpose_DeletedDuringCall(t *testing.T) {
	for name, stmt := range map[string]string{
		"soft delete": `UPDATE graph.nodes SET deleted_at=now() WHERE id='jira:P-1'`,
		"hard delete": `DELETE FROM graph.nodes WHERE id='jira:P-1'`,
	} {
		t.Run(name, func(t *testing.T) {
			pool, mg, deps := ppSetup(t, true)
			ppNode(t, pool, "jira:P-1", "jira", "T", "", sp("body"))
			mg.cheapGenerateResult = func() (string, error) {
				if name == "hard delete" {
					_, _ = pool.Exec(context.Background(), `DELETE FROM graph.artifact_bodies WHERE node_id='jira:P-1'`)
				}
				if _, err := pool.Exec(context.Background(), stmt); err != nil {
					t.Error(err)
				}
				return `{"purpose":"Gone."}`, nil
			}
			if err := runPurpose(deps, "jira:P-1"); err != nil {
				t.Fatal(err)
			}
			if readPurpose(t, pool, "jira:P-1").exists {
				t.Fatal("row written for deleted node")
			}
		})
	}
}
