package scoring

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Settings keys for the hybrid-search-only weights. Absent or unparsable keys
// fall back to DefaultHybridWeights.
const (
	KeyHybridRec   = "graph.weights.hybrid_rec"
	KeyHybridKW    = "graph.weights.kw"
	KeyHybridTitle = "graph.weights.title"
)

// HybridWeights are the weights only /search?match=hybrid uses. Rec replaces
// Weights.Rec there; KW and Title weight the two hybrid boosts.
type HybridWeights struct {
	Rec   float64 `json:"hybrid_rec"`
	KW    float64 `json:"kw"`
	Title float64 `json:"title"`
}

// DefaultHybridWeights are the values the 2026-10-03 ranking eval chose
// (40 tickets, 80 linked threads: MRR 0.395 -> 0.746).
func DefaultHybridWeights() HybridWeights {
	return HybridWeights{Rec: 0.075, KW: 0.15, Title: 0.20}
}

// LoadHybridWeights reads the hybrid-only settings, falling back to defaults.
func LoadHybridWeights(ctx context.Context, db *pgxpool.Pool) (HybridWeights, error) {
	w := DefaultHybridWeights()
	rows, err := db.Query(ctx, `SELECT key, value FROM public.settings WHERE key IN ($1,$2,$3)`, KeyHybridRec, KeyHybridKW, KeyHybridTitle)
	if err != nil {
		return w, fmt.Errorf("load hybrid weights: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return w, err
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			continue
		}
		switch k {
		case KeyHybridRec:
			w.Rec = f
		case KeyHybridKW:
			w.KW = f
		case KeyHybridTitle:
			w.Title = f
		}
	}
	return w, rows.Err()
}

// TitleOverlap is the share of the query's distinct words that are also words
// of the title. A word is a lowercased run of letters and digits; query words
// shorter than 3 runes are ignored. 0 when the query has no such word.
func TitleOverlap(query, title string) float64 {
	q := words(query, 3)
	if len(q) == 0 {
		return 0
	}
	t := words(title, 1)
	hit := 0
	for w := range q {
		if t[w] {
			hit++
		}
	}
	return float64(hit) / float64(len(q))
}

func words(s string, minRunes int) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if utf8.RuneCountInString(w) < minRunes {
			continue
		}
		out[w] = true
	}
	return out
}
