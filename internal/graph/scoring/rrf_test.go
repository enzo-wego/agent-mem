package scoring

import (
	"math"
	"testing"
)

func TestFuse_TwoLists(t *testing.T) {
	got := Fuse(map[string][]string{
		"semantic": {"a", "b", "c"},
		"keyword":  {"c", "a", "d", "d"},
	}, 60)

	if len(got) != 4 {
		t.Fatalf("fused %d ids, want 4: %v", len(got), got)
	}
	want := map[string]float64{
		"a": 1.0/61 + 1.0/62,
		"b": 1.0 / 62,
		"c": 1.0/63 + 1.0/61,
		"d": 1.0 / 63, // duplicate keeps first rank, no double count
	}
	for id, s := range want {
		if math.Abs(got[id].Score-s) > 1e-12 {
			t.Errorf("%s score = %v, want %v", id, got[id].Score, s)
		}
	}
	// a and c both appear in both lists with ranks {1,2} / {3,1}; a wins on
	// the smaller rank sum.
	if got["a"].Score <= got["c"].Score || got["c"].Score <= got["b"].Score {
		t.Errorf("order want a > c > b: a=%v c=%v b=%v", got["a"].Score, got["c"].Score, got["b"].Score)
	}
	if r := got["a"].Ranks; r["semantic"] != 1 || r["keyword"] != 2 {
		t.Errorf("a ranks = %v", r)
	}
	if _, ok := got["b"].Ranks["keyword"]; ok {
		t.Errorf("b must not have a keyword rank: %v", got["b"].Ranks)
	}
	if got["d"].Ranks["keyword"] != 3 {
		t.Errorf("d keyword rank = %d, want 3", got["d"].Ranks["keyword"])
	}
}

func TestFuse_DefaultKAndEmpty(t *testing.T) {
	if got := Fuse(nil, 0); len(got) != 0 {
		t.Errorf("nil lists → %v", got)
	}
	got := Fuse(map[string][]string{"x": {"a"}}, 0)
	if math.Abs(got["a"].Score-1.0/(RRFK+1)) > 1e-12 {
		t.Errorf("k=0 must fall back to %d: %v", RRFK, got["a"].Score)
	}
}

func TestBoost(t *testing.T) {
	a := DefaultBoostAlphas
	if s := Boost(1, a, 0.5, 0.5, 0.5, 0.5); math.Abs(s-1) > 1e-12 {
		t.Errorf("neutral components must not change the score: %v", s)
	}
	if s := Boost(1, a, 1, 0.5, 0.5, 0.5); math.Abs(s-1.1) > 1e-12 {
		t.Errorf("rec=1 with α=0.2 → 1.1, got %v", s)
	}
	if s := Boost(1, a, 0.5, 0.5, 0.5, 0); math.Abs(s-0.95) > 1e-12 {
		t.Errorf("auth=0 with α=0.1 → 0.95, got %v", s)
	}
	if s := Boost(1, BoostAlphas{}, 0, 0, 0, 0); s != 1 {
		t.Errorf("zero alphas disable boosts, got %v", s)
	}
}
