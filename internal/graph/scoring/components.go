// Package scoring computes the five-component relevance score for graph nodes.
package scoring

import (
	"math"
	"time"
)

// Semantic is the cosine similarity between query and node embedding,
// already normalised to [0,1] by the caller. Pass-through for symmetry.
func Semantic(cosine float64) float64 {
	if cosine < 0 {
		return 0
	}
	if cosine > 1 {
		return 1
	}
	return cosine
}

// Recency is exp(-age / halfLife). Default halfLife ~30 days.
func Recency(nodeTS, now time.Time, halfLife time.Duration) float64 {
	age := now.Sub(nodeTS)
	if age <= 0 {
		return 1.0
	}
	if halfLife <= 0 {
		return 0
	}
	return math.Exp(-float64(age) / float64(halfLife))
}

// Edge is 1 / (1 + hops_from_seed). Seed itself = 1.0, 1 hop = 0.5,
// 2 hops = 0.333, 3 hops = 0.25.
func Edge(hops int) float64 {
	if hops < 0 {
		hops = 0
	}
	return 1.0 / (1.0 + float64(hops))
}

// PersonContext captures everything the person component needs to know
// to bucket a person relative to the asker.
type PersonContext struct {
	IsSelf           bool
	InAskerTeamGroup bool // payments-geeks
	InAskerDeptGroup bool // payments-ops
	OrgDistance      int  // hops in BambooHR tree; 0 if same node, math.MaxInt if unknown
	IsExternal       bool // no @wego.com email
}

// Person returns the affinity score in [0,1] using the bucket ladder
// from the design.
func Person(p PersonContext) float64 {
	if p.IsSelf {
		return 1.00
	}
	if p.InAskerTeamGroup {
		return 0.90
	}
	if p.InAskerDeptGroup {
		return 0.70
	}
	if p.IsExternal {
		return 0.10
	}
	if p.OrgDistance <= 2 {
		return 0.40
	}
	if p.OrgDistance <= 4 {
		return 0.25
	}
	return 0.10
}

// Authority is 1 - depth/maxDepth. Roots (depth 0) score 1.0; lowest IC
// scores 0. Used as a small-weight tiebreaker.
func Authority(depth int16, maxDepth int16) float64 {
	if maxDepth <= 0 {
		return 0
	}
	if depth >= maxDepth {
		return 0
	}
	if depth < 0 {
		return 1.0
	}
	return 1.0 - float64(depth)/float64(maxDepth)
}
