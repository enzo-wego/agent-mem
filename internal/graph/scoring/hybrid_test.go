package scoring_test

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/agent-mem/agent-mem/internal/graph/scoring"
)

func TestTitleOverlap(t *testing.T) {
	for _, tt := range []struct {
		query, title string
		want         float64
	}{
		{"PAN card upload fails in SGD", "SGD card: PAN upload", 0.8},
		{"pan PAN, card", "card", 0.5},
		{"an to", "an to", 0},
		{"pan-india rollout", "India Rollout plan", 2.0 / 3},
		{"pay", "payments", 0},
		{"Café crème", "CAFÉ", 0.5},
		{"", "x", 0},
	} {
		t.Run(tt.query, func(t *testing.T) {
			if got := scoring.TitleOverlap(tt.query, tt.title); math.Abs(got-tt.want) >= 1e-12 {
				t.Fatalf("TitleOverlap(%q, %q) = %v, want %v", tt.query, tt.title, got, tt.want)
			}
		})
	}
}

func TestDefaultHybridWeights(t *testing.T) {
	w := scoring.DefaultHybridWeights()
	if want := (scoring.HybridWeights{Rec: 0.075, KW: 0.15, Title: 0.20}); w != want {
		t.Fatalf("defaults = %+v, want %+v", w, want)
	}
	b, err := json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(b), `{"hybrid_rec":0.075,"kw":0.15,"title":0.2}`; got != want {
		t.Fatalf("JSON = %s, want %s", got, want)
	}
}
