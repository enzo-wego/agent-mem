package main

import (
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
		if c.CheckedBy != nil {
			t.Errorf("%s checked_by must be null", c.ID)
		}
		if c.LabelledBy != "fable-draft" {
			t.Errorf("%s labelled_by=%q", c.ID, c.LabelledBy)
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
