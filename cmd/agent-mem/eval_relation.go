package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

// Ground truth for the wrong-item detectors: for every non-seed lead root of
// every case, its relation to the case. Models write rows only; relation-assemble
// computes the header and the merge, relation-check validates the result. Every
// failure is a fixed category on stderr, never content.

const (
	relationVersion   = 1
	relationAdjudic   = "r1b-v1"
	relationInputMode = "lead:title+lead_summary"
)

var relationEnum = map[string]bool{"same_case": true, "related_context": true, "different_case": true, "insufficient": true}

var reasonEnum = map[string]bool{
	"same_ref": true, "same_ticket_or_pr": true, "work_on_case": true, "precedent": true,
	"same_component": true, "runbook": true, "different_ref": true, "different_incident": true,
	"shared_words_only": true, "empty_evidence": true, "ambiguous": true, "reviewers_disagree": true,
}

var checkEnum = map[string]bool{"agree": true, "disputed": true, "missing": true, "enzo": true}

var relationOrder = []string{"same_case", "related_context", "different_case", "insufficient"}

// relationRow is what a model writes: {case_id: {node_id: {relation, reason}}}.
type relationRow struct {
	Relation string `json:"relation"`
	Reason   string `json:"reason"`
}

// labelRow is a row of relation_labels.json.
type labelRow struct {
	Relation string `json:"relation"`
	Reason   string `json:"reason"`
	Check    string `json:"check"`
}

type relationHeader struct {
	Version           int    `json:"version"`
	Adjudication      string `json:"adjudication"`
	CasesSHA256       string `json:"cases_sha256"`
	EligibilitySHA256 string `json:"eligibility_sha256"`
	SnapshotSHA256    string `json:"snapshot_sha256"`
	BuildRev          string `json:"build_rev"`
	InputMode         string `json:"input_mode"`
}

type relationFile struct {
	relationHeader
	Cases map[string]map[string]labelRow `json:"cases"`
}

var relationHeaderKeys = []string{"version", "adjudication", "cases_sha256", "eligibility_sha256", "snapshot_sha256", "build_rev", "input_mode", "cases"}

// rejectDuplicateKeys fails on invalid JSON or an object with a repeated key.
func rejectDuplicateKeys(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := walkNoDup(dec); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return categoryErr("invalid_json")
	}
	return nil
}

func walkNoDup(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return categoryErr("invalid_json")
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch d {
	case '{':
		seen := map[string]bool{}
		for dec.More() {
			kt, err := dec.Token()
			key, isStr := kt.(string)
			if err != nil || !isStr {
				return categoryErr("invalid_json")
			}
			if seen[key] {
				return categoryErr("duplicate_key")
			}
			seen[key] = true
			if err := walkNoDup(dec); err != nil {
				return err
			}
		}
	case '[':
		for dec.More() {
			if err := walkNoDup(dec); err != nil {
				return err
			}
		}
	}
	if _, err := dec.Token(); err != nil {
		return categoryErr("invalid_json")
	}
	return nil
}

// strictDecode checks duplicate keys, then decodes with unknown fields rejected.
func strictDecode(data []byte, dst any) error {
	if err := rejectDuplicateKeys(data); err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return categoryErr("row_shape")
	}
	return nil
}

// relationContext is the validated, shared input of both relation commands.
type relationContext struct {
	replay  replayFile
	cases   []loadedCase
	sum     string
	lead    map[string][]leadItem
	nonSeed map[string]map[string]bool // case id -> non-seed node id set
}

// loadRelationContext reads replay.json, the case files and node_meta.json, runs
// validateLeadReplay first, then the seed consistency check.
func loadRelationContext(dataDir string) (*relationContext, error) {
	data, err := os.ReadFile(filepath.Join(dataDir, "replay.json"))
	if err != nil {
		return nil, categoryErr("replay_read")
	}
	var rc relationContext
	if json.Unmarshal(data, &rc.replay) != nil {
		return nil, categoryErr("replay_read")
	}
	rc.cases, rc.sum, err = loadEvalCases(dataDir)
	if err != nil {
		return nil, categoryErr("cases_read")
	}
	if rc.lead, err = validateLeadReplay(rc.replay, rc.cases); err != nil {
		return nil, err
	}
	metaData, err := os.ReadFile(filepath.Join(dataDir, "node_meta.json"))
	if err != nil {
		return nil, categoryErr("meta_read")
	}
	var meta map[string]nodeMeta
	if json.Unmarshal(metaData, &meta) != nil {
		return nil, categoryErr("meta_read")
	}
	rc.nonSeed = map[string]map[string]bool{}
	for _, c := range rc.cases {
		hop0 := map[string]bool{}
		set := map[string]bool{}
		for _, it := range rc.lead[c.ID] {
			if it.Hop == 0 {
				hop0[it.ID] = true
			} else {
				set[it.ID] = true
			}
		}
		for _, s := range rc.replay.Cases[c.ID].CanonicalSeeds {
			if !hop0[s] && !hop0[meta[s].ThreadRoot] {
				return nil, categoryErr("seed_mismatch")
			}
		}
		rc.nonSeed[c.ID] = set
	}
	return &rc, nil
}

type relationAssembleOpts struct {
	Draft, Review, Owner, Out string
	DataDir                   string
}

// readRows reads a model's rows file. The header is never taken from model
// output: any top-level key that names a header field is dropped before the
// strict decode.
func readRows(path string) (map[string]map[string]relationRow, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, categoryErr("rows_read")
	}
	if err := rejectDuplicateKeys(data); err != nil {
		return nil, err
	}
	var top map[string]json.RawMessage
	if json.Unmarshal(data, &top) != nil || top == nil {
		return nil, categoryErr("row_shape")
	}
	for _, k := range relationHeaderKeys {
		delete(top, k)
	}
	data, _ = json.Marshal(top)
	var rows map[string]map[string]relationRow
	if err := strictDecode(data, &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

func validRow(r relationRow) error {
	if !relationEnum[r.Relation] {
		return categoryErr("enum_relation")
	}
	if !reasonEnum[r.Reason] {
		return categoryErr("enum_reason")
	}
	return nil
}

// checkRows validates enums and that every case and node id exists (non-seed).
func (rc *relationContext) checkRows(rows map[string]map[string]relationRow) error {
	for caseID, nodes := range rows {
		set, ok := rc.nonSeed[caseID]
		if !ok {
			return categoryErr("unknown_case")
		}
		for id, r := range nodes {
			if !set[id] {
				return categoryErr("unknown_node")
			}
			if err := validRow(r); err != nil {
				return err
			}
		}
	}
	return nil
}

func runRelationAssemble(out io.Writer, o relationAssembleOpts) error {
	if o.Draft == "" || o.Review == "" || o.Out == "" {
		return categoryErr("args_required")
	}
	rc, err := loadRelationContext(o.DataDir)
	if err != nil {
		return err
	}
	if rc.replay.CasesSHA256 != rc.sum {
		return categoryErr("cases_sha256_mismatch")
	}
	draft, err := readRows(o.Draft)
	if err != nil {
		return err
	}
	review, err := readRows(o.Review)
	if err != nil {
		return err
	}
	var owner map[string]map[string]relationRow
	if o.Owner != "" {
		if owner, err = readRows(o.Owner); err != nil {
			return err
		}
	}
	for _, rows := range []map[string]map[string]relationRow{draft, review, owner} {
		if err := rc.checkRows(rows); err != nil {
			return err
		}
	}
	res := relationFile{
		relationHeader: relationHeader{
			Version: relationVersion, Adjudication: relationAdjudic,
			CasesSHA256: rc.sum, EligibilitySHA256: rc.replay.EligibilitySHA256,
			SnapshotSHA256: rc.replay.SnapshotSHA256, BuildRev: rc.replay.BuildRev, InputMode: relationInputMode,
		},
		Cases: map[string]map[string]labelRow{},
	}
	for _, c := range rc.cases {
		merged := map[string]labelRow{}
		for id := range rc.nonSeed[c.ID] {
			d, dok := draft[c.ID][id]
			r, rok := review[c.ID][id]
			var row labelRow
			switch {
			case !dok || !rok:
				row = labelRow{Relation: "insufficient", Reason: "empty_evidence", Check: "missing"}
			case d.Relation == r.Relation:
				row = labelRow{Relation: d.Relation, Reason: d.Reason, Check: "agree"}
			default:
				row = labelRow{Relation: "insufficient", Reason: "reviewers_disagree", Check: "disputed"}
			}
			if o, ok := owner[c.ID][id]; ok {
				row = labelRow{Relation: o.Relation, Reason: o.Reason, Check: "enzo"}
			}
			merged[id] = row
		}
		res.Cases[c.ID] = merged
	}
	enc, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return err
	}
	if err := writePrivateFile(o.Out, append(enc, '\n')); err != nil {
		return err
	}
	fmt.Fprintf(out, "wrote relation labels for %d cases\n", len(res.Cases))
	return nil
}

func runRelationCheck(out io.Writer, path, dataDir string) error {
	rc, err := loadRelationContext(dataDir)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return categoryErr("labels_read")
	}
	if err := rejectDuplicateKeys(data); err != nil {
		return err
	}
	var top map[string]json.RawMessage
	if json.Unmarshal(data, &top) != nil || top == nil {
		return categoryErr("invalid_json")
	}
	if len(top) != len(relationHeaderKeys) {
		return categoryErr("header_fields")
	}
	for _, k := range relationHeaderKeys {
		if _, ok := top[k]; !ok {
			return categoryErr("header_fields")
		}
	}
	var f relationFile
	if err := strictDecode(data, &f); err != nil {
		return err
	}
	switch {
	case f.Version != relationVersion:
		return categoryErr("version")
	case f.InputMode != relationInputMode:
		return categoryErr("input_mode")
	case f.EligibilitySHA256 != rc.replay.EligibilitySHA256:
		return categoryErr(catEligibilityMismatch)
	case f.SnapshotSHA256 != rc.replay.SnapshotSHA256:
		return categoryErr("snapshot_digest_mismatch")
	case f.CasesSHA256 != rc.replay.CasesSHA256 || f.CasesSHA256 != rc.sum:
		return categoryErr("cases_sha256_mismatch")
	}
	if len(f.Cases) != len(rc.cases) {
		return categoryErr("case_set")
	}
	for _, c := range rc.cases {
		if _, ok := f.Cases[c.ID]; !ok {
			return categoryErr("case_set")
		}
	}
	for caseID, rows := range f.Cases {
		set := rc.nonSeed[caseID]
		if len(rows) != len(set) {
			return categoryErr("item_set")
		}
		for id, r := range rows {
			if !set[id] {
				return categoryErr("item_set")
			}
			if !relationEnum[r.Relation] {
				return categoryErr("enum_relation")
			}
			if !reasonEnum[r.Reason] {
				return categoryErr("enum_reason")
			}
			if !checkEnum[r.Check] {
				return categoryErr("enum_check")
			}
			if (r.Check == "disputed" || r.Check == "missing") && r.Relation != "insufficient" {
				return categoryErr("check_relation")
			}
		}
	}
	printRelationCounts(out, rc, f)
	return nil
}

// printRelationCounts prints counts only.
func printRelationCounts(out io.Writer, rc *relationContext, f relationFile) {
	byLink, byAuthor := map[[2]string]int{}, map[[2]string]int{}
	reasons := map[string]int{}
	withDifferent := 0
	total := 0
	for _, c := range rc.cases {
		counts := map[string]int{}
		for _, it := range rc.lead[c.ID] {
			if it.Hop == 0 {
				continue
			}
			row := f.Cases[c.ID][it.ID]
			total++
			counts[row.Relation]++
			byLink[[2]string{row.Relation, leadLinkBucket(it.Links)}]++
			byAuthor[[2]string{row.Relation, it.AuthoredByBot}]++
			if row.Relation == "insufficient" {
				reasons[row.Reason]++
			}
		}
		if counts["different_case"] > 0 {
			withDifferent++
		}
		fmt.Fprintf(out, "case %s", c.ID)
		for _, rel := range relationOrder {
			fmt.Fprintf(out, " %s=%d", rel, counts[rel])
		}
		fmt.Fprintln(out)
	}
	fmt.Fprintf(out, "relation labels ok: cases=%d items=%d\n", len(rc.cases), total)
	for _, m := range []struct {
		name string
		v    map[[2]string]int
	}{{"relation_by_link_bucket", byLink}, {"relation_by_author_bucket", byAuthor}} {
		keys := make([][2]string, 0, len(m.v))
		for k := range m.v {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			if keys[i][0] != keys[j][0] {
				return keys[i][0] < keys[j][0]
			}
			return keys[i][1] < keys[j][1]
		})
		for _, k := range keys {
			fmt.Fprintf(out, "%s %s %s %d\n", m.name, k[0], k[1], m.v[k])
		}
	}
	fmt.Fprintf(out, "cases_with_different_case %d\n", withDifferent)
	rk := make([]string, 0, len(reasons))
	for r := range reasons {
		rk = append(rk, r)
	}
	sort.Strings(rk)
	for _, r := range rk {
		fmt.Fprintf(out, "insufficient_reason %s %d\n", r, reasons[r])
	}
}
