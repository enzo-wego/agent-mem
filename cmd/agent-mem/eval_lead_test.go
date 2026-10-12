package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/spf13/cobra"
)

// ---- dry run ---------------------------------------------------------------

func TestEvalDryRunLeadVariant(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "gc")
	for _, id := range []string{"qa", "qb"} {
		writeTestCase(t, dataDir, testCaseSpec{ID: id, Query: "query " + id, Seeds: []string{"seed"}})
	}
	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)
	if err := runRelevance(cmd, relevanceOpts{DryRun: true, DataDir: dataDir, EnvPath: "/nonexistent"}); err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, line := range strings.Split(buf.String(), "\n") {
		trim := strings.TrimSpace(line)
		for _, v := range evalVariants {
			if !strings.HasPrefix(trim, v+":") {
				continue
			}
			counts[v]++
			if v != evalLeadVariant {
				if strings.Contains(line, "X-Graph-Lead") || strings.Contains(line, "include_bodies") {
					t.Errorf("%s leaked lead settings: %s", v, line)
				}
				continue
			}
			for _, want := range []string{"X-Graph-Lead:1", "X-Graph-Caller:eval", `"include_bodies":false`, `"max_items":30`, `"asker_eeid":1`} {
				if !strings.Contains(line, want) {
					t.Errorf("lead line lacks %s: %s", want, line)
				}
			}
			if strings.Contains(line, "X-Graph-Admin") || strings.Contains(line, "X-Asker-User") {
				t.Errorf("lead line carries admin/search headers: %s", line)
			}
		}
	}
	if len(evalVariants) != 4 {
		t.Fatalf("variants = %v", evalVariants)
	}
	for _, v := range evalVariants {
		if counts[v] != 2 {
			t.Errorf("%s printed %d times, want 2 (%v)", v, counts[v], counts)
		}
	}
}

// The YAML anchor makes variant bodies share one map: building the lead body
// must not touch the resolve-asker body.
func TestLeadRequestDoesNotMutateSharedBody(t *testing.T) {
	shared := map[string]any{"query": "q", "seeds": []any{"s1"}, "nested": map[string]any{"k": "v"}}
	c := evalCase{ID: "x", Requests: map[string]evalRequest{
		"resolve-admin": {Body: shared}, "resolve-asker": {Body: shared},
	}}
	_, _, before, _, err := buildEvalRequest(c, "resolve-asker")
	if err != nil {
		t.Fatal(err)
	}
	_, _, leadBody, headers, err := buildEvalRequest(c, evalLeadVariant)
	if err != nil {
		t.Fatal(err)
	}
	_, _, after, _, _ := buildEvalRequest(c, "resolve-asker")
	if string(before) != string(after) || strings.Contains(string(after), "include_bodies") || strings.Contains(string(after), "max_items") {
		t.Fatalf("asker body mutated:\nbefore %s\nafter  %s", before, after)
	}
	if !strings.Contains(string(leadBody), `"include_bodies":false`) || !strings.Contains(string(leadBody), `"max_items":30`) || headers["X-Graph-Lead"] != "1" {
		t.Fatalf("lead request wrong: %s %v", leadBody, headers)
	}
	if _, ok := shared["nested"].(map[string]any)["k"]; !ok || len(shared) != 3 {
		t.Fatalf("shared map changed: %v", shared)
	}
}

// ---- strict lead decoding ---------------------------------------------------

func leadCall(status int, header http.Header, body string) evalCall {
	if header == nil {
		header = http.Header{}
	}
	call := evalCall{Status: status, Header: header, Body: []byte(body)}
	if status < 200 || status >= 300 {
		call.Err = http.ErrNotSupported
	}
	return call
}

func TestParseLeadResponseOutcomes(t *testing.T) {
	good := `{"node_id":"a","hop":0,"links":[{"kind":"PART_OF","confidence":0.95,"from":"a","to":"b"}],"authored_by_bot":"human"}`
	off := http.Header{"X-Graph-Lead": []string{"off"}}
	cases := []struct {
		name   string
		call   evalCall
		want   string
		nItems int
	}{
		{"ok", leadCall(200, nil, `{"artifacts":[`+good+`]}`), outcomeOK, 1},
		{"empty array ok", leadCall(200, nil, `{"artifacts":[]}`), outcomeOK, 0},
		{"links absent ok, confidence absent reads 0", leadCall(200, nil, `{"artifacts":[{"node_id":"a","hop":1,"authored_by_bot":"bot","links":[{"kind":"SAME_TOPIC","from":"a","to":"b"}]}]}`), outcomeOK, 1},
		{"lead off", leadCall(200, off, `{"artifacts":[`+good+`]}`), outcomeLeadOff, 0},
		{"http 500", leadCall(500, nil, `boom`), outcomeHTTPError, 0},
		{"transport", evalCall{Err: http.ErrHandlerTimeout}, outcomeTransportError, 0},
		{"empty object", leadCall(200, nil, `{}`), outcomeDecodeError, 0},
		{"artifacts null", leadCall(200, nil, `{"artifacts":null}`), outcomeDecodeError, 0},
		{"artifacts absent", leadCall(200, nil, `{"results":[]}`), outcomeDecodeError, 0},
		{"not json", leadCall(200, nil, `nope`), outcomeDecodeError, 0},
		{"duplicate id", leadCall(200, nil, `{"artifacts":[`+good+`,`+good+`]}`), outcomeDecodeError, 0},
		{"missing hop", leadCall(200, nil, `{"artifacts":[{"node_id":"a","authored_by_bot":"human"}]}`), outcomeDecodeError, 0},
		{"fractional hop", leadCall(200, nil, `{"artifacts":[{"node_id":"a","hop":1.5,"authored_by_bot":"human"}]}`), outcomeDecodeError, 0},
		{"bad authored_by_bot", leadCall(200, nil, `{"artifacts":[{"node_id":"a","hop":0,"authored_by_bot":"robot"}]}`), outcomeDecodeError, 0},
		{"missing authored_by_bot", leadCall(200, nil, `{"artifacts":[{"node_id":"a","hop":0}]}`), outcomeDecodeError, 0},
		{"link without kind", leadCall(200, nil, `{"artifacts":[{"node_id":"a","hop":0,"authored_by_bot":"human","links":[{"from":"a","to":"b"}]}]}`), outcomeDecodeError, 0},
		{"link confidence string", leadCall(200, nil, `{"artifacts":[{"node_id":"a","hop":0,"authored_by_bot":"human","links":[{"kind":"X","confidence":"hi","from":"a","to":"b"}]}]}`), outcomeDecodeError, 0},
		{"empty node id", leadCall(200, nil, `{"artifacts":[{"node_id":"","hop":0,"authored_by_bot":"human"}]}`), outcomeDecodeError, 0},
	}
	for _, c := range cases {
		got, items := parseLeadResponse(c.call)
		if got != c.want || len(items) != c.nItems {
			t.Errorf("%s: outcome=%s items=%d, want %s/%d", c.name, got, len(items), c.want, c.nItems)
		}
	}
}

func TestEligibilityDigestSeesOneLinkConfidence(t *testing.T) {
	mk := func(conf float64) map[string][]leadItem {
		return map[string][]leadItem{"c": {{ID: "a", Hop: 1, AuthoredByBot: "human", Links: []leadLink{{Kind: "SAME_TOPIC", Confidence: conf, From: "a", To: "b"}}}}}
	}
	a, b := leadEligibilityDigest(mk(0.95)), leadEligibilityDigest(mk(0.50))
	if a == b || a != leadEligibilityDigest(mk(0.95)) {
		t.Fatalf("digest must change with one confidence and be stable: %s %s", a, b)
	}
	// nil and empty links hash alike (canonical form).
	n1 := map[string][]leadItem{"c": {{ID: "a"}}}
	n2 := map[string][]leadItem{"c": {{ID: "a", Links: []leadLink{}}}}
	if leadEligibilityDigest(n1) != leadEligibilityDigest(n2) {
		t.Fatal("nil vs empty links differ")
	}
}

func TestLeadLinkBucket(t *testing.T) {
	cases := []struct {
		links []leadLink
		want  string
	}{
		{nil, "no_link"},
		{[]leadLink{{Kind: "SAME_TOPIC", Confidence: 0.99}, {Kind: "REFERS_TO"}}, "structural"},
		{[]leadLink{{Kind: "PART_OF"}}, "structural"},
		{[]leadLink{{Kind: "THREAD"}, {Kind: "SAME_TOPIC", Confidence: 0.9}}, "same_topic_hi"},
		{[]leadLink{{Kind: "SAME_TOPIC", Confidence: 0.89}}, "same_topic_lo"},
		{[]leadLink{{Kind: "THREAD"}}, "other_link"},
	}
	for _, c := range cases {
		if got := leadLinkBucket(c.links); got != c.want {
			t.Errorf("%v = %s, want %s", c.links, got, c.want)
		}
	}
}

// ---- fixture without a database ---------------------------------------------

type fxItem struct {
	id                               string
	hop                              int
	links                            []leadLink
	bot                              string
	title, summary, author, authorID string
}

type fxCase struct {
	id, query, useCase string
	seeds              []string
	items              []fxItem
}

type leadFixture struct {
	t                *testing.T
	dataDir, snapDir string
	specs            []fxCase
}

// Sentinels planted in query, use case, titles, summaries, author names and ids.
// Every one is of a shape the pseudonymizer must remove.
var leadSentinels = []string{"QSENTQ12345", "qsentmail", "sentinel.example", "SENTUC987654", "SENTTITLE12345", "Zebulon", "Quux", "USENT0001", "SENTSUM98765"}

func defaultFixtureSpecs() []fxCase {
	return []fxCase{
		{
			id: "qa", query: "payment ref QSENTQ12345 failed for qsentmail@sentinel.example", useCase: "UC1 SENTUC987654",
			seeds: []string{"slack:CA:1.000000"},
			items: []fxItem{
				{id: "slack:CA:1.000000", hop: 0, title: "Seed thread SENTTITLE12345", summary: "seed by @Zebulon Quux", author: "Zebulon Quux", authorID: "USENT0001"},
				{id: "slack:CA:2.000000", hop: 1, links: []leadLink{{Kind: "PART_OF", Confidence: 0.95, From: "slack:CA:2.000000", To: "slack:CA:1.000000"}}, title: "ticket from <@USENT0001>", summary: "Zebulon Quux says order ref SENTSUM98765 mail qsentmail@sentinel.example"},
				{id: "jira:PAY-1", hop: 1, links: []leadLink{{Kind: "SAME_TOPIC", Confidence: 0.95, From: "jira:PAY-1", To: "slack:CA:1.000000"}}, bot: "bot", title: "PAY-1 incident", summary: ""},
				{id: "slack:CA:3.000000", hop: 2, bot: "unknown", title: "unrelated", summary: "shares the word timeout"},
			},
		},
		{
			id: "qb", query: "other question", seeds: []string{"slack:CB:1.000000"},
			items: []fxItem{
				{id: "slack:CB:1.000000", hop: 0, title: "seed b", summary: "b"},
				{id: "slack:CB:2.000000", hop: 1, links: []leadLink{{Kind: "THREAD", Confidence: 1, From: "slack:CB:2.000000", To: "slack:CB:1.000000"}}, title: "x1", summary: "x1 summary"},
			},
		},
	}
}

func newLeadFixture(t *testing.T, specs []fxCase) *leadFixture {
	t.Helper()
	f := &leadFixture{t: t, dataDir: filepath.Join(t.TempDir(), "gc"), snapDir: filepath.Join(t.TempDir(), "snap"), specs: specs}
	for _, s := range specs {
		path := writeTestCase(t, f.dataDir, testCaseSpec{ID: s.id, Query: s.query, Seeds: []string{"seed-request"}})
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if raw, err = rewriteCanonicalSeeds(raw, s.seeds); err != nil {
			t.Fatal(err)
		}
		if s.useCase != "" {
			raw = bytes.Replace(raw, []byte("use_case: UC1"), []byte("use_case: "+s.useCase), 1)
		}
		if err := os.WriteFile(path, raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_, sum, err := loadEvalCases(f.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	replay := replayFile{PreparedAt: "2026-10-12T00:00:00Z", BuildRev: "rev-fx", CasesSHA256: sum, Cases: map[string]replayCase{}}
	byCase := map[string][]leadItem{}
	snap := snapshotFile{PreparedAt: replay.PreparedAt, CasesSHA256: sum, Cases: map[string]snapshotCase{}}
	meta := map[string]nodeMeta{}
	for _, s := range specs {
		items := []leadItem{}
		returned := []string{}
		snapItems := map[string]snapshotItem{}
		for _, it := range s.items {
			links := it.links
			if links == nil {
				links = []leadLink{}
			}
			bot := it.bot
			if bot == "" {
				bot = "human"
			}
			items = append(items, leadItem{ID: it.id, Hop: it.hop, Links: links, AuthoredByBot: bot, HasTitle: it.title != "", SummaryRunes: utf8.RuneCountInString(it.summary), LastActivityAt: "2026-10-01T00:00:00Z"})
			returned = append(returned, it.id)
			snapItems[it.id] = snapshotItem{Title: it.title, LeadSummary: it.summary, Author: it.author, AuthorSlackID: it.authorID}
		}
		byCase[s.id] = items
		snap.Cases[s.id] = snapshotCase{Query: s.query, Items: snapItems}
		replay.Cases[s.id] = replayCase{CanonicalSeeds: s.seeds, Variants: map[string]replayVariant{
			"resolve-admin": {Status: 200, Outcome: outcomeOK, Returned: returned},
			evalLeadVariant: {Status: 200, Outcome: outcomeOK, RequestID: "req", Returned: returned, Items: &items},
		}}
		for _, id := range s.seeds {
			meta[id] = nodeMeta{Type: "slack", Exists: true}
		}
	}
	replay.EligibilitySHA256 = leadEligibilityDigest(byCase)
	snap.EligibilitySHA256 = replay.EligibilitySHA256
	snapJSON, _ := json.MarshalIndent(snap, "", "  ")
	sum2 := sha256.Sum256(snapJSON)
	replay.SnapshotSHA256 = hex.EncodeToString(sum2[:])
	if err := os.Mkdir(f.snapDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.snapDir, "snapshot.json"), snapJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	rj, _ := json.MarshalIndent(replay, "", "  ")
	mj, _ := json.Marshal(meta)
	if err := os.WriteFile(filepath.Join(f.dataDir, "replay.json"), rj, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.dataDir, "node_meta.json"), mj, 0o644); err != nil {
		t.Fatal(err)
	}
	return f
}

// mutateReplay edits replay.json as raw JSON without touching any stored digest.
func (f *leadFixture) mutateReplay(fn func(m map[string]any)) {
	f.t.Helper()
	p := filepath.Join(f.dataDir, "replay.json")
	data, err := os.ReadFile(p)
	if err != nil {
		f.t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		f.t.Fatal(err)
	}
	fn(m)
	out, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(p, out, 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func rawLead(m map[string]any, caseID string) map[string]any {
	return m["cases"].(map[string]any)[caseID].(map[string]any)["variants"].(map[string]any)[evalLeadVariant].(map[string]any)
}

func rawItem(m map[string]any, caseID string, i int) map[string]any {
	return rawLead(m, caseID)["items"].([]any)[i].(map[string]any)
}

func (f *leadFixture) nonSeed(caseID string) []string {
	var ids []string
	for _, s := range f.specs {
		if s.id != caseID {
			continue
		}
		for _, it := range s.items {
			if it.hop != 0 {
				ids = append(ids, it.id)
			}
		}
	}
	return ids
}

// writeRows writes a model rows file; rel picks the row for each non-seed item
// and returns ok=false to omit it.
func (f *leadFixture) writeRows(rel func(caseID, nodeID string) (relationRow, bool)) string {
	f.t.Helper()
	rows := map[string]map[string]relationRow{}
	for _, s := range f.specs {
		rows[s.id] = map[string]relationRow{}
		for _, id := range f.nonSeed(s.id) {
			if r, ok := rel(s.id, id); ok {
				rows[s.id][id] = r
			}
		}
	}
	data, _ := json.Marshal(rows)
	p := filepath.Join(f.t.TempDir(), "rows.json")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		f.t.Fatal(err)
	}
	return p
}

func defaultRel(caseID, nodeID string) (relationRow, bool) {
	switch nodeID {
	case "slack:CA:2.000000":
		return relationRow{"same_case", "same_ref"}, true
	case "jira:PAY-1":
		return relationRow{"related_context", "precedent"}, true
	case "slack:CA:3.000000":
		return relationRow{"different_case", "shared_words_only"}, true
	}
	return relationRow{"insufficient", "ambiguous"}, true
}

func newOut(t *testing.T) string { return filepath.Join(t.TempDir(), "o") }

func (f *leadFixture) sheet(out string) (string, error) {
	var buf bytes.Buffer
	err := runLabelSheet(context.Background(), &buf, labelSheetOpts{Variant: evalLeadVariant, All: true, Mask: true, SnapshotDir: f.snapDir, OutDir: out, DataDir: f.dataDir})
	return buf.String(), err
}

func (f *leadFixture) assemble(draft, review, owner string) (outFile, stdout string, err error) {
	var buf bytes.Buffer
	outFile = filepath.Join(f.t.TempDir(), "labels.json")
	err = runRelationAssemble(&buf, relationAssembleOpts{Draft: draft, Review: review, Owner: owner, Out: outFile, DataDir: f.dataDir})
	return outFile, buf.String(), err
}

func (f *leadFixture) check(file string) (string, error) {
	var buf bytes.Buffer
	err := runRelationCheck(&buf, file, f.dataDir)
	return buf.String(), err
}

// validLabels assembles a valid labels file from the default rows.
func (f *leadFixture) validLabels() string {
	f.t.Helper()
	rows := f.writeRows(defaultRel)
	out, _, err := f.assemble(rows, rows, "")
	if err != nil {
		f.t.Fatalf("assemble: %v", err)
	}
	return out
}

func wantCategory(t *testing.T, what string, err error, want string) {
	t.Helper()
	if err == nil || err.Error() != want {
		t.Errorf("%s: err = %v, want %q", what, err, want)
	}
}

// ---- label-sheet resolve-lead -------------------------------------------------

func TestLeadLabelSheetMasksAndShapes(t *testing.T) {
	f := newLeadFixture(t, defaultFixtureSpecs())
	out := newOut(t)
	stdout, err := f.sheet(out)
	if err != nil {
		t.Fatal(err)
	}
	var all strings.Builder
	all.WriteString(stdout)
	entries, err := os.ReadDir(out)
	if err != nil || len(entries) != 3 {
		t.Fatalf("out dir: %v %v", entries, err)
	}
	for _, e := range entries {
		data, _ := os.ReadFile(filepath.Join(out, e.Name()))
		all.Write(data)
		info, _ := e.Info()
		if info.Mode().Perm() != 0o600 {
			t.Errorf("%s mode %v", e.Name(), info.Mode().Perm())
		}
	}
	if info, _ := os.Stat(out); info.Mode().Perm() != 0o700 {
		t.Errorf("dir mode %v", info.Mode().Perm())
	}
	lower := strings.ToLower(all.String())
	for _, s := range leadSentinels {
		if strings.Contains(lower, strings.ToLower(s)) {
			t.Errorf("sentinel %q appears in output", s)
		}
	}
	qa, _ := os.ReadFile(filepath.Join(out, "qa.md"))
	sheet := string(qa)
	if !strings.Contains(sheet, "## Seeds") || !strings.Contains(sheet, "MASKED:") || !strings.Contains(sheet, "Source thread: qa") {
		t.Errorf("sheet header/seeds block missing:\n%s", sheet)
	}
	seedBlock := sheet[strings.Index(sheet, "## Seeds"):strings.Index(sheet, "Candidates:")]
	if strings.Contains(seedBlock, "[r") || strings.Contains(seedBlock, "slack:CA:1.000000") {
		t.Errorf("seeds block must carry no [r<k>] or node id:\n%s", seedBlock)
	}
	if got := strings.Count(sheet, "\n## [r"); got != 3 {
		t.Errorf("rows = %d, want 3:\n%s", got, sheet)
	}
	for _, want := range []string{"## [r1] slack:CA:2.000000", "- bucket: structural", "- bucket: same_topic_hi", "- bucket: no_link", "- authored_by_bot: bot", "- authored_by_bot: unknown", "- hop: 2"} {
		if !strings.Contains(sheet, want) {
			t.Errorf("sheet lacks %q", want)
		}
	}
	// One author, one alias across query, title and summary rows.
	if !strings.Contains(sheet, "person-1") || strings.Contains(sheet, "person-2") {
		t.Errorf("author not one alias:\n%s", sheet)
	}
	var manifest map[string][]map[string]any
	data, _ := os.ReadFile(filepath.Join(out, "manifest.json"))
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest["qa"]) != 3 || len(manifest["qb"]) != 1 {
		t.Fatalf("manifest = %v", manifest)
	}
	for _, row := range manifest["qa"] {
		if len(row) != 2 || row["node_id"] == nil || row["r"] == nil {
			t.Fatalf("manifest row must be ids only: %v", row)
		}
	}
}

func TestLeadLabelSheetCapsAfterMasking(t *testing.T) {
	long := strings.Repeat("x ", 400) + "booking SENTLONG98765 end"
	specs := []fxCase{{id: "qa", query: "q", seeds: []string{"slack:CA:1.000000"}, items: []fxItem{
		{id: "slack:CA:1.000000", hop: 0, title: "s", summary: "s"},
		{id: "slack:CA:2.000000", hop: 1, title: strings.Repeat("t", 300), summary: long},
	}}}
	f := newLeadFixture(t, specs)
	out := newOut(t)
	if _, err := f.sheet(out); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(out, "qa.md"))
	for _, line := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(line, "- title: "); ok && utf8.RuneCountInString(v) > 200 {
			t.Errorf("title not capped: %d runes", utf8.RuneCountInString(v))
		}
		if v, ok := strings.CutPrefix(line, "- summary: "); ok && utf8.RuneCountInString(v) > 600 {
			t.Errorf("summary not capped: %d runes", utf8.RuneCountInString(v))
		}
	}
}

func TestLeadLabelSheetRefusals(t *testing.T) {
	f := newLeadFixture(t, defaultFixtureSpecs())
	ctx := context.Background()
	run := func(o labelSheetOpts) error {
		o.DataDir = f.dataDir
		var buf bytes.Buffer
		return runLabelSheet(ctx, &buf, o)
	}
	base := func() labelSheetOpts {
		return labelSheetOpts{Variant: evalLeadVariant, All: true, Mask: true, SnapshotDir: f.snapDir, OutDir: newOut(t)}
	}
	// flags
	o := base()
	o.Mask = false
	wantCategory(t, "no mask", run(o), "mask_required")
	o = base()
	o.SnapshotDir = ""
	wantCategory(t, "no snapshot", run(o), "snapshot_required")
	o = base()
	o.Variant = "resolve-admin"
	wantCategory(t, "unknown variant", run(o), "variant_invalid")
	o = base()
	o.Variant = "all"
	wantCategory(t, "mask with all", run(o), "flag_invalid")

	// existing out dir, symlinked out, out through a symlink into the repo.
	o = base()
	if err := os.Mkdir(o.OutDir, 0o700); err != nil {
		t.Fatal(err)
	}
	wantCategory(t, "existing out", run(o), "out_exists")
	o = base()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(filepath.Join(t.TempDir(), "nowhere"), link); err != nil {
		t.Fatal(err)
	}
	o.OutDir = link
	wantCategory(t, "symlinked out (dangling)", run(o), "out_exists")
	repo, _ := repoRoot()
	inRepo := filepath.Join(t.TempDir(), "repolink")
	if err := os.Symlink(repo, inRepo); err != nil {
		t.Fatal(err)
	}
	o = base()
	o.OutDir = filepath.Join(inRepo, "sheets-via-symlink")
	wantCategory(t, "out parent symlinked into repo", run(o), "out_inside_repo")
	if _, err := os.Stat(filepath.Join(repo, "sheets-via-symlink")); !os.IsNotExist(err) {
		t.Fatal("refused run must not create anything in the repo")
	}

	// symlinked file path: writePrivateFile refuses anything at the path.
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	fl := filepath.Join(dir, "file")
	if err := os.Symlink(target, fl); err != nil {
		t.Fatal(err)
	}
	wantCategory(t, "symlinked file", writePrivateFile(fl, []byte("x")), "file_exists")
	if b, _ := os.ReadFile(target); string(b) != "keep" {
		t.Fatal("symlink target was written")
	}

	// snapshot digest mismatch.
	snapPath := filepath.Join(f.snapDir, "snapshot.json")
	orig, _ := os.ReadFile(snapPath)
	if err := os.WriteFile(snapPath, append(append([]byte{}, orig...), ' '), 0o600); err != nil {
		t.Fatal(err)
	}
	wantCategory(t, "snapshot digest", run(base()), "snapshot_digest_mismatch")

	// eligibility mismatch and missing row, each with a consistent snapshot digest.
	reseal := func(mutate func(s *snapshotFile)) {
		var s snapshotFile
		if err := json.Unmarshal(orig, &s); err != nil {
			t.Fatal(err)
		}
		mutate(&s)
		data, _ := json.MarshalIndent(s, "", "  ")
		if err := os.WriteFile(snapPath, data, 0o600); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		f.mutateReplay(func(m map[string]any) { m["snapshot_sha256"] = hex.EncodeToString(sum[:]) })
	}
	reseal(func(s *snapshotFile) { s.EligibilitySHA256 = strings.Repeat("0", 64) })
	wantCategory(t, "snapshot eligibility", run(base()), "eligibility_mismatch")
	reseal(func(s *snapshotFile) { delete(s.Cases["qa"].Items, "jira:PAY-1") })
	wantCategory(t, "missing snapshot row", run(base()), "snapshot_missing_row")
	reseal(func(s *snapshotFile) { s.CasesSHA256 = strings.Repeat("1", 64) })
	wantCategory(t, "snapshot cases digest", run(base()), "cases_sha256_mismatch")

	// stale case bytes.
	reseal(func(s *snapshotFile) {})
	if err := run(base()); err != nil {
		t.Fatalf("restored snapshot must pass: %v", err)
	}
	cases, _, _ := loadEvalCases(f.dataDir)
	if err := os.WriteFile(cases[0].path, append(cases[0].raw, []byte("# edit\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	wantCategory(t, "stale case bytes", run(base()), "cases_sha256_mismatch")
}

// ---- validateLeadReplay regressions through all three readers ----------------------

func TestValidateLeadReplayRegressions(t *testing.T) {
	type mutation struct {
		name string
		want string
		fn   func(m map[string]any)
	}
	muts := []mutation{
		{"one link confidence 0.95 -> 0.50", "eligibility_mismatch", func(m map[string]any) {
			rawItem(m, "qa", 1)["links"].([]any)[0].(map[string]any)["confidence"] = 0.50
		}},
		{"one link endpoint", "eligibility_mismatch", func(m map[string]any) {
			rawItem(m, "qa", 1)["links"].([]any)[0].(map[string]any)["to"] = "slack:CA:9.000000"
		}},
		{"one authored_by_bot", "eligibility_mismatch", func(m map[string]any) { rawItem(m, "qa", 1)["authored_by_bot"] = "bot" }},
		{"outcome missing", "lead_outcome", func(m map[string]any) { delete(rawLead(m, "qa"), "outcome") }},
		{"outcome lead_off", "lead_outcome", func(m map[string]any) { rawLead(m, "qa")["outcome"] = "lead_off" }},
		{"outcome http_error", "lead_outcome", func(m map[string]any) { rawLead(m, "qa")["outcome"] = "http_error" }},
		{"outcome decode_error", "lead_outcome", func(m map[string]any) { rawLead(m, "qa")["outcome"] = "decode_error" }},
		{"status 500", "lead_outcome", func(m map[string]any) { rawLead(m, "qa")["status"] = 500 }},
		{"items missing", "lead_items", func(m map[string]any) { delete(rawLead(m, "qa"), "items") }},
		{"item missing hop", "lead_items", func(m map[string]any) { delete(rawItem(m, "qa", 1), "hop") }},
		{"item bad authored_by_bot enum", "lead_items", func(m map[string]any) { rawItem(m, "qa", 1)["authored_by_bot"] = "robot" }},
		{"lead variant missing", "lead_missing", func(m map[string]any) {
			delete(m["cases"].(map[string]any)["qa"].(map[string]any)["variants"].(map[string]any), evalLeadVariant)
		}},
	}
	for _, mu := range muts {
		t.Run(mu.name, func(t *testing.T) {
			f := newLeadFixture(t, defaultFixtureSpecs())
			labels := f.validLabels() // built from the intact replay
			rows := f.writeRows(defaultRel)
			f.mutateReplay(mu.fn)
			_, err := f.sheet(newOut(t))
			wantCategory(t, "label-sheet", err, mu.want)
			_, _, err = f.assemble(rows, rows, "")
			wantCategory(t, "relation-assemble", err, mu.want)
			_, err = f.check(labels)
			wantCategory(t, "relation-check", err, mu.want)
		})
	}

	t.Run("valid empty items passes", func(t *testing.T) {
		specs := defaultFixtureSpecs()
		specs[1] = fxCase{id: "qb", query: "empty", seeds: nil, items: nil}
		f := newLeadFixture(t, specs)
		if _, err := f.sheet(newOut(t)); err != nil {
			t.Fatalf("label-sheet: %v", err)
		}
		labels := f.validLabels()
		var parsed relationFile
		data, _ := os.ReadFile(labels)
		if err := json.Unmarshal(data, &parsed); err != nil || len(parsed.Cases["qb"]) != 0 || parsed.Cases["qb"] == nil {
			t.Fatalf("empty case must be an empty map: %v %v", parsed.Cases["qb"], err)
		}
		if _, err := f.check(labels); err != nil {
			t.Fatalf("relation-check: %v", err)
		}
	})
}

// ---- relation-assemble -------------------------------------------------------------

func TestRelationAssembleMerge(t *testing.T) {
	f := newLeadFixture(t, defaultFixtureSpecs())
	draft := f.writeRows(func(c, n string) (relationRow, bool) {
		switch n {
		case "slack:CA:2.000000":
			return relationRow{"same_case", "same_ref"}, true
		case "jira:PAY-1":
			return relationRow{"different_case", "different_incident"}, true
		case "slack:CA:3.000000":
			return relationRow{"different_case", "shared_words_only"}, true
		}
		return relationRow{}, false // slack:CB:2 missing from the draft
	})
	review := f.writeRows(func(c, n string) (relationRow, bool) {
		switch n {
		case "slack:CA:2.000000":
			return relationRow{"same_case", "work_on_case"}, true // agrees on relation
		case "jira:PAY-1":
			return relationRow{"related_context", "precedent"}, true // differs
		case "slack:CA:3.000000":
			return relationRow{"related_context", "runbook"}, true // differs, owner overrides
		}
		return relationRow{"same_case", "same_ref"}, true
	})
	ownerData, _ := json.Marshal(map[string]map[string]relationRow{"qa": {"slack:CA:3.000000": {"different_case", "different_ref"}}})
	// A model must not be able to set the header: header-named keys are dropped.
	draftData, _ := os.ReadFile(draft)
	var withHeader map[string]any
	_ = json.Unmarshal(draftData, &withHeader)
	withHeader["cases_sha256"] = "bad"
	withHeader["version"] = 99
	withHeader["input_mode"] = "x"
	draftData, _ = json.Marshal(withHeader)
	if err := os.WriteFile(draft, draftData, 0o600); err != nil {
		t.Fatal(err)
	}
	owner := filepath.Join(t.TempDir(), "owner.json")
	if err := os.WriteFile(owner, ownerData, 0o600); err != nil {
		t.Fatal(err)
	}
	out, stdout, err := f.assemble(draft, review, owner)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(out)
	var got relationFile
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(out); info.Mode().Perm() != 0o600 {
		t.Errorf("out mode %v", info.Mode().Perm())
	}
	replayData, _ := os.ReadFile(filepath.Join(f.dataDir, "replay.json"))
	var replay replayFile
	_ = json.Unmarshal(replayData, &replay)
	wantHeader := relationHeader{Version: 1, Adjudication: "r1b-v1", CasesSHA256: replay.CasesSHA256, EligibilitySHA256: replay.EligibilitySHA256, SnapshotSHA256: replay.SnapshotSHA256, BuildRev: "rev-fx", InputMode: "lead:title+lead_summary"}
	if got.relationHeader != wantHeader {
		t.Errorf("header = %+v, want %+v", got.relationHeader, wantHeader)
	}
	want := map[string]map[string]labelRow{
		"qa": {
			"slack:CA:2.000000": {"same_case", "same_ref", "agree"},
			"jira:PAY-1":        {"insufficient", "reviewers_disagree", "disputed"},
			"slack:CA:3.000000": {"different_case", "different_ref", "enzo"},
		},
		"qb": {"slack:CB:2.000000": {"insufficient", "empty_evidence", "missing"}},
	}
	for c, rows := range want {
		for n, w := range rows {
			if got.Cases[c][n] != w {
				t.Errorf("%s %s = %+v, want %+v", c, n, got.Cases[c][n], w)
			}
		}
	}
	if _, err := f.check(out); err != nil {
		t.Fatalf("assembled file must pass relation-check: %v", err)
	}
	if strings.Contains(stdout, "same_ref") || strings.Contains(stdout, "slack:") {
		t.Errorf("stdout carries row content: %s", stdout)
	}

	// refusals
	if _, _, err := f.assemble(draft, review, owner); err != nil {
		t.Fatal(err) // a fresh out path each call
	}
	bad := func(rows map[string]map[string]relationRow) string {
		d, _ := json.Marshal(rows)
		p := filepath.Join(t.TempDir(), "bad.json")
		_ = os.WriteFile(p, d, 0o600)
		return p
	}
	_, _, err = f.assemble(bad(map[string]map[string]relationRow{"nope": {}}), review, "")
	wantCategory(t, "unknown case", err, "unknown_case")
	_, _, err = f.assemble(bad(map[string]map[string]relationRow{"qa": {"slack:CA:1.000000": {"same_case", "same_ref"}}}), review, "")
	wantCategory(t, "seed id in rows", err, "unknown_node")
	_, _, err = f.assemble(bad(map[string]map[string]relationRow{"qa": {"jira:PAY-1": {"maybe", "same_ref"}}}), review, "")
	wantCategory(t, "bad relation", err, "enum_relation")
	_, _, err = f.assemble(bad(map[string]map[string]relationRow{"qa": {"jira:PAY-1": {"same_case", "because"}}}), review, "")
	wantCategory(t, "bad reason", err, "enum_reason")
	dup := filepath.Join(t.TempDir(), "dup.json")
	_ = os.WriteFile(dup, []byte(`{"qa":{"jira:PAY-1":{"relation":"same_case","reason":"same_ref"},"jira:PAY-1":{"relation":"same_case","reason":"same_ref"}}}`), 0o600)
	_, _, err = f.assemble(dup, review, "")
	wantCategory(t, "duplicate key", err, "duplicate_key")
	// existing out is never overwritten.
	var buf bytes.Buffer
	err = runRelationAssemble(&buf, relationAssembleOpts{Draft: draft, Review: review, Out: out, DataDir: f.dataDir})
	wantCategory(t, "existing out", err, "file_exists")
}

// ---- relation-check ------------------------------------------------------------------

func TestRelationCheck(t *testing.T) {
	f := newLeadFixture(t, defaultFixtureSpecs())
	valid := f.validLabels()
	raw, _ := os.ReadFile(valid)

	stdout, err := f.check(valid)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"relation labels ok: cases=2 items=4",
		"case qa same_case=1 related_context=1 different_case=1 insufficient=0",
		"relation_by_link_bucket different_case no_link 1",
		"relation_by_link_bucket same_case structural 1",
		"relation_by_author_bucket related_context bot 1",
		"cases_with_different_case 1",
		"insufficient_reason ambiguous 1",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
	for _, id := range []string{"slack:", "jira:", "PAY-1"} {
		if strings.Contains(stdout, id) {
			t.Errorf("stdout must be counts only, found %q", id)
		}
	}

	var doc map[string]any
	mutate := func(fn func(d map[string]any)) string {
		doc = nil
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		fn(doc)
		data, _ := json.MarshalIndent(doc, "", "  ")
		p := filepath.Join(t.TempDir(), "labels.json")
		_ = os.WriteFile(p, data, 0o600)
		return p
	}
	rows := func(d map[string]any, c string) map[string]any {
		return d["cases"].(map[string]any)[c].(map[string]any)
	}

	cases := []struct {
		name, want string
		file       func() string
	}{
		{"empty cases", "case_set", func() string { return mutate(func(d map[string]any) { d["cases"] = map[string]any{} }) }},
		{"omitted case", "case_set", func() string { return mutate(func(d map[string]any) { delete(d["cases"].(map[string]any), "qb") }) }},
		{"omitted item", "item_set", func() string { return mutate(func(d map[string]any) { delete(rows(d, "qa"), "jira:PAY-1") }) }},
		{"extra item", "item_set", func() string {
			return mutate(func(d map[string]any) {
				rows(d, "qa")["slack:CA:9.000000"] = map[string]any{"relation": "same_case", "reason": "same_ref", "check": "agree"}
			})
		}},
		{"seed item", "item_set", func() string {
			return mutate(func(d map[string]any) {
				rows(d, "qa")["slack:CA:1.000000"] = map[string]any{"relation": "same_case", "reason": "same_ref", "check": "agree"}
				delete(rows(d, "qa"), "jira:PAY-1")
			})
		}},
		{"disputed with different_case", "check_relation", func() string {
			return mutate(func(d map[string]any) {
				rows(d, "qa")["slack:CA:3.000000"] = map[string]any{"relation": "different_case", "reason": "reviewers_disagree", "check": "disputed"}
			})
		}},
		{"missing with same_case", "check_relation", func() string {
			return mutate(func(d map[string]any) {
				rows(d, "qa")["slack:CA:2.000000"] = map[string]any{"relation": "same_case", "reason": "empty_evidence", "check": "missing"}
			})
		}},
		{"wrong input_mode", "input_mode", func() string { return mutate(func(d map[string]any) { d["input_mode"] = "lead:body" }) }},
		{"wrong version", "version", func() string { return mutate(func(d map[string]any) { d["version"] = 2 }) }},
		{"eligibility mismatch", "eligibility_mismatch", func() string {
			return mutate(func(d map[string]any) { d["eligibility_sha256"] = strings.Repeat("a", 64) })
		}},
		{"snapshot digest mismatch", "snapshot_digest_mismatch", func() string {
			return mutate(func(d map[string]any) { d["snapshot_sha256"] = strings.Repeat("a", 64) })
		}},
		{"cases digest mismatch", "cases_sha256_mismatch", func() string {
			return mutate(func(d map[string]any) { d["cases_sha256"] = strings.Repeat("a", 64) })
		}},
		{"missing header field", "header_fields", func() string { return mutate(func(d map[string]any) { delete(d, "build_rev") }) }},
		{"extra header field", "header_fields", func() string { return mutate(func(d map[string]any) { d["extra"] = 1 }) }},
		{"bad relation enum", "enum_relation", func() string {
			return mutate(func(d map[string]any) { rows(d, "qa")["jira:PAY-1"].(map[string]any)["relation"] = "maybe" })
		}},
		{"bad reason enum", "enum_reason", func() string {
			return mutate(func(d map[string]any) { rows(d, "qa")["jira:PAY-1"].(map[string]any)["reason"] = "because" })
		}},
		{"bad check enum", "enum_check", func() string {
			return mutate(func(d map[string]any) { rows(d, "qa")["jira:PAY-1"].(map[string]any)["check"] = "maybe" })
		}},
		{"row with extra field", "row_shape", func() string {
			return mutate(func(d map[string]any) { rows(d, "qa")["jira:PAY-1"].(map[string]any)["note"] = "x" })
		}},
		{"duplicate JSON key", "duplicate_key", func() string {
			p := filepath.Join(t.TempDir(), "dup.json")
			dup := strings.Replace(string(raw), `"version": 1,`, `"version": 1, "version": 1,`, 1)
			_ = os.WriteFile(p, []byte(dup), 0o600)
			return p
		}},
		{"invalid json", "invalid_json", func() string {
			p := filepath.Join(t.TempDir(), "bad.json")
			_ = os.WriteFile(p, []byte(`{`), 0o600)
			return p
		}},
	}
	for _, c := range cases {
		_, err := f.check(c.file())
		wantCategory(t, c.name, err, c.want)
	}

	// explicit insufficient rows pass and are counted.
	ins := mutate(func(d map[string]any) {
		rows(d, "qa")["jira:PAY-1"] = map[string]any{"relation": "insufficient", "reason": "empty_evidence", "check": "agree"}
	})
	out, err := f.check(ins)
	if err != nil || !strings.Contains(out, "insufficient_reason empty_evidence 1") || !strings.Contains(out, "insufficient_reason ambiguous 1") {
		t.Errorf("insufficient rows: %v\n%s", err, out)
	}

	// stale case bytes: header digest no longer matches the case files.
	cs, _, _ := loadEvalCases(f.dataDir)
	if err := os.WriteFile(cs[0].path, append(cs[0].raw, []byte("# edit\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = f.check(valid)
	wantCategory(t, "stale case bytes", err, "cases_sha256_mismatch")
}

func TestRelationCheckSeedMismatch(t *testing.T) {
	f := newLeadFixture(t, defaultFixtureSpecs())
	labels := f.validLabels()
	// a canonical seed that is not a hop-0 item and whose thread_root is not one either.
	f.mutateReplay(func(m map[string]any) {
		m["cases"].(map[string]any)["qa"].(map[string]any)["canonical_seeds"] = []any{"slack:CA:9.000000"}
	})
	_, err := f.check(labels)
	wantCategory(t, "seed not a hop-0 item", err, "seed_mismatch")

	// the seed's thread_root (per node_meta.json) being a hop-0 item is enough.
	metaPath := filepath.Join(f.dataDir, "node_meta.json")
	mj, _ := json.Marshal(map[string]nodeMeta{"slack:CA:9.000000": {Type: "slack", Exists: true, IsReply: true, ThreadRoot: "slack:CA:1.000000"}, "slack:CB:1.000000": {Type: "slack", Exists: true}})
	if err := os.WriteFile(metaPath, mj, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := f.check(labels); err != nil {
		t.Fatalf("thread_root hop-0 must satisfy the seed check: %v", err)
	}
}

// ---- no content on any channel --------------------------------------------------------

func TestLeadCommandsCarryNoSentinels(t *testing.T) {
	f := newLeadFixture(t, defaultFixtureSpecs())
	var chans []string
	out := newOut(t)
	so, err := f.sheet(out)
	chans = append(chans, so)
	if err != nil {
		chans = append(chans, err.Error())
	}
	_, err = f.sheet(out) // refused: out exists
	if err != nil {
		chans = append(chans, err.Error())
	}
	rows := f.writeRows(defaultRel)
	file, ao, err := f.assemble(rows, rows, "")
	chans = append(chans, ao)
	if err != nil {
		chans = append(chans, err.Error())
	}
	co, err := f.check(file)
	chans = append(chans, co)
	if err != nil {
		chans = append(chans, err.Error())
	}
	_, err = f.check(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		chans = append(chans, err.Error())
	}
	all := strings.ToLower(strings.Join(chans, "\n"))
	for _, s := range leadSentinels {
		if strings.Contains(all, strings.ToLower(s)) {
			t.Errorf("sentinel %q on stdout/stderr", s)
		}
	}
}

// ---- relevance ---------------------------------------------------------------------------

type leadRelevanceJSON struct {
	PerCase []struct {
		CaseID          string  `json:"case_id"`
		Variant         string  `json:"variant"`
		Roots           *int    `json:"roots"`
		SeedGroups      *int    `json:"seed_groups"`
		UnreachableMust *int    `json:"unreachable_must"`
		Recall          float64 `json:"recall"`
	} `json:"per_case"`
	Aggregate map[string]variantAgg `json:"aggregate"`
}

// leadRelevanceData is one labelled case whose must set contains a reply.
func leadRelevanceData(t *testing.T) string {
	t.Helper()
	dataDir := filepath.Join(t.TempDir(), "gc")
	p := writeTestCase(t, dataDir, testCaseSpec{ID: "ok", Query: "ok", Seeds: []string{"seed"}, LabelledBy: "fable-draft", Must: []string{"must1", "reply1"}, May: []string{"may1"}, MustNot: []string{"bad1"}})
	raw, _ := os.ReadFile(p)
	raw, err := rewriteCanonicalSeeds(raw, []string{"seed"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	meta := map[string]nodeMeta{
		"seed": {Type: "slack", Exists: true}, "must1": {Type: "slack", Exists: true}, "may1": {Type: "slack", Exists: true},
		"bad1": {Type: "jira", Exists: true}, "reply1": {Type: "slack", Exists: true, IsReply: true, ThreadRoot: "slack:C:1.0"},
	}
	data, _ := json.Marshal(meta)
	if err := os.WriteFile(filepath.Join(dataDir, "node_meta.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	return dataDir
}

func TestRelevanceScoresLeadVariant(t *testing.T) {
	dataDir := leadRelevanceData(t)
	t.Setenv("AGENT_MEM_API_KEY", "")
	rs := &relevanceServer{calls: map[string]int{}}
	// The lead warm-up (first lead request) fails; measured runs succeed.
	var mu sync.Mutex
	leadCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Graph-Lead") == "1" {
			mu.Lock()
			leadCalls++
			n := leadCalls
			mu.Unlock()
			if n == 1 {
				http.Error(w, "boom", 500)
				return
			}
		}
		rs.handler(w, r)
	}))
	defer server.Close()
	jsonPath := filepath.Join(t.TempDir(), "r.json")
	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)
	if err := runRelevance(cmd, relevanceOpts{BaseURL: server.URL, JSONPath: jsonPath, DataDir: dataDir, EnvPath: "/nonexistent", Quiet: true}); err != nil {
		t.Fatalf("%v\n%s", err, buf.String())
	}
	var res leadRelevanceJSON
	data, _ := os.ReadFile(jsonPath)
	if err := json.Unmarshal(data, &res); err != nil {
		t.Fatal(err)
	}
	lead := res.Aggregate[evalLeadVariant]
	if lead.WarmupOK != 0 || lead.WarmupTotal != 1 || lead.RunsOK != 10 || lead.RunsTotal != 10 {
		t.Errorf("lead warmup/runs = %+v", lead)
	}
	asker := res.Aggregate["resolve-asker"]
	if asker.WarmupOK != 1 || asker.WarmupTotal != 1 || asker.RunsOK != 10 || asker.RunsTotal != 10 {
		t.Errorf("asker warmup/runs = %+v", asker)
	}
	if !strings.Contains(buf.String(), "warmup_ok=0/1 runs_ok=10/10") || !strings.Contains(buf.String(), "warmup_ok=1/1 runs_ok=10/10") {
		t.Errorf("--quiet counters missing:\n%s", buf.String())
	}
	var sawLead, sawAsker bool
	for _, pc := range res.PerCase {
		switch pc.Variant {
		case evalLeadVariant:
			sawLead = true
			// ids: seed, must1, may1, bad1, reply1, unk1 -> 6 roots, 1 seed group, reply-only must unreachable.
			if pc.Roots == nil || *pc.Roots != 6 || pc.SeedGroups == nil || *pc.SeedGroups != 1 || pc.UnreachableMust == nil || *pc.UnreachableMust != 1 {
				t.Errorf("lead per-case: %+v", pc)
			}
			if pc.Recall != 1 { // must1 hit; reply1 excluded from the denominator
				t.Errorf("lead recall = %v, want 1", pc.Recall)
			}
		case "resolve-asker":
			sawAsker = true
			if pc.Roots != nil || pc.UnreachableMust != nil {
				t.Errorf("old variant carries lead fields: %+v", pc)
			}
			if pc.Recall != 1 { // must1 and reply1 both returned by the fake
				t.Errorf("asker recall = %v", pc.Recall)
			}
		}
	}
	if !sawLead || !sawAsker {
		t.Fatal("variants missing from per_case")
	}
}

// Old variants' output must match a golden recorded before resolve-lead existed.
func TestRelevanceOldVariantsMatchGolden(t *testing.T) {
	dataDir := relevanceFixture(t)
	t.Setenv("AGENT_MEM_API_KEY", "")
	rs := &relevanceServer{calls: map[string]int{}}
	server := httptest.NewServer(http.HandlerFunc(rs.handler))
	defer server.Close()
	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)
	if err := runRelevance(cmd, relevanceOpts{BaseURL: server.URL, DataDir: dataDir, EnvPath: "/nonexistent"}); err != nil {
		t.Fatal(err)
	}
	latency := regexp.MustCompile(`latency:.*`)
	norm := func(s string) string { return strings.TrimRight(latency.ReplaceAllString(s, ""), "\n") }
	lines := strings.Split(buf.String(), "\n")
	var kept []string
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		switch {
		case strings.HasPrefix(l, "  "+evalLeadVariant+":"):
			i++ // its result line
		case l == evalLeadVariant:
			for i+1 < len(lines) && lines[i+1] != "" {
				i++
			}
		default:
			kept = append(kept, l)
		}
	}
	golden, err := os.ReadFile("testdata/relevance_golden.txt")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := norm(strings.Join(kept, "\n")), norm(string(golden)); got != want {
		t.Fatalf("old variants' output changed:\n--- got\n%s\n--- want\n%s", got, want)
	}
}

// ---- prepare with a fake server (database) ------------------------------------------------

func TestPrepareLeadOutcomes(t *testing.T) {
	pool, _ := evalTestDB(t)
	seedEvalNodes(t, pool, "CLEAD")
	root := "slack:CLEAD:1.000000"
	goodItem := func(id string, hop int) string {
		return `{"node_id":"` + id + `","hop":` + string(rune('0'+hop)) + `,"links":[],"authored_by_bot":"human","title":"SENTTITLE12345 Zebulon Quux","lead_summary":"SENTSUM98765","author":"Zebulon Quux","author_slack_id":"USENT0001"}`
	}
	modes := []struct {
		name, want string
		serve      func(w http.ResponseWriter)
	}{
		{"lead off", "prepare: lead_off qa", func(w http.ResponseWriter) {
			w.Header().Set("X-Graph-Lead", "off")
			_, _ = w.Write([]byte(`{"artifacts":[` + goodItem(root, 0) + `]}`))
		}},
		{"http 500", "prepare: lead_http_error qa", func(w http.ResponseWriter) { http.Error(w, "boom", 500) }},
		{"empty object", "prepare: lead_decode_error qa", func(w http.ResponseWriter) { _, _ = w.Write([]byte(`{}`)) }},
		{"artifacts absent", "prepare: lead_decode_error qa", func(w http.ResponseWriter) { _, _ = w.Write([]byte(`{"results":[]}`)) }},
		{"duplicate id", "prepare: lead_decode_error qa", func(w http.ResponseWriter) {
			_, _ = w.Write([]byte(`{"artifacts":[` + goodItem(root, 0) + `,` + goodItem(root, 1) + `]}`))
		}},
		{"item missing hop", "prepare: lead_decode_error qa", func(w http.ResponseWriter) {
			_, _ = w.Write([]byte(`{"artifacts":[{"node_id":"` + root + `","authored_by_bot":"human"}]}`))
		}},
		{"bad authored_by_bot", "prepare: lead_decode_error qa", func(w http.ResponseWriter) {
			_, _ = w.Write([]byte(`{"artifacts":[{"node_id":"` + root + `","hop":0,"authored_by_bot":"robot"}]}`))
		}},
	}
	for _, m := range modes {
		t.Run(m.name, func(t *testing.T) {
			ps := &prepareServer{byQ: map[string][]string{"qa": {root}}, fail: map[string]bool{}}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-Graph-Lead") == "1" {
					m.serve(w)
					return
				}
				ps.handler(w, r)
			}))
			defer server.Close()
			dataDir := filepath.Join(t.TempDir(), "gc")
			writeTestCase(t, dataDir, testCaseSpec{ID: "qa", Query: "qa", Seeds: []string{root}})
			snap := filepath.Join(t.TempDir(), "snap")
			before := snapshotDir(t, dataDir)
			t.Setenv("AGENT_MEM_API_KEY", "")
			var buf bytes.Buffer
			err := runEvalPrepare(context.Background(), &buf, evalPrepareOpts{Snapshot: snap, BaseURL: server.URL, DataDir: dataDir, EnvPath: "/nonexistent", Pool: pool})
			if err == nil || err.Error() != m.want {
				t.Fatalf("err = %v, want %q", err, m.want)
			}
			if !sameSnapshot(before, snapshotDir(t, dataDir)) {
				t.Fatal("files written despite the failure")
			}
			if _, err := os.Lstat(snap); !os.IsNotExist(err) {
				t.Fatal("snapshot dir left behind")
			}
			for _, s := range leadSentinels {
				if strings.Contains(strings.ToLower(buf.String()+err.Error()), strings.ToLower(s)) {
					t.Errorf("sentinel %q on output", s)
				}
			}
		})
	}

	t.Run("valid empty artifacts, legacy replay", func(t *testing.T) {
		ps := &prepareServer{byQ: map[string][]string{"qa": {root, "slack:CLEAD:3.000000"}}, fail: map[string]bool{}}
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-Graph-Lead") == "1" {
				_, _ = w.Write([]byte(`{"artifacts":[]}`))
				return
			}
			ps.handler(w, r)
		}))
		defer server.Close()
		dataDir := filepath.Join(t.TempDir(), "gc")
		writeTestCase(t, dataDir, testCaseSpec{ID: "qa", Query: "qa", Seeds: []string{root}})
		snap := filepath.Join(t.TempDir(), "snap")
		t.Setenv("AGENT_MEM_API_KEY", "")
		var buf bytes.Buffer
		if err := runEvalPrepare(context.Background(), &buf, evalPrepareOpts{Snapshot: snap, BaseURL: server.URL, DataDir: dataDir, EnvPath: "/nonexistent", Pool: pool}); err != nil {
			t.Fatalf("%v\n%s", err, buf.String())
		}
		if info, err := os.Stat(snap); err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("snapshot dir: %v %v", info, err)
		}
		if info, err := os.Stat(filepath.Join(snap, "snapshot.json")); err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("snapshot file: %v %v", info, err)
		}
		data, _ := os.ReadFile(filepath.Join(dataDir, "replay.json"))
		var replay replayFile
		if err := json.Unmarshal(data, &replay); err != nil {
			t.Fatal(err)
		}
		v := replay.Cases["qa"].Variants[evalLeadVariant]
		if v.Outcome != outcomeOK || v.Items == nil || len(*v.Items) != 0 || !strings.Contains(string(data), `"items": []`) {
			t.Fatalf("empty lead variant wrong: %+v", v)
		}
		sum := sha256.Sum256(mustRead(t, filepath.Join(snap, "snapshot.json")))
		if replay.SnapshotSHA256 != hex.EncodeToString(sum[:]) || replay.EligibilitySHA256 == "" {
			t.Fatalf("digests not recorded: %+v", replay)
		}
		// An existing snapshot dir is refused before any request.
		err := runEvalPrepare(context.Background(), &buf, evalPrepareOpts{Snapshot: snap, BaseURL: server.URL, DataDir: dataDir, EnvPath: "/nonexistent", Pool: pool})
		if err == nil || !strings.Contains(err.Error(), "out_exists") {
			t.Fatalf("existing snapshot dir: %v", err)
		}

		// Legacy replay: no outcome, no items, no lead variant, no digests.
		var m map[string]any
		_ = json.Unmarshal(data, &m)
		delete(m, "eligibility_sha256")
		delete(m, "snapshot_sha256")
		for _, c := range m["cases"].(map[string]any) {
			variants := c.(map[string]any)["variants"].(map[string]any)
			delete(variants, evalLeadVariant)
			for _, vv := range variants {
				delete(vv.(map[string]any), "outcome")
			}
		}
		legacy, _ := json.Marshal(m)
		if err := os.WriteFile(filepath.Join(dataDir, "replay.json"), legacy, 0o644); err != nil {
			t.Fatal(err)
		}
		outAll := newOut(t)
		if err := runLabelSheet(context.Background(), &buf, labelSheetOpts{All: true, OutDir: outAll, DataDir: dataDir, Pool: pool}); err != nil {
			t.Fatalf("legacy replay with --variant all: %v", err)
		}
		sheet, _ := os.ReadFile(filepath.Join(outAll, "qa.md"))
		if !strings.Contains(string(sheet), "## [r1] ") {
			t.Fatalf("all-variant heading not [r<k>]:\n%s", sheet)
		}
		err = runLabelSheet(context.Background(), &buf, labelSheetOpts{Variant: evalLeadVariant, All: true, Mask: true, SnapshotDir: snap, OutDir: newOut(t), DataDir: dataDir})
		wantCategory(t, "legacy replay with resolve-lead", err, "lead_missing")
	})

	t.Run("sentinels stay off stdout on success", func(t *testing.T) {
		ps := &prepareServer{byQ: map[string][]string{"qa": {root}}, fail: map[string]bool{}}
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-Graph-Lead") == "1" {
				_, _ = w.Write([]byte(`{"artifacts":[` + goodItem(root, 0) + `,` + goodItem("slack:CLEAD:3.000000", 1) + `]}`))
				return
			}
			ps.handler(w, r)
		}))
		defer server.Close()
		dataDir := filepath.Join(t.TempDir(), "gc")
		writeTestCase(t, dataDir, testCaseSpec{ID: "qa", Query: "qa", Seeds: []string{root}})
		snap := filepath.Join(t.TempDir(), "snap")
		t.Setenv("AGENT_MEM_API_KEY", "")
		var buf bytes.Buffer
		if err := runEvalPrepare(context.Background(), &buf, evalPrepareOpts{Snapshot: snap, BaseURL: server.URL, DataDir: dataDir, EnvPath: "/nonexistent", Pool: pool}); err != nil {
			t.Fatal(err)
		}
		for _, s := range leadSentinels {
			if strings.Contains(strings.ToLower(buf.String()), strings.ToLower(s)) {
				t.Errorf("sentinel %q on stdout", s)
			}
		}
		// The text is in the private snapshot, never in the tracked replay.
		replay := mustRead(t, filepath.Join(dataDir, "replay.json"))
		snapData := mustRead(t, filepath.Join(snap, "snapshot.json"))
		if strings.Contains(string(replay), "SENTTITLE12345") || !strings.Contains(string(snapData), "SENTTITLE12345") {
			t.Error("lead text must live only in the snapshot")
		}
	})
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
