package handlers

import (
	"context"
	"net/http"
)

// WithLeadMode sets whether lead mode may run (worker flag GRAPH_LEAD_MODE_ENABLED).
func (s *Search) WithLeadMode(enabled bool) *Search {
	s.leadEnabled = enabled
	return s
}

// leadPage applies A2a to the final result page and computes the A4 fields.
// Slack rows whose thread root is not eligible (hidden, missing, deleted,
// outside the epic filter, other scope) are dropped, in default and hybrid
// output alike. A lookup failure returns errLeadLookup.
func (s *Search) leadPage(ctx context.Context, r *http.Request, acl askerACL, results []searchResult, epicKeys []string) ([]searchResult, map[string]*leadFields, error) {
	ids := make([]string, len(results))
	exempt := make(map[string]bool, len(results))
	for i, res := range results {
		ids[i] = res.NodeID
		exempt[res.NodeID] = true
	}
	roots, err := leadRoots(ctx, s.db, acl, ids, epicKeys, exempt)
	if err != nil {
		return nil, nil, err
	}
	kept := make([]searchResult, 0, len(results))
	items := make([]leadItem, 0, len(results))
	for _, res := range results {
		lr, live := roots[res.NodeID]
		if !live || !lr.Eligible {
			continue
		}
		kept = append(kept, res)
		items = append(items, leadItem{ID: res.NodeID, Root: lr.Root})
	}
	fields, err := leadEnrich(ctx, s.db, acl, isAdminRequest(r), items)
	if err != nil {
		return nil, nil, err
	}
	return kept, fields, nil
}
