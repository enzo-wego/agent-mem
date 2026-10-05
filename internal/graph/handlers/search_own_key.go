package handlers

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/agent-mem/agent-mem/internal/graph/scoring"
	"github.com/agent-mem/agent-mem/internal/graph/temporal"
)

var (
	ownKeyJiraQuery = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]{1,9}-[0-9]{1,6}$`)
	ownKeyPRQuery   = regexp.MustCompile(`^wego/[\w.-]+#[0-9]+$`)
)

// ponytail: own references pin their owners; no general rank change.
func (s *Search) pinOwnKey(ctx context.Context, q string, results []searchResult, f searchFilter,
	fused map[string]scoring.Fused, armLocal map[string]map[string]float64,
	alphas scoring.BoostAlphas, askerEEID int, win temporal.Window, hasWindow bool, now time.Time) ([]searchResult, error) {
	refs := queryRefs(q)
	if len(refs) == 0 {
		return results, nil
	}
	pinned := make([]searchResult, 0, len(results)+len(refs))
	for _, id := range refs {
		at := -1
		for i, result := range results {
			if result.NodeID == id {
				at = i
				break
			}
		}
		if at >= 0 {
			pinned = append(pinned, results[at])
			continue
		}
		// Hydration is the eligibility check, including deleted/type/ACL/epic
		// filters. Only arms that actually returned the owner retain ranks.
		if _, ok := fused[id]; !ok {
			fused[id] = scoring.Fused{}
		}
		owner, err := s.hydrateResults(ctx, []string{id}, f, fused, armLocal, alphas, askerEEID, win, hasWindow, now)
		if err != nil {
			return nil, err
		}
		if len(owner) > 0 {
			pinned = append(pinned, owner[0])
		}
	}
	if len(pinned) == 0 {
		return results, nil
	}
	pinCount := len(pinned)
	for _, result := range results {
		isPinned := false
		for _, owner := range pinned[:pinCount] {
			if owner.NodeID == result.NodeID {
				isPinned = true
				break
			}
		}
		if !isPinned {
			pinned = append(pinned, result)
		}
	}
	return pinned, nil
}

func queryRefs(q string) []string {
	q = strings.TrimSpace(q)
	if ownKeyJiraQuery.MatchString(q) {
		return []string{"jira:" + strings.ToUpper(q)}
	}
	if ref := strings.ToLower(q); ownKeyPRQuery.MatchString(ref) {
		return []string{"gh_pr:" + ref}
	}
	matches := [3][][]int{
		reJiraKey.FindAllStringSubmatchIndex(q, -1),
		reGHPRShort.FindAllStringSubmatchIndex(q, -1),
		reGHPRURL.FindAllStringSubmatchIndex(q, -1),
	}
	var next [3]int
	var refs []string
	// ponytail: cap at three distinct references before hydration, even if
	// an earlier reference is missing or ineligible.
	for len(refs) < 3 {
		first := -1
		for i := range matches {
			if next[i] < len(matches[i]) && (first < 0 || matches[i][next[i]][0] < matches[first][next[first]][0]) {
				first = i
			}
		}
		if first < 0 {
			break
		}
		m := matches[first][next[first]]
		next[first]++
		id := "jira:" + q[m[0]:m[1]]
		if first != 0 {
			id = "gh_pr:" + strings.ToLower(q[m[2]:m[3]]+"#"+q[m[4]:m[5]])
		}
		duplicate := false
		for _, ref := range refs {
			duplicate = duplicate || ref == id
		}
		if !duplicate {
			refs = append(refs, id)
		}
	}
	return refs
}
