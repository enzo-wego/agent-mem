package scoring_test

import (
	"math"
	"testing"
	"time"

	"github.com/agent-mem/agent-mem/internal/graph/scoring"
)

func TestRecency_DecaysExpectedly(t *testing.T) {
	now := time.Date(2026, 5, 27, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		ageDays float64
		want    float64
	}{
		{0, 1.00},
		{30, 0.367}, // exp(-1)
		{60, 0.135},
		{90, 0.0498},
	}
	for _, tc := range cases {
		got := scoring.Recency(now.Add(-time.Duration(tc.ageDays*24)*time.Hour), now, 30*24*time.Hour)
		if math.Abs(got-tc.want) > 0.01 {
			t.Errorf("age %vd: got %.3f want %.3f", tc.ageDays, got, tc.want)
		}
	}
}

func TestEdgeProximity_HopsTable(t *testing.T) {
	cases := []struct {
		hops int
		want float64
	}{
		{0, 1.00},  // seed itself
		{1, 0.50},
		{2, 0.333},
		{3, 0.25},
	}
	for _, tc := range cases {
		got := scoring.Edge(tc.hops)
		if math.Abs(got-tc.want) > 0.01 {
			t.Errorf("hops=%d: got %.3f want %.3f", tc.hops, got, tc.want)
		}
	}
}

func TestPersonScore_Buckets(t *testing.T) {
	cases := []struct {
		desc string
		ctx  scoring.PersonContext
		want float64
	}{
		{"self", scoring.PersonContext{IsSelf: true}, 1.00},
		{"team-group", scoring.PersonContext{InAskerTeamGroup: true}, 0.90},
		{"dept-group", scoring.PersonContext{InAskerDeptGroup: true}, 0.70},
		{"subtree-2-hops", scoring.PersonContext{OrgDistance: 2}, 0.40},
		{"distant", scoring.PersonContext{OrgDistance: 6}, 0.10},
		{"external", scoring.PersonContext{IsExternal: true}, 0.10},
	}
	for _, tc := range cases {
		got := scoring.Person(tc.ctx)
		if math.Abs(got-tc.want) > 0.01 {
			t.Errorf("%s: got %.2f want %.2f", tc.desc, got, tc.want)
		}
	}
}

func TestAuthority_DepthInverted(t *testing.T) {
	// max_depth = 6
	cases := []struct {
		depth int16
		want  float64
	}{
		{0, 1.0},   // root
		{2, 0.667},
		{4, 0.333},
		{6, 0.0},
	}
	for _, tc := range cases {
		got := scoring.Authority(tc.depth, 6)
		if math.Abs(got-tc.want) > 0.01 {
			t.Errorf("depth=%d: got %.3f want %.3f", tc.depth, got, tc.want)
		}
	}
}

func TestSemantic_PassThrough(t *testing.T) {
	if got := scoring.Semantic(0.87); got != 0.87 {
		t.Errorf("got %v want 0.87", got)
	}
}
