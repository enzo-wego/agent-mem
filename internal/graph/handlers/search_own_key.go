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

// ponytail: exact own-key query pins the owner; no general rank change.
func (s *Search) pinOwnKey(ctx context.Context, q string, results []searchResult, f searchFilter,
	fused map[string]scoring.Fused, armLocal map[string]map[string]float64,
	alphas scoring.BoostAlphas, askerEEID int, win temporal.Window, hasWindow bool, now time.Time) ([]searchResult, error) {
	q = strings.TrimSpace(q)
	var id string
	if ownKeyJiraQuery.MatchString(q) {
		id = "jira:" + strings.ToUpper(q)
	} else if ref := strings.ToLower(q); ownKeyPRQuery.MatchString(ref) {
		id = "gh_pr:" + ref
	} else {
		return results, nil
	}

	at := -1
	for i, result := range results {
		if result.NodeID == id {
			at = i
			break
		}
	}
	if at < 0 {
		// Hydration is the eligibility check, including deleted/type/ACL/epic
		// filters. Only arms that actually returned the owner retain ranks.
		if _, ok := fused[id]; !ok {
			fused[id] = scoring.Fused{}
		}
		owner, err := s.hydrateResults(ctx, []string{id}, f, fused, armLocal, alphas, askerEEID, win, hasWindow, now)
		if err != nil {
			return nil, err
		}
		if len(owner) == 0 {
			return results, nil
		}
		results = append(results, owner[0])
		at = len(results) - 1
	}
	owner := results[at]
	copy(results[1:at+1], results[:at])
	results[0] = owner
	return results, nil
}
