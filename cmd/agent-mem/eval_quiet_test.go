package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"
)

// Sentinels that must never reach the operator in quiet mode. leadAQuietQuery
// is the case query: it lands in the search request URL (query string) and in
// the resolve request body. leadAQuietBaseToken rides in the base URL's query
// string, so the very first (warm-up) request error carries it.
const (
	leadAQuietQuery     = "SENTINEL-QUERY-7f3a91"
	leadAQuietBaseToken = "SENTINEL-BASE-c20d44"
)

// leadAQuietData builds a one-case data dir whose query is the sentinel.
func leadAQuietData(t *testing.T) string {
	t.Helper()
	dataDir := filepath.Join(t.TempDir(), "gc")
	writeTestCase(t, dataDir, testCaseSpec{ID: "quietcase", Query: leadAQuietQuery, Seeds: []string{"seed"}, LabelledBy: "fable-draft", Must: []string{"must1"}, May: []string{"may1"}, MustNot: []string{"bad1"}})
	cases, _, err := loadEvalCases(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		out, err := rewriteCanonicalSeeds(c.raw, []string{"seed"})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(c.path, out, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	meta := map[string]nodeMeta{"seed": {Type: "slack", Exists: true}, "must1": {Type: "slack", Exists: true}, "may1": {Type: "slack", Exists: true}, "bad1": {Type: "jira", Exists: true}}
	data, _ := json.Marshal(meta)
	if err := os.WriteFile(filepath.Join(dataDir, "node_meta.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	return dataDir
}

// leadAQuietServer answers per request according to mode; "transport-late"
// answers each request key once (the warm-up) and then drops the connection.
type leadAQuietServer struct {
	mode  string
	mu    sync.Mutex
	calls map[string]int
}

func (s *leadAQuietServer) handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	key := r.Method + " " + r.URL.RequestURI() + "|" + string(body)
	s.mu.Lock()
	s.calls[key]++
	n := s.calls[key]
	s.mu.Unlock()
	drop := func() {
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			conn.Close()
		}
	}
	list := func() {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Graph-Request-Id", fmt.Sprintf("req-%d", n))
		if r.URL.Path == "/api/graph/resolve" {
			fmt.Fprint(w, `{"artifacts":[{"node_id":"seed"},{"node_id":"must1"},{"node_id":"may1"}]}`)
		} else {
			fmt.Fprint(w, `{"results":[{"node_id":"must1"},{"node_id":"may1"}]}`)
		}
	}
	switch s.mode {
	case "ok":
		list()
	case "transport-late":
		if n == 1 {
			list()
			return
		}
		drop()
	case "http500":
		http.Error(w, "boom "+leadAQuietQuery, http.StatusInternalServerError)
	case "decode":
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, "this is not json")
	default:
		http.Error(w, "bad mode", http.StatusTeapot)
	}
}

type leadAQuietRun struct {
	out, errOut, procOut, procErr string
	err                           error
	jsonBytes                     []byte
}

// leadAQuietCapture also captures the process-level stdout/stderr, so that
// anything written to os.Stdout/os.Stderr (not through the cobra command) is
// seen as well.
func leadAQuietCapture(t *testing.T, fn func()) (stdout, stderr string) {
	t.Helper()
	oldOut, oldErr := os.Stdout, os.Stderr
	ro, wo, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	re, we, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout, os.Stderr = wo, we
	var wg sync.WaitGroup
	var bo, be bytes.Buffer
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(&bo, ro) }()
	go func() { defer wg.Done(); _, _ = io.Copy(&be, re) }()
	func() {
		defer func() {
			os.Stdout, os.Stderr = oldOut, oldErr
			wo.Close()
			we.Close()
		}()
		fn()
	}()
	wg.Wait()
	return bo.String(), be.String()
}

// leadAQuietRelevance runs runRelevance under a cobra root so the CLI's own
// error printing (stderr) is captured as well.
func leadAQuietRelevance(t *testing.T, o relevanceOpts, withJSON bool) leadAQuietRun {
	t.Helper()
	t.Setenv("AGENT_MEM_API_KEY", "")
	var res leadAQuietRun
	jsonPath := ""
	if withJSON {
		jsonPath = filepath.Join(t.TempDir(), "result.json")
		o.JSONPath = jsonPath
	}
	if o.EnvPath == "" {
		o.EnvPath = "/nonexistent"
	}
	var outBuf, errBuf bytes.Buffer
	root := &cobra.Command{Use: "relevance", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return runRelevance(cmd, o)
	}}
	root.SetOut(&outBuf)
	root.SetErr(&errBuf)
	root.SetArgs([]string{})
	res.procOut, res.procErr = leadAQuietCapture(t, func() { res.err = root.Execute() })
	res.out, res.errOut = outBuf.String(), errBuf.String()
	if withJSON {
		if data, err := os.ReadFile(jsonPath); err == nil {
			res.jsonBytes = data
		}
	}
	return res
}

func (r leadAQuietRun) everything() string {
	errText := ""
	if r.err != nil {
		errText = r.err.Error()
	}
	return strings.Join([]string{r.out, r.errOut, r.procOut, r.procErr, errText, string(r.jsonBytes)}, "\n--\n")
}

// leadAAssertNoLeak fails if any captured channel carries a URL, host, request
// path or sentinel.
func leadAAssertNoLeak(t *testing.T, r leadAQuietRun, serverURL string) {
	t.Helper()
	host := strings.TrimPrefix(serverURL, "http://")
	all := r.everything()
	for _, bad := range []string{leadAQuietQuery, leadAQuietBaseToken, serverURL, host, "127.0.0.1", "http://", "https://", "/api/graph", "?q=", "?token", "headers=", "body="} {
		if bad != "" && strings.Contains(all, bad) {
			t.Fatalf("quiet output leaks %q:\n%s", bad, all)
		}
	}
}

type leadAQuietJSON struct {
	PerCase []struct {
		CaseID  string `json:"case_id"`
		Variant string `json:"variant"`
		Failed  bool   `json:"failed"`
		Runs    []struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		} `json:"runs"`
	} `json:"per_case"`
	Aggregate map[string]variantAgg `json:"aggregate"`
}

func leadAParseJSON(t *testing.T, r leadAQuietRun) leadAQuietJSON {
	t.Helper()
	var out leadAQuietJSON
	if len(r.jsonBytes) == 0 {
		t.Fatalf("no --json output; err=%v out=%s", r.err, r.out)
	}
	if err := json.Unmarshal(r.jsonBytes, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestLeadAQuietWarmupTransportError: the unreachable-server error of the very
// first request is a fixed message plus category in quiet mode.
func TestLeadAQuietWarmupTransportError(t *testing.T) {
	dataDir := leadAQuietData(t)
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	base := deadURL + "?token=" + leadAQuietBaseToken

	quiet := leadAQuietRelevance(t, relevanceOpts{BaseURL: base, DataDir: dataDir, Quiet: true}, true)
	if quiet.err == nil {
		t.Fatalf("unreachable server must fail; out=%s", quiet.out)
	}
	if quiet.err.Error() != "eval relevance failed: transport" {
		t.Fatalf("quiet error = %q", quiet.err.Error())
	}
	leadAAssertNoLeak(t, quiet, deadURL)
	if !strings.Contains(quiet.errOut, "eval relevance failed: transport") {
		t.Fatalf("CLI stderr should carry the fixed message: %q", quiet.errOut)
	}
	if !strings.Contains(quiet.out, "case quietcase") {
		t.Fatalf("quiet output should still name the case: %q", quiet.out)
	}

	// Control: without --quiet the same run reports the URL (unchanged).
	loud := leadAQuietRelevance(t, relevanceOpts{BaseURL: base, DataDir: dataDir}, false)
	if loud.err == nil || !strings.Contains(loud.err.Error(), "unreachable server") || !strings.Contains(loud.err.Error(), leadAQuietBaseToken) {
		t.Fatalf("non-quiet error changed: %v", loud.err)
	}
}

// TestLeadAQuietMeasuredRunFailures: failures after a good warm-up are
// recorded and printed as a category only, and still counted.
func TestLeadAQuietMeasuredRunFailures(t *testing.T) {
	cases := []struct {
		name, mode, category string
		wantNonQuiet         string // raw text a non-quiet run still shows
	}{
		{"transport", "transport-late", "transport", leadAQuietQuery},
		{"http500", "http500", "http_500", "HTTP 500"},
		{"decode", "decode", "decode", "decode response"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dataDir := leadAQuietData(t)
			srv := &leadAQuietServer{mode: tc.mode, calls: map[string]int{}}
			server := httptest.NewServer(http.HandlerFunc(srv.handler))
			defer server.Close()

			quiet := leadAQuietRelevance(t, relevanceOpts{BaseURL: server.URL, DataDir: dataDir, Quiet: true}, true)
			if quiet.err != nil {
				t.Fatalf("per-run failures must not abort the run: %v\n%s", quiet.err, quiet.out)
			}
			leadAAssertNoLeak(t, quiet, server.URL)

			// Failure counts are still reported, as categories.
			if !strings.Contains(quiet.out, "FAILED (0/10 runs succeeded)") {
				t.Fatalf("failure not reported:\n%s", quiet.out)
			}
			if !strings.Contains(quiet.out, "failed=1 (quietcase)") || !strings.Contains(quiet.out, "failed runs (10):") {
				t.Fatalf("failure counts missing:\n%s", quiet.out)
			}
			if got := strings.Count(quiet.out, "quietcase/"); got != 3*10 {
				t.Fatalf("failed run lines = %d, want 30:\n%s", got, quiet.out)
			}
			if got := strings.Count(quiet.out, "#1: "+tc.category+"\n"); got != 3 {
				t.Fatalf("category %q lines = %d, want 3:\n%s", tc.category, got, quiet.out)
			}
			res := leadAParseJSON(t, quiet)
			if len(res.PerCase) != 3 {
				t.Fatalf("per_case = %d", len(res.PerCase))
			}
			for _, pc := range res.PerCase {
				if !pc.Failed || len(pc.Runs) != 10 {
					t.Fatalf("%s: failed=%v runs=%d", pc.Variant, pc.Failed, len(pc.Runs))
				}
				for _, run := range pc.Runs {
					if run.OK || run.Error != tc.category {
						t.Fatalf("%s: run = %+v, want error %q", pc.Variant, run, tc.category)
					}
				}
			}
			for _, variant := range evalVariants {
				agg := res.Aggregate[variant]
				if agg.Failed != 1 || len(agg.FailedIDs) != 1 || agg.FailedIDs[0] != "quietcase" || len(agg.FailedRuns) != 10 {
					t.Fatalf("%s aggregate: %+v", variant, agg)
				}
				for i, f := range agg.FailedRuns {
					if want := fmt.Sprintf("quietcase/%s#%d: %s", variant, i+1, tc.category); f != want {
						t.Fatalf("%s failed_runs[%d] = %q, want %q", variant, i, f, want)
					}
				}
			}

			// Control: without --quiet the raw detail is still reported.
			srv2 := &leadAQuietServer{mode: tc.mode, calls: map[string]int{}}
			server2 := httptest.NewServer(http.HandlerFunc(srv2.handler))
			defer server2.Close()
			loud := leadAQuietRelevance(t, relevanceOpts{BaseURL: server2.URL, DataDir: dataDir}, true)
			if loud.err != nil {
				t.Fatalf("non-quiet run failed: %v", loud.err)
			}
			if !strings.Contains(loud.out, tc.wantNonQuiet) {
				t.Fatalf("non-quiet output lost its detail %q:\n%s", tc.wantNonQuiet, loud.out)
			}
			if !strings.Contains(loud.out, "POST /api/graph/resolve") || !strings.Contains(loud.out, "GET /api/graph/search?") {
				t.Fatalf("non-quiet output must still print request URLs:\n%s", loud.out)
			}
		})
	}
}

// TestLeadAQuietSetupErrorsAreFixed: every error returned in quiet mode is a
// fixed message plus category, never a path or wrapped cause.
func TestLeadAQuietSetupErrorsAreFixed(t *testing.T) {
	dataDir := leadAQuietData(t)
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()

	missingBase := leadAQuietRelevance(t, relevanceOpts{DataDir: dataDir, Quiet: true}, false)
	if missingBase.err == nil || missingBase.err.Error() != "eval relevance failed: setup" {
		t.Fatalf("missing base url: %v", missingBase.err)
	}
	if strings.Contains(missingBase.everything(), "--base-url") {
		t.Fatalf("setup error leaks detail: %s", missingBase.everything())
	}

	if err := os.Remove(filepath.Join(dataDir, "node_meta.json")); err != nil {
		t.Fatal(err)
	}
	missingOracle := leadAQuietRelevance(t, relevanceOpts{BaseURL: server.URL, DataDir: dataDir, Quiet: true}, false)
	if missingOracle.err == nil || missingOracle.err.Error() != "eval relevance failed: setup" {
		t.Fatalf("missing oracle: %v", missingOracle.err)
	}
	if strings.Contains(missingOracle.everything(), dataDir) || strings.Contains(missingOracle.everything(), "node_meta") {
		t.Fatalf("setup error leaks the path: %s", missingOracle.everything())
	}

	nonQuiet := leadAQuietRelevance(t, relevanceOpts{BaseURL: server.URL, DataDir: dataDir}, false)
	if nonQuiet.err == nil || !strings.Contains(nonQuiet.err.Error(), "oracle") {
		t.Fatalf("non-quiet error changed: %v", nonQuiet.err)
	}
}

var leadAQuietLatency = regexp.MustCompile(`(?m)^  latency: .*\n`)

// TestLeadAQuietHealthyRunMatchesNonQuiet: on a healthy server quiet differs
// from non-quiet only by the request echo lines.
func TestLeadAQuietHealthyRunMatchesNonQuiet(t *testing.T) {
	dataDir := leadAQuietData(t)
	srvQ := &leadAQuietServer{mode: "ok", calls: map[string]int{}}
	serverQ := httptest.NewServer(http.HandlerFunc(srvQ.handler))
	defer serverQ.Close()
	srvL := &leadAQuietServer{mode: "ok", calls: map[string]int{}}
	serverL := httptest.NewServer(http.HandlerFunc(srvL.handler))
	defer serverL.Close()

	quiet := leadAQuietRelevance(t, relevanceOpts{BaseURL: serverQ.URL, DataDir: dataDir, Quiet: true}, true)
	loud := leadAQuietRelevance(t, relevanceOpts{BaseURL: serverL.URL, DataDir: dataDir}, true)
	if quiet.err != nil || loud.err != nil {
		t.Fatalf("quiet=%v loud=%v", quiet.err, loud.err)
	}
	leadAAssertNoLeak(t, quiet, serverQ.URL)

	for _, want := range []string{"  resolve-admin: POST /api/graph/resolve headers=", "  resolve-asker: POST /api/graph/resolve headers=", "  search-hybrid: GET /api/graph/search?", "q=" + leadAQuietQuery, `"query":"` + leadAQuietQuery + `"`} {
		if !strings.Contains(loud.out, want) {
			t.Fatalf("non-quiet output lost %q:\n%s", want, loud.out)
		}
	}
	echo := regexp.MustCompile(`(?m)^  (resolve-admin|resolve-asker|search-hybrid): .*$`)
	loudStripped := echo.ReplaceAllString(loud.out, "  $1")
	norm := func(s string) string { return leadAQuietLatency.ReplaceAllString(s, "") }
	if norm(loudStripped) != norm(quiet.out) {
		t.Fatalf("quiet output differs beyond request echo\nquiet:\n%s\nnon-quiet stripped:\n%s", quiet.out, loudStripped)
	}

	// Metrics in the JSON are the same too (ids ranked identically).
	rq, rl := leadAParseJSON(t, quiet), leadAParseJSON(t, loud)
	for _, v := range evalVariants {
		a, b := rq.Aggregate[v], rl.Aggregate[v]
		if a.Cases != b.Cases || a.Scored != b.Scored || a.Failed != b.Failed || a.Samples != b.Samples || a.Primary != b.Primary {
			t.Fatalf("%s aggregate differs: %+v vs %+v", v, a, b)
		}
		if a.Samples != 10 || a.Failed != 0 || len(a.FailedRuns) != 0 {
			t.Fatalf("%s healthy aggregate: %+v", v, a)
		}
	}
}

// TestLeadAQuietFlagRegistered: `eval relevance --quiet` exists and defaults off.
func TestLeadAQuietFlagRegistered(t *testing.T) {
	var relevance *cobra.Command
	for _, c := range newEvalCmd().Commands() {
		if c.Name() == "relevance" {
			relevance = c
		}
	}
	if relevance == nil {
		t.Fatal("eval relevance command missing")
	}
	f := relevance.Flags().Lookup("quiet")
	if f == nil || f.DefValue != "false" {
		t.Fatalf("--quiet flag = %+v", f)
	}
}
