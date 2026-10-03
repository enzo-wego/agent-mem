package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func transcriptFixture() []string {
	var lines []string
	for i := 1; i <= 20; i++ {
		lines = append(lines, fmt.Sprintf("line-%02d\n", i))
	}
	return lines
}
func TestFitTranscript_UnderBudgetUnchanged(t *testing.T) {
	lines := transcriptFixture()
	if got := fitTranscript(lines, 160); got != strings.Join(lines, "") {
		t.Fatalf("got %q", got)
	}
}
func TestFitTranscript_HeadMarkerTail(t *testing.T) {
	want := "line-01\nline-02\nline-03\n[… 12 messages omitted …]\nline-16\nline-17\nline-18\nline-19\nline-20\n"
	if got := fitTranscript(transcriptFixture(), 100); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
func TestFitTranscript_OneByteUnder(t *testing.T) {
	lines := transcriptFixture()
	want := strings.Join(lines[:4], "") + fmt.Sprintf(transcriptMarkerFmt, 4) + strings.Join(lines[8:], "")
	if got := fitTranscript(lines, 159); got != want {
		t.Fatalf("got %q", got)
	}
}
func TestTranscriptLine(t *testing.T) {
	want := "[3 2026-10-01 ts=1788350714.195559] Tuan (Payments · Engineer): final design / agreed\n"
	if got := transcriptLine(3, "2026-10-01", "1788350714.195559", "Tuan (Payments · Engineer)", "final  design\nagreed"); got != want {
		t.Fatalf("got %q", got)
	}
}
func TestGroundDecisions(t *testing.T) {
	dates := map[string]string{"100.000001": "2026-10-01", "100.000002": "2026-10-02"}
	in := []threadDecision{{Text: "Use final design", By: "Tuan", Date: "2026-09-30", TS: "100.000002"}, {Text: "Fake", By: "X", TS: "999.999999"}, {Text: "  ", TS: "100.000001"}, {Text: "bookingRef not required", By: "Lan", TS: " ts=100.000001"}}
	want := []threadDecision{{Text: "Use final design", By: "Tuan", Date: "2026-10-02", TS: "100.000002"}, {Text: "bookingRef not required", By: "Lan", Date: "2026-10-01", TS: "100.000001"}}
	if got := groundDecisions(in, dates); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v", got)
	}
	raw, _ := json.Marshal(groundDecisions(nil, dates))
	if string(raw) != "[]" {
		t.Fatal(string(raw))
	}
	in = nil
	for i := range 25 {
		in = append(in, threadDecision{Text: fmt.Sprint(i), TS: "100.000001"})
	}
	got := groundDecisions(in, dates)
	if len(got) != 20 || got[0].Text != "5" || got[19].Text != "24" {
		t.Fatal(got)
	}
}
func TestCleanOpenQuestions(t *testing.T) {
	if got := cleanOpenQuestions([]string{" a ", "", "b"}); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatal(got)
	}
	raw, _ := json.Marshal(cleanOpenQuestions(nil))
	if string(raw) != "[]" {
		t.Fatal(string(raw))
	}
	in := make([]string, 12)
	for i := range in {
		in[i] = fmt.Sprint(i)
	}
	got := cleanOpenQuestions(in)
	if len(got) != 10 || got[9] != "9" {
		t.Fatal(got)
	}
}
func TestSlackMessagePermalink(t *testing.T) {
	root := "1788350714.195559"
	base := "https://wego.slack.com/archives/C0BBJAHV4G1/p"
	if got := slackMessagePermalink("C0BBJAHV4G1", root, root); got != base+"1788350714195559" {
		t.Fatal(got)
	}
	if got := slackMessagePermalink("C0BBJAHV4G1", root, "1791000000.000100"); got != base+"1791000000000100?thread_ts="+root+"&cid=C0BBJAHV4G1" {
		t.Fatal(got)
	}
}
func TestDecodeThreadDecisions_NullAndEmpty(t *testing.T) {
	d, q := decodeThreadDecisions("C", "100.1", nil, nil)
	if d != nil || q != nil {
		t.Fatal(d, q)
	}
	d, q = decodeThreadDecisions("C", "100.1", []byte("[]"), []byte("[]"))
	if len(d) != 0 || len(q) != 0 {
		t.Fatal(d, q)
	}
	d, _ = decodeThreadDecisions("C", "100.1", []byte(`[{"text":"Ship","ts":"100.2"}]`), nil)
	if len(d) != 1 || d[0].URL != slackMessagePermalink("C", "100.1", "100.2") {
		t.Fatal(d)
	}
	d, q = decodeThreadDecisions("C", "100.1", []byte("bad"), []byte("bad"))
	if d != nil || q != nil {
		t.Fatal(d, q)
	}
}
func TestGenThreadDeepSummary_ParsesDecisions(t *testing.T) {
	g := &mockGemini{generateResult: func() (string, error) {
		return `{"topic":"T","overview":"O","highlights":["h"],"kind":"substantive","decisions":[{"text":"A","ts":" ts=1"},{"text":"B","ts":"2"}],"open_questions":["Q"]}`, nil
	}}
	ds := genThreadDeepSummary(context.Background(), g, "thread")
	if len(ds.Decisions) != 2 || ds.Decisions[0].TS != " ts=1" || !reflect.DeepEqual(ds.OpenQuestions, []string{"Q"}) {
		t.Fatal(ds)
	}
}
func TestGenThreadDeepSummary_WithoutNewFields(t *testing.T) {
	g := &mockGemini{generateResult: func() (string, error) {
		return `{"topic":"T","overview":"O","highlights":["h"],"kind":"substantive"}`, nil
	}}
	ds := genThreadDeepSummary(context.Background(), g, "thread")
	if ds.Topic != "T" || ds.Overview != "O" || ds.Kind != "substantive" || !reflect.DeepEqual(ds.Highlights, []string{"h"}) || ds.Decisions != nil || ds.OpenQuestions != nil {
		t.Fatal(ds)
	}
}
