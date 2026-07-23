package handlers

import (
	"sort"
	"testing"
)

func TestDeltaLabel(t *testing.T) {
	cases := []struct {
		cur, base float64
		want      string
	}{
		{42, 118, "Δ -64%"},   // filters working: big drop vs baseline
		{120, 100, "Δ +20%"},  // spike
		{0, 0, "flat"},        // both zero
		{5, 0, "no baseline"}, // new activity, no history
		{100, 100, "Δ +0%"},   // unchanged
	}
	for _, c := range cases {
		if got := deltaLabel(c.cur, c.base); got != c.want {
			t.Errorf("deltaLabel(%v,%v) = %q; want %q", c.cur, c.base, got, c.want)
		}
	}
}

func TestScopeList(t *testing.T) {
	got := scopeList([]string{"C1", "C2"})
	if len(got) != 2 || got[0] != "slack:C1" || got[1] != "slack:C2" {
		t.Fatalf("scopeList = %v; want [slack:C1 slack:C2]", got)
	}
}

func TestIgnoreList(t *testing.T) {
	f := compileChannelFilters(`{"ignore":["C1","C2","C3"]}`)
	got := f.ignoreList()
	sort.Strings(got)
	if len(got) != 3 || got[0] != "C1" || got[2] != "C3" {
		t.Fatalf("ignoreList = %v; want 3 ids C1..C3", got)
	}
}
