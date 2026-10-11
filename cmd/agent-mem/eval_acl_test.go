package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"
)

type capturedEvalRequest struct {
	variant string
	admin   string
	asker   string
	caller  string
	eeid    int
}

// captureEvalRequests records the ACL headers of every request and then hands
// the request to next, so the harness sees a normal scripted server.
type captureEvalRequests struct {
	mu   sync.Mutex
	seen []capturedEvalRequest
	next http.HandlerFunc
}

func (c *captureEvalRequests) handler(w http.ResponseWriter, r *http.Request) {
	rec := capturedEvalRequest{admin: r.Header.Get("X-Graph-Admin"), asker: r.Header.Get("X-Asker-User"), caller: r.Header.Get("X-Graph-Caller")}
	if r.URL.Path == "/api/graph/resolve" {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		var b struct {
			AskerEEID int `json:"asker_eeid"`
		}
		_ = json.Unmarshal(body, &b)
		rec.eeid = b.AskerEEID
		rec.variant = "resolve-asker"
		if b.AskerEEID == 0 {
			rec.variant = "resolve-admin"
		}
	} else {
		rec.variant = "search-hybrid"
	}
	c.mu.Lock()
	c.seen = append(c.seen, rec)
	c.mu.Unlock()
	c.next(w, r)
}

// assertVariantHeaders: the admin header only on resolve-admin; resolve-asker
// carries its asker in the body; search-hybrid carries the case asker eeid in
// X-Asker-User and never the admin header.
func assertVariantHeaders(t *testing.T, seen []capturedEvalRequest, wantAsker string) {
	t.Helper()
	counts := map[string]int{}
	for _, r := range seen {
		counts[r.variant]++
		if r.caller != "eval" {
			t.Errorf("%s: X-Graph-Caller=%q", r.variant, r.caller)
		}
		switch r.variant {
		case "resolve-admin":
			if r.admin != "1" || r.asker != "" {
				t.Errorf("resolve-admin: admin=%q asker=%q, want admin=1 and no asker", r.admin, r.asker)
			}
		case "resolve-asker":
			if r.admin != "" || r.asker != "" || r.eeid == 0 {
				t.Errorf("resolve-asker: admin=%q asker=%q eeid=%d, want body eeid only", r.admin, r.asker, r.eeid)
			}
		case "search-hybrid":
			if r.admin != "" || r.asker != wantAsker {
				t.Errorf("search-hybrid: admin=%q asker=%q, want asker=%q and no admin", r.admin, r.asker, wantAsker)
			}
		}
	}
	for _, v := range evalVariants {
		if counts[v] == 0 {
			t.Errorf("variant %s sent no request (%v)", v, counts)
		}
	}
}

func TestEvalRelevanceSendsPerVariantACLHeaders(t *testing.T) {
	dataDir := relevanceFixture(t)
	t.Setenv("AGENT_MEM_API_KEY", "k")
	rs := &relevanceServer{calls: map[string]int{}}
	cap := &captureEvalRequests{next: rs.handler}
	server := httptest.NewServer(http.HandlerFunc(cap.handler))
	defer server.Close()
	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)
	if err := runRelevance(cmd, relevanceOpts{BaseURL: server.URL, JSONPath: filepath.Join(t.TempDir(), "r.json"), DataDir: dataDir, EnvPath: "/nonexistent"}); err != nil {
		t.Fatalf("%v\n%s", err, buf.String())
	}
	assertVariantHeaders(t, cap.seen, "1") // fixture cases carry asker_eeid: 1
}

func TestEvalPrepareSendsPerVariantACLHeaders(t *testing.T) {
	pool, _ := evalTestDB(t)
	seedEvalNodes(t, pool, "CACL")
	root := "slack:CACL:1.000000"
	ps := &prepareServer{byQ: map[string][]string{"qa": {root}}, fail: map[string]bool{}}
	cap := &captureEvalRequests{next: ps.handler}
	server := httptest.NewServer(http.HandlerFunc(cap.handler))
	defer server.Close()
	dataDir := filepath.Join(t.TempDir(), "gc")
	writeTestCase(t, dataDir, testCaseSpec{ID: "qa", Query: "qa", Seeds: []string{root}})
	t.Setenv("AGENT_MEM_API_KEY", "")
	var buf bytes.Buffer
	if err := runEvalPrepare(t.Context(), &buf, evalPrepareOpts{BaseURL: server.URL, DataDir: dataDir, EnvPath: "/nonexistent", Pool: pool}); err != nil {
		t.Fatalf("%v\n%s", err, buf.String())
	}
	assertVariantHeaders(t, cap.seen, "1")

	// label-sheet reads the saved replay (renamed keys) with no HTTP call.
	server.Close()
	data, err := os.ReadFile(filepath.Join(dataDir, "replay.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "resolve-asker0") || !strings.Contains(string(data), `"resolve-admin"`) {
		t.Fatalf("replay.json keys not renamed: %s", data)
	}
	outDir := filepath.Join(t.TempDir(), "sheets")
	if err := runLabelSheet(t.Context(), &buf, labelSheetOpts{All: true, OutDir: outDir, DataDir: dataDir, EnvPath: "/nonexistent", Pool: pool}); err != nil {
		t.Fatalf("label-sheet with the server down: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outDir, "qa.md")); err != nil {
		t.Fatal(err)
	}
}

const checkedInEvalData = "../../testdata/eval/graph_context"

// The dry run over the 20 checked-in cases prints the new variant set with
// the headers each request will carry, from the same value the HTTP path uses.
func TestEvalRelevanceDryRunShowsNewVariantsAndHeaders(t *testing.T) {
	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)
	if err := runRelevance(cmd, relevanceOpts{DryRun: true, DataDir: checkedInEvalData, EnvPath: "/nonexistent"}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if strings.Contains(out, "resolve-asker0") {
		t.Fatal("dry run still names resolve-asker0")
	}
	lineCount := map[string]int{}
	for _, line := range strings.Split(out, "\n") {
		trim := strings.TrimSpace(line)
		for _, v := range evalVariants {
			if !strings.HasPrefix(trim, v+":") {
				continue
			}
			lineCount[v]++
			hasAdmin := strings.Contains(line, "X-Graph-Admin:1")
			hasAsker := regexp.MustCompile(`X-Asker-User:\d+`).MatchString(line)
			switch v {
			case "resolve-admin":
				if !hasAdmin || hasAsker {
					t.Errorf("resolve-admin headers wrong: %s", line)
				}
			case "resolve-asker":
				if hasAdmin || hasAsker {
					t.Errorf("resolve-asker headers wrong: %s", line)
				}
			case "search-hybrid":
				if hasAdmin || !hasAsker {
					t.Errorf("search-hybrid headers wrong: %s", line)
				}
			}
		}
	}
	for _, v := range evalVariants {
		if lineCount[v] != 20 {
			t.Errorf("%s printed for %d cases, want 20", v, lineCount[v])
		}
	}
}

// Only the requests block of each case file changed: everything else (labels,
// label_reasons, seeds) is byte-equal to the base commit.
func TestEvalCaseLabelsUnchangedFromBase(t *testing.T) {
	const base = "483d03d"
	if err := exec.Command("git", "cat-file", "-e", base+"^{commit}").Run(); err != nil {
		if os.Getenv("AGENT_MEM_SKIP_GIT_HISTORY_TESTS") == "1" {
			t.Skipf("base commit %s not available and AGENT_MEM_SKIP_GIT_HISTORY_TESTS=1: %v", base, err)
		}
		t.Fatalf("base commit %s not available (shallow clone?); fetch full history or set AGENT_MEM_SKIP_GIT_HISTORY_TESTS=1: %v", base, err)
	}
	paths, err := filepath.Glob(filepath.Join(checkedInEvalData, "cases", "*.yaml"))
	if err != nil || len(paths) != 20 {
		t.Fatalf("cases=%d err=%v", len(paths), err)
	}
	withoutRequests := func(b []byte) string {
		lines := strings.Split(string(b), "\n")
		var out []string
		in := false
		for _, l := range lines {
			if strings.HasPrefix(l, "requests:") {
				in = true
				continue
			}
			if in && strings.HasPrefix(l, "  ") {
				continue
			}
			in = false
			out = append(out, l)
		}
		return strings.Join(out, "\n")
	}
	for _, p := range paths {
		rel, _ := filepath.Rel("../..", p)
		old, err := exec.Command("git", "show", base+":"+filepath.ToSlash(rel)).Output()
		if err != nil {
			t.Fatalf("git show %s: %v", rel, err)
		}
		cur, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if withoutRequests(old) != withoutRequests(cur) {
			t.Errorf("%s: content outside requests changed", rel)
		}
		if strings.Contains(string(cur), "resolve-asker0") || !strings.Contains(string(cur), "resolve-admin:") {
			t.Errorf("%s: variant key not renamed", rel)
		}
	}
}
