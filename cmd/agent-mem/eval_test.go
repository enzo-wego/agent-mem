package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

func TestGraphContextCasesCoverManifestAndUseCases(t *testing.T) {
	root := "../../"
	paths, err := filepath.Glob(filepath.Join(root, evalCaseDir, "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 20 {
		t.Fatalf("case files=%d want=20", len(paths))
	}
	covered := map[string]bool{}
	cases := map[string]evalCase{}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var c evalCase
		if err := yaml.Unmarshal(data, &c); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		cases[c.ID] = c
		covered[c.UseCase] = true
		if c.CheckedBy != nil {
			t.Errorf("%s checked_by must be null", c.ID)
		}
		if c.LabelledBy != "none" {
			t.Errorf("%s labelled_by=%q", c.ID, c.LabelledBy)
		}
		for route, labels := range c.Labels {
			if len(labels.Must)+len(labels.May)+len(labels.MustNot) != 0 {
				t.Errorf("%s %s labels must be empty", c.ID, route)
			}
		}
		for _, name := range []string{"resolve-asker0", "resolve-asker", "search-hybrid"} {
			if _, ok := c.Requests[name]; !ok {
				t.Errorf("%s missing %s", c.ID, name)
			}
		}
	}
	for i := 1; i <= 10; i++ {
		uc := fmt.Sprintf("UC%d", i)
		if !covered[uc] {
			t.Errorf("missing %s", uc)
		}
	}
	manifestData, err := os.ReadFile(filepath.Join(root, probeManifestPath))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Probes []struct {
			ID      string         `yaml:"id"`
			Resolve map[string]any `yaml:"resolve_request"`
			Hybrid  map[string]any `yaml:"hybrid_request"`
		} `yaml:"probes"`
	}
	if err := yaml.Unmarshal(manifestData, &manifest); err != nil {
		t.Fatal(err)
	}
	for _, p := range manifest.Probes {
		c, ok := cases[p.ID]
		if !ok {
			t.Errorf("missing %s", p.ID)
			continue
		}
		if got := c.Requests["search-hybrid"].Params["match"]; got != "hybrid" {
			t.Errorf("%s match=%v", p.ID, got)
		}
		if _, ok := c.Requests["search-hybrid"].Headers["X-Asker-User"]; ok {
			t.Errorf("%s unexpectedly sends X-Asker-User", p.ID)
		}
		if _, ok := c.Requests["resolve-asker0"].Body["budget_tokens"]; ok {
			t.Errorf("%s null budget_tokens was not omitted", p.ID)
		}
	}
}

func TestRelevanceJSONContainsDocumentedResult(t *testing.T) {
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir("../.."); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Graph-Request-Id", "ctx_test")
		w.Header().Set("X-Graph-Build-Rev", "rev-test")
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/graph/resolve" {
			_, _ = fmt.Fprint(w, `{"artifacts":[]}`)
		} else {
			_, _ = fmt.Fprint(w, `{"results":[],"arm_errors":{"semantic":"test"}}`)
		}
	}))
	defer server.Close()
	out := filepath.Join(t.TempDir(), "result.json")
	cmd := &cobra.Command{}
	if err := runRelevance(cmd, server.URL, false, out); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"build_rev", "cases_sha256", "node_meta_sha256", "started_at", "per_case", "aggregate", "request_ids"} {
		if _, ok := result[key]; !ok {
			t.Errorf("missing %s in %s", key, data)
		}
	}
	if len(result["per_case"].([]any)) != 60 {
		t.Fatalf("per_case entries=%d want=60", len(result["per_case"].([]any)))
	}
}

func TestImportProbesDerivesNeedsContextAndLeavesLabelsEmpty(t *testing.T) {
	manifest, err := os.ReadFile(filepath.Join("..", "..", probeManifestPath))
	if err != nil {
		t.Fatal(err)
	}
	// probe-f becomes the UC9 (no context needed) probe.
	manifest = bytes.Replace(manifest, []byte("use_case: UC8   # closest match"), []byte("use_case: UC9   # closest match"), 1)
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(probeManifestPath)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, probeManifestPath), manifest, 0o644); err != nil {
		t.Fatal(err)
	}
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldWD) })
	if err := importProbes(); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{"probe-a": "yes", "probe-c": "yes", "probe-f": "no"} {
		data, err := os.ReadFile(filepath.Join(evalCaseDir, id+".yaml"))
		if err != nil {
			t.Fatal(err)
		}
		var c evalCase
		if err := yaml.Unmarshal(data, &c); err != nil {
			t.Fatal(err)
		}
		if c.NeedsContext != want || c.LabelledBy != "none" || c.CheckedBy != nil {
			t.Fatalf("%s needs_context=%q labelled_by=%q checked_by=%v", id, c.NeedsContext, c.LabelledBy, c.CheckedBy)
		}
	}
}
