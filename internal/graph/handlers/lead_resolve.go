package handlers

import (
	"encoding/json"
	"net/http"
	"sort"
	"time"
	"unicode/utf8"

	"github.com/agent-mem/agent-mem/internal/graph/bfs"
)

type leadResolveParams struct {
	req        resolveRequest
	maxItems   int
	acl        askerACL
	epicKeys   []string
	canonSeeds []string
	focus      map[string]string
	filtered   []bfs.Candidate
	visited    int
	scored     []scoredCand
	t0         time.Time
}

type leadGroup struct {
	key     string
	slack   bool
	seed    bool
	members []scoredCand
	best    scoredCand
	focusTS string
}

// betterCand: higher score, then lower node id.
func betterCand(a, b scoredCand) bool {
	if a.Score != b.Score {
		return a.Score > b.Score
	}
	return a.NodeID < b.NodeID
}

// serveLead is the resolve lead pipeline (plan A2, steps 3-9). Steps 1-2
// (authorize, seeds) ran in ServeHTTP.
func (h *Resolve) serveLead(w http.ResponseWriter, r *http.Request, p leadResolveParams) {
	ctx := r.Context()
	admin := isAdminRequest(r)

	authorized := make(map[string]bool, len(p.filtered))
	for _, c := range p.filtered {
		authorized[c.NodeID] = true
	}
	scoredIDs := make([]string, 0, len(p.scored))
	for _, s := range p.scored {
		scoredIDs = append(scoredIDs, s.NodeID)
	}

	// 3-4. Map to roots and authorize them; a group whose root fails is dropped.
	roots, err := leadRoots(ctx, h.db, p.acl, scoredIDs, p.epicKeys, authorized)
	if err != nil {
		writeLeadLookupFailed(w)
		return
	}

	// 5. Group.
	seedSet := make(map[string]bool, len(p.canonSeeds))
	for _, s := range p.canonSeeds {
		seedSet[s] = true
	}
	groups := map[string]*leadGroup{}
	memberGroup := map[string]string{}
	for _, s := range p.scored {
		lr, live := roots[s.NodeID]
		if !live || !lr.Eligible {
			continue
		}
		key, slack := s.NodeID, false
		if lr.Root != "" {
			key, slack = lr.Root, true
		}
		g := groups[key]
		if g == nil {
			g = &leadGroup{key: key, slack: slack, seed: seedSet[key]}
			groups[key] = g
		}
		g.members = append(g.members, s)
		memberGroup[s.NodeID] = key
	}
	var seeds, others []*leadGroup
	for _, g := range groups {
		g.best = g.members[0]
		for _, m := range g.members[1:] {
			if betterCand(m, g.best) {
				g.best = m
			}
		}
		if g.seed {
			g.best.Hop = 0
		}
		if g.slack {
			if ts, ok := p.focus[g.key]; ok {
				g.focusTS = ts
			} else {
				var bestReply *scoredCand
				for i := range g.members {
					m := g.members[i]
					if m.NodeID == g.key {
						continue
					}
					if bestReply == nil || betterCand(m, *bestReply) {
						bestReply = &g.members[i]
					}
				}
				if bestReply != nil {
					g.focusTS = threadTSOfRoot(bestReply.NodeID)
				}
			}
		}
		if g.seed {
			seeds = append(seeds, g)
		} else {
			others = append(others, g)
		}
	}

	// 6. Select: seeds always; then others up to max_items.
	order := func(l []*leadGroup) {
		sort.Slice(l, func(i, j int) bool {
			if l[i].best.Score != l[j].best.Score {
				return l[i].best.Score > l[j].best.Score
			}
			return l[i].key < l[j].key
		})
	}
	order(seeds)
	order(others)
	selected := append([]*leadGroup{}, seeds...)
	if room := p.maxItems - len(seeds); room > 0 {
		if room > len(others) {
			room = len(others)
		}
		selected = append(selected, others[:room]...)
	}
	selectedKeys := make([]string, len(selected))
	for i, g := range selected {
		selectedKeys[i] = g.key
	}

	// 7. Misses over every selected item.
	var missed []string
	if len(selectedKeys) > 0 {
		mrows, err := h.db.Query(ctx, `SELECT n.id FROM unnest($1::text[]) AS n(id)
WHERE NOT EXISTS (SELECT 1 FROM graph.artifact_bodies b WHERE b.node_id = n.id)`, selectedKeys)
		if err != nil {
			writeLeadLookupFailed(w)
			return
		}
		missSet := map[string]bool{}
		for mrows.Next() {
			var id string
			if err := mrows.Scan(&id); err != nil {
				mrows.Close()
				writeLeadLookupFailed(w)
				return
			}
			missSet[id] = true
		}
		mrows.Close()
		if mrows.Err() != nil {
			writeLeadLookupFailed(w)
			return
		}
		for _, k := range selectedKeys {
			if missSet[k] {
				missed = append(missed, k)
			}
		}
	}

	// Display data for the selected items.
	type display struct{ typ, url, title, author, threadTS string }
	disp := map[string]display{}
	if len(selectedKeys) > 0 {
		drows, err := h.db.Query(ctx, `
SELECT n.id, n.type, COALESCE(n.url,''),
       COALESCE(NULLIF(n.title,''), CASE WHEN n.type = 'slack' THEN NULLIF(ts.summary,'') END, ''),
       COALESCE(p.display_name,''), COALESCE(n.metadata->>'thread_ts','')
FROM graph.nodes n
LEFT JOIN graph.people p ON p.id = n.author_person_id
LEFT JOIN graph.thread_summaries ts
  ON ts.channel_id = REPLACE(n.scope,'slack:','')
 AND ts.thread_ts = COALESCE(NULLIF(n.metadata->>'thread_ts',''), split_part(n.id,':',3))
WHERE n.id = ANY($1) AND n.deleted_at IS NULL`, selectedKeys)
		if err != nil {
			writeLeadLookupFailed(w)
			return
		}
		for drows.Next() {
			var id string
			var d display
			if err := drows.Scan(&id, &d.typ, &d.url, &d.title, &d.author, &d.threadTS); err != nil {
				drows.Close()
				writeLeadLookupFailed(w)
				return
			}
			disp[id] = d
		}
		drows.Close()
		if drows.Err() != nil {
			writeLeadLookupFailed(w)
			return
		}
		for _, k := range selectedKeys {
			if _, ok := disp[k]; !ok {
				writeLeadLookupFailed(w)
				return
			}
		}
	}

	// 8. Links between seed members and members of selected non-seed groups.
	selectedSet := map[string]bool{}
	var seedMembers, otherMembers []string
	for _, g := range selected {
		selectedSet[g.key] = true
		for _, m := range g.members {
			if g.seed {
				seedMembers = append(seedMembers, m.NodeID)
			} else {
				otherMembers = append(otherMembers, m.NodeID)
			}
		}
	}
	best := map[string]map[string]linkPick{}
	if len(seedMembers) > 0 && len(otherMembers) > 0 {
		lrows, err := h.db.Query(ctx, `
SELECT from_node_id, to_node_id, kind, metadata
FROM graph.edges
WHERE (from_node_id = ANY($1) AND to_node_id = ANY($2))
   OR (to_node_id = ANY($1) AND from_node_id = ANY($2))`, seedMembers, otherMembers)
		if err != nil {
			writeLeadLookupFailed(w)
			return
		}
		for lrows.Next() {
			var from, to, kind string
			var meta map[string]any
			if err := lrows.Scan(&from, &to, &kind, &meta); err != nil {
				lrows.Close()
				writeLeadLookupFailed(w)
				return
			}
			gf, gt := memberGroup[from], memberGroup[to]
			if gf == "" || gt == "" || gf == gt || !selectedSet[gf] || !selectedSet[gt] {
				continue
			}
			other := ""
			switch {
			case groups[gf].seed && !groups[gt].seed:
				other = gt
			case groups[gt].seed && !groups[gf].seed:
				other = gf
			default:
				continue
			}
			l := resolveLink{Kind: kind, From: gf, To: gt}
			l.Tag, _ = meta["tag"].(string)
			l.Topic, _ = meta["topic"].(string)
			l.Why, _ = meta["why"].(string)
			l.Confidence, _ = meta["confidence"].(float64)
			pick := linkPick{link: l, from: gf, to: gt}
			if best[other] == nil {
				best[other] = map[string]linkPick{}
			}
			if cur, ok := best[other][kind]; !ok || leadLinkBetter(pick, cur) {
				best[other][kind] = pick
			}
		}
		lrows.Close()
		if lrows.Err() != nil {
			writeLeadLookupFailed(w)
			return
		}
	}

	// 9. Enrich and respond.
	items := make([]leadItem, len(selected))
	for i, g := range selected {
		items[i] = leadItem{ID: g.key}
		if g.slack {
			items[i].Root = g.key
		}
	}
	fields, err := leadEnrich(ctx, h.db, p.acl, admin, items)
	if err != nil {
		writeLeadLookupFailed(w)
		return
	}
	resp := resolveResponse{
		Artifacts: make([]resolveArtifact, 0, len(selected)),
		GraphTrace: resolveTrace{
			Seeds:               p.req.Seeds,
			ExpandedNodes:       p.visited,
			AfterACL:            len(p.filtered),
			AfterScoreThreshold: len(p.scored),
			TookMs:              time.Since(p.t0).Milliseconds(),
		},
		CacheMisses: missed,
	}
	for _, g := range selected {
		d := disp[g.key]
		f := fields[g.key]
		a := resolveArtifact{
			NodeID: g.key, URL: d.url, Type: d.typ, Title: d.title,
			Author: d.author, ThreadTS: d.threadTS,
			Score: g.best.Score, ScoreBreakdown: g.best.Breakdown, Hop: g.best.Hop,
			Links:      linksFor(best[g.key]),
			leadFields: f,
		}
		if g.slack {
			focus := g.focusTS
			a.FocusTS = &focus
		}
		resp.Artifacts = append(resp.Artifacts, a)
		resp.ContextTokens += (utf8.RuneCountInString(d.title) + utf8.RuneCountInString(f.LeadSummary) + 3) / 4
	}

	if !noSideEffects(r, p.req.NoSideEffects) {
		for _, m := range missed {
			h.enqueueFetchBodyFn(ctx, m)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func linksFor(byKind map[string]linkPick) []resolveLink {
	if len(byKind) == 0 {
		return nil
	}
	return sortedLinks(byKind)
}

// leadLinkBetter is linkPick.better plus a total order on the prose fields:
// once edge endpoints fold to thread roots, two edges can tie on confidence and
// (from, to), and the winner must not depend on row order.
func leadLinkBetter(p, q linkPick) bool {
	if p.link.Confidence != q.link.Confidence || p.from != q.from || p.to != q.to {
		return p.better(q)
	}
	a, b := p.link, q.link
	if a.Tag != b.Tag {
		return a.Tag < b.Tag
	}
	if a.Topic != b.Topic {
		return a.Topic < b.Topic
	}
	return a.Why < b.Why
}
