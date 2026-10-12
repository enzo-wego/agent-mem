package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Regression tests for the code-r1 review of R1b Part A (findings 4-12).

func TestParseLeadResponseRejectsNullAndEmpty(t *testing.T) {
	for name, body := range map[string]string{
		"hop null":     `{"artifacts":[{"node_id":"a","hop":null,"authored_by_bot":"human"}]}`,
		"node_id null": `{"artifacts":[{"node_id":null,"hop":0,"authored_by_bot":"human"}]}`,
		"author null":  `{"artifacts":[{"node_id":"a","hop":0,"authored_by_bot":null}]}`,
		"kind null":    `{"artifacts":[{"node_id":"a","hop":0,"authored_by_bot":"human","links":[{"kind":null,"from":"a","to":"b"}]}]}`,
		"kind empty":   `{"artifacts":[{"node_id":"a","hop":0,"authored_by_bot":"human","links":[{"kind":"","from":"a","to":"b"}]}]}`,
		"from null":    `{"artifacts":[{"node_id":"a","hop":0,"authored_by_bot":"human","links":[{"kind":"X","from":null,"to":"b"}]}]}`,
		"to null":      `{"artifacts":[{"node_id":"a","hop":0,"authored_by_bot":"human","links":[{"kind":"X","from":"a","to":null}]}]}`,
	} {
		if got, _ := parseLeadResponse(leadCall(200, nil, body)); got != outcomeDecodeError {
			t.Errorf("%s: outcome = %s, want %s", name, got, outcomeDecodeError)
		}
	}
}

func TestHubCheckoutAndGuard(t *testing.T) {
	base := t.TempDir()
	hub := filepath.Join(base, "hub")
	wt := filepath.Join(base, "wt")
	for _, d := range []string{filepath.Join(hub, ".git", "worktrees", "wt"), wt, filepath.Join(base, "other")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+filepath.Join(hub, ".git", "worktrees", "wt")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := hubCheckout(wt); got != hub {
		t.Fatalf("hubCheckout = %q, want %q", got, hub)
	}
	if !insideAnyCheckout(filepath.Join(hub, "newsnap"), wt) {
		t.Error("hub checkout must be refused")
	}
	if !insideAnyCheckout(filepath.Join(wt, "a", "b"), wt) {
		t.Error("worktree must be refused")
	}
	if insideAnyCheckout(filepath.Join(base, "other", "x"), wt) {
		t.Error("unrelated dir must be allowed")
	}
	// Same directory spelled with different case (case-insensitive file systems).
	upper := filepath.Join(base, "WT")
	if _, err := os.Stat(upper); err == nil {
		if !insideAnyCheckout(filepath.Join(upper, "newsnap"), wt) {
			t.Error("case-variant spelling of the repo must be refused")
		}
	}
}

func TestRelationRowsRefuseCasesShape(t *testing.T) {
	p := filepath.Join(t.TempDir(), "rows.json")
	if err := os.WriteFile(p, []byte(`{"cases":{"qa":{"slack:CA:2.000000":{"relation":"same_case","reason":"same_ref"}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := readRows(p)
	wantCategory(t, "cases key", err, "row_shape")
}

func TestRelationCheckValuesNotJustPresence(t *testing.T) {
	f := newLeadFixture(t, defaultFixtureSpecs())
	valid := f.validLabels()
	raw, _ := os.ReadFile(valid)
	mutate := func(fn func(d map[string]any)) string {
		var d map[string]any
		if err := json.Unmarshal(raw, &d); err != nil {
			t.Fatal(err)
		}
		fn(d)
		data, _ := json.MarshalIndent(d, "", "  ")
		p := filepath.Join(t.TempDir(), "labels.json")
		_ = os.WriteFile(p, data, 0o600)
		return p
	}
	_, err := f.check(mutate(func(d map[string]any) { d["adjudication"] = "anything" }))
	wantCategory(t, "adjudication", err, "adjudication")
	_, err = f.check(mutate(func(d map[string]any) { d["build_rev"] = "other-rev" }))
	wantCategory(t, "build_rev", err, "build_rev_mismatch")

	// empty snapshot_sha256 on both sides is refused by the shared lead validation.
	f.mutateReplay(func(m map[string]any) { m["snapshot_sha256"] = "" })
	_, err = f.check(mutate(func(d map[string]any) { d["snapshot_sha256"] = "" }))
	wantCategory(t, "empty snapshot digest", err, "snapshot_missing")
}

func TestLeadSeedsPresent(t *testing.T) {
	items := []leadItem{{ID: "root", Hop: 0}, {ID: "x", Hop: 1}}
	meta := map[string]nodeMeta{
		"root": {Exists: true}, "x": {Exists: true},
		"reply": {Exists: true, ThreadRoot: "root"},
		"gone":  {Exists: false},
	}
	if !leadSeedsPresent([]string{"root", "reply"}, items, meta) {
		t.Error("seed and reply-to-root seed must be present")
	}
	if leadSeedsPresent([]string{"x"}, items, meta) {
		t.Error("existing non-hop-0 seed must fail")
	}
	if !leadSeedsPresent([]string{"gone", "unknown"}, items, meta) {
		t.Error("exists=false or unmapped seed must be skipped")
	}
}

// Mask-then-cut vs cut-then-mask: a 14-char unlabelled ref straddling the cut
// leaves a 6-char fragment that no ref rule can catch if the cut comes first.
func TestLeadLabelSheetMasksBeforeCutAtBoundary(t *testing.T) {
	ref := "ABCDEFGH123456"
	for _, tc := range []struct {
		name        string
		title, summ string
	}{
		{"summary", "t", strings.Repeat("x ", 297) + ref + " end"},
		{"title", strings.Repeat("x ", 97) + ref + " end", "s"},
	} {
		specs := []fxCase{{id: "qa", query: "q", seeds: []string{"slack:CA:1.000000"}, items: []fxItem{
			{id: "slack:CA:1.000000", hop: 0, title: "s", summary: "s"},
			{id: "slack:CA:2.000000", hop: 1, title: tc.title, summary: tc.summ},
		}}}
		f := newLeadFixture(t, specs)
		out := newOut(t)
		if _, err := f.sheet(out); err != nil {
			t.Fatal(err)
		}
		data, _ := os.ReadFile(filepath.Join(out, "qa.md"))
		if strings.Contains(string(data), "ABCDE") {
			t.Errorf("%s: ref fragment leaked across the cut", tc.name)
		}
		if !strings.Contains(string(data), "ref-1") {
			t.Errorf("%s: masked alias missing", tc.name)
		}
	}
}

// The real cleanup path: the snapshot is written, then writing replay.json
// fails; the snapshot dir must be removed.
func TestPrepareRemovesSnapshotWhenFinalWriteFails(t *testing.T) {
	pool, _ := evalTestDB(t)
	seedEvalNodes(t, pool, "CLEAD")
	root := "slack:CLEAD:1.000000"
	ps := &prepareServer{byQ: map[string][]string{"qa": {root}}, fail: map[string]bool{}}
	server := httptest.NewServer(http.HandlerFunc(ps.handler))
	defer server.Close()
	dataDir := filepath.Join(t.TempDir(), "gc")
	writeTestCase(t, dataDir, testCaseSpec{ID: "qa", Query: "qa", Seeds: []string{root}})
	// replay.json is a non-empty directory: the final rename fails.
	if err := os.MkdirAll(filepath.Join(dataDir, "replay.json", "x"), 0o755); err != nil {
		t.Fatal(err)
	}
	snap := filepath.Join(t.TempDir(), "snap")
	t.Setenv("AGENT_MEM_API_KEY", "")
	var buf bytes.Buffer
	err := runEvalPrepare(context.Background(), &buf, evalPrepareOpts{Snapshot: snap, BaseURL: server.URL, DataDir: dataDir, EnvPath: "/nonexistent", Pool: pool})
	if err == nil {
		t.Fatal("expected failure")
	}
	if _, err := os.Lstat(snap); !os.IsNotExist(err) {
		t.Fatal("snapshot dir left behind after a failed final write")
	}
}
