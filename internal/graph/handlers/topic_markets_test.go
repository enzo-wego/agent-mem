package handlers

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestDetectMarkets(t *testing.T) {
	for _, tc := range []struct {
		text string
		want []string
	}{
		{"Request for CSV of all IN bookings", []string{"IN"}},
		{"fixed in prod", []string{}},
		{"Tamara risk-check vs Tabby", []string{"SA"}},
		{"IN and SA tax", []string{"IN", "SA"}},
		{"india gst", []string{"IN"}},
	} {
		if got := detectMarkets(tc.text); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("detectMarkets(%q) = %v, want %v", tc.text, got, tc.want)
		}
	}
}

func TestParseMarkets(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want []string
	}{
		{"valid", `["SA","IN","IN"]`, []string{"IN", "SA"}},
		{"mixed", `["in","IN",5,"IND",null,"SA"]`, []string{"IN", "SA"}},
		{"string", `"IN"`, []string{}},
		{"number", `17`, []string{}},
		{"object", `{}`, []string{}},
		{"null", `null`, []string{}},
		{"absent", ``, []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parseMarkets(json.RawMessage(tc.raw))
			if got == nil {
				t.Fatal("parseMarkets returned nil")
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("parseMarkets(%s) = %v, want %v", tc.raw, got, tc.want)
			}
		})
	}
}

func TestResolveMarkets(t *testing.T) {
	if got := resolveMarkets([]string{}, true, "India GST"); len(got) != 0 {
		t.Fatalf("stored empty markets fell back to detection: %v", got)
	}
	for _, hasStored := range []bool{true, false} {
		if got := resolveMarkets(nil, hasStored, "India GST"); !reflect.DeepEqual(got, []string{"IN"}) {
			t.Fatalf("resolveMarkets(nil, %v) = %v, want [IN]", hasStored, got)
		}
	}
}

func TestRecentMarketCandidateMatches(t *testing.T) {
	for _, tc := range []struct {
		name      string
		src, cand []string
		srcScope  string
		candScope string
		want      bool
	}{
		{"overlap", []string{"IN"}, []string{"IN", "SA"}, "slack:C1", "slack:C2", true},
		{"no overlap", []string{"IN"}, []string{"SA"}, "slack:C1", "slack:C1", false},
		{"candidate empty", []string{"IN"}, []string{}, "slack:C1", "slack:C1", false},
		{"source empty same scope", []string{}, []string{"SA"}, "slack:C1", "slack:C1", true},
		{"source empty other scope", []string{}, []string{"IN"}, "slack:C1", "slack:C2", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := recentMarketCandidateMatches(tc.src, tc.cand, tc.srcScope, tc.candScope); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestGenThreadDeepSummaryMarketsAreFieldLocal(t *testing.T) {
	for _, tc := range []struct {
		name, reply string
		want        []string
		wantTopic   string
	}{
		{"present", `{"topic":"Valid topic","overview":"Valid overview","kind":"substantive","markets":["SA","IN"]}`, []string{"IN", "SA"}, "Valid topic"},
		{"legacy absent", `{"topic":"Valid topic","overview":"Valid overview","kind":"substantive"}`, []string{}, "Valid topic"},
		{"malformed field", `{"topic":"Valid topic","overview":"Valid overview","kind":"substantive","markets":[17]}`, []string{}, "Valid topic"},
		{"prose", `A plain prose summary`, []string{}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gem := &mockGemini{generateResult: func() (string, error) { return tc.reply, nil }}
			got := genThreadDeepSummary(context.Background(), gem, "thread")
			if got.Markets == nil || !reflect.DeepEqual(got.Markets, tc.want) {
				t.Fatalf("markets = %v, want %v", got.Markets, tc.want)
			}
			if got.Topic != tc.wantTopic {
				t.Fatalf("topic = %q, want %q", got.Topic, tc.wantTopic)
			}
			if tc.wantTopic != "" && !strings.Contains(got.Overview, "Valid overview") {
				t.Fatalf("valid structured fields were discarded: %+v", got)
			}
		})
	}
}
