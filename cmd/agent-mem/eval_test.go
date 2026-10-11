package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"

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
		if err := validateEvalCaseState(c); err != nil {
			t.Error(err)
		}
		for _, name := range []string{"resolve-admin", "resolve-asker", "search-hybrid"} {
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
		if _, ok := c.Requests["resolve-admin"].Body["budget_tokens"]; ok {
			t.Errorf("%s null budget_tokens was not omitted", p.ID)
		}
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
