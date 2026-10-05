package scoring

import (
	"context"
	"math"
	"os"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestFuseWeighted_Contributions(t *testing.T) {
	lists := map[string][]string{"graph": {"a", "g", "a"}, "semantic": {"b", "a"}, "keyword": {"a"}}
	for _, tc := range []struct {
		name    string
		weights map[string]float64
		graph   float64
	}{
		{"nil", nil, 1}, {"missing", map[string]float64{"other": 0}, 1},
		{"double", map[string]float64{"graph": 2}, 2}, {"half", map[string]float64{"graph": .5}, .5},
		{"zero", map[string]float64{"graph": 0}, 0}, {"negative", map[string]float64{"graph": -.1}, 1},
		{"nan", map[string]float64{"graph": math.NaN()}, 1},
		{"positive infinity", map[string]float64{"graph": math.Inf(1)}, 1},
		{"negative infinity", map[string]float64{"graph": math.Inf(-1)}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := FuseWeighted(lists, RRFK, tc.weights)
			want := map[string]Fused{
				"a": {Score: tc.graph/61 + 1.0/62 + 1.0/61, Ranks: map[string]int{"graph": 1, "semantic": 2, "keyword": 1}},
				"b": {Score: 1.0 / 61, Ranks: map[string]int{"semantic": 1}},
				"g": {Score: tc.graph / 62, Ranks: map[string]int{"graph": 2}},
			}
			compareWeightedFusion(t, got, want)
		})
	}
}

func TestFuseWeighted_DefaultKAndDuplicates(t *testing.T) {
	lists := map[string][]string{"graph": {"a", "a", "b"}, "semantic": {"b", "a", "b"}}
	for _, k := range []int{-5, 0, 60, 7} {
		got := FuseWeighted(lists, k, nil)
		compareWeightedFusion(t, got, Fuse(lists, k))
		effective := k
		if effective <= 0 {
			effective = RRFK
		}
		want := map[string]Fused{
			"a": {Score: 1/float64(effective+1) + 1/float64(effective+2), Ranks: map[string]int{"graph": 1, "semantic": 2}},
			"b": {Score: 1/float64(effective+3) + 1/float64(effective+1), Ranks: map[string]int{"graph": 3, "semantic": 1}},
		}
		compareWeightedFusion(t, got, want)
	}
	compareWeightedFusion(t, FuseWeighted(nil, 0, nil), Fuse(nil, 0))
}

func compareWeightedFusion(t *testing.T, got, want map[string]Fused) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("ids: got %v, want %v", got, want)
	}
	for id, w := range want {
		g, ok := got[id]
		if !ok || !reflect.DeepEqual(g.Ranks, w.Ranks) {
			t.Errorf("%s ranks: got %+v, want %+v", id, g, w)
		}
		if math.Abs(g.Score-w.Score) > 1e-12 {
			t.Errorf("%s score: got %.15g, want %.15g", id, g.Score, w.Score)
		}
	}
}

func TestLoadGraphArmWeight(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping integration test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	cleanup := func() {
		if _, err := pool.Exec(ctx, `DELETE FROM public.settings WHERE key = 'graph.rrf.weight.graph'`); err != nil {
			t.Error(err)
		}
	}
	cleanup()
	t.Cleanup(cleanup)
	for _, tc := range []struct {
		value string
		want  float64
	}{
		{"", 1}, {"abc", 1}, {"NaN", 1}, {"Inf", 1}, {"-Inf", 1}, {"-0.1", 1}, {"1.5", 1}, {"0", 0}, {"0.3", .3}, {"1", 1},
	} {
		t.Run("value="+tc.value, func(t *testing.T) {
			cleanup()
			if tc.value != "" {
				if _, err := pool.Exec(ctx, `INSERT INTO public.settings(key,value) VALUES($1,$2)`, GraphArmWeightKey, tc.value); err != nil {
					t.Fatal(err)
				}
			}
			got, err := LoadGraphArmWeight(ctx, pool)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("got %g, want %g", got, tc.want)
			}
		})
	}
}
