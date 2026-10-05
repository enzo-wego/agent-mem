package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/agent-mem/agent-mem/internal/gemini"
	"github.com/agent-mem/agent-mem/internal/graph/acl"
	"github.com/agent-mem/agent-mem/internal/graph/bfs"
	"github.com/agent-mem/agent-mem/internal/graph/scoring"
	"github.com/agent-mem/agent-mem/internal/graph/temporal"
)

// Embedder is the minimal interface Search needs to embed a query string.
// *gemini.Client satisfies it via its Embed method.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
	EmbedWithOptions(ctx context.Context, text string, opts gemini.EmbedOptions) ([]float32, error)
}

// Search handles GET /api/graph/search: four retrieval arms (semantic,
// keyword, graph, temporal) run concurrently, are fused by reciprocal rank,
// then boosted by recency / team / temporal proximity / authority.
type Search struct {
	db     *pgxpool.Pool
	embed  Embedder
	aclBld *acl.Builder
	exp    *bfs.Expander
	// now is injectable so the temporal window parser is testable.
	now func() time.Time
}

// NewSearch creates a Search handler. embed may be nil: the semantic and
// graph arms are then skipped and reported in arm_errors.
func NewSearch(db *pgxpool.Pool) (*Search, error) {
	return &Search{
		db:     db,
		embed:  nil, // wired at server level via NewSearchWithEmbedder
		aclBld: acl.NewBuilder(db, 5*time.Minute),
		exp:    bfs.NewExpander(db),
		now:    time.Now,
	}, nil
}

// NewSearchWithEmbedder is used when a real Embedder is available.
func NewSearchWithEmbedder(db *pgxpool.Pool, embed Embedder) (*Search, error) {
	s, err := NewSearch(db)
	if err != nil {
		return nil, err
	}
	s.embed = embed
	return s, nil
}

type searchResult struct {
	NodeID         string             `json:"node_id"`
	ID             string             `json:"id"` // alias of node_id; the dashboard reads `id`
	Type           string             `json:"type"`
	Title          string             `json:"title"`
	URL            string             `json:"url"`
	Summary        string             `json:"summary"`
	Score          float64            `json:"score"`
	ScoreBreakdown scoring.Components `json:"score_breakdown"`
	Author         string             `json:"author,omitempty"`
	CreatedAt      time.Time          `json:"created_at"`
}

type searchWindow struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
	Hard  bool      `json:"hard,omitempty"`
}

type searchResponse struct {
	Results []searchResult `json:"results"`
	Total   int            `json:"total"`
	// Arms that contributed a list to fusion, in fixed order.
	Arms []string `json:"arms"`
	// ArmErrors maps an arm that was requested but produced nothing usable to
	// the reason (query error, no embedder…). Never fails the request.
	ArmErrors map[string]string `json:"arm_errors,omitempty"`
	// Window is the parsed or explicit time window the temporal arm used.
	Window *searchWindow `json:"window,omitempty"`
	// Query is the text the semantic/keyword arms saw (time phrase removed).
	Query string `json:"query"`
}

// Result limits per mode. ?limit is clamped to the mode's maximum; absent or
// non-positive falls back to its default. Per-arm budgets are limit*3.
const (
	defaultSearchLimit = 10
	maxSearchLimit     = 50
	hybridSearchLimit  = 50
	maxHybridLimit     = 100
)

func (s *Search) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	qv := r.URL.Query()
	q := strings.TrimSpace(qv.Get("q"))
	if q == "" {
		http.Error(w, "q required", http.StatusBadRequest)
		return
	}
	hybrid := qv.Get("match") == "hybrid"
	limit, maxLimit := defaultSearchLimit, maxSearchLimit
	if hybrid {
		limit, maxLimit = hybridSearchLimit, maxHybridLimit
	}
	if n, _ := strconv.Atoi(qv.Get("limit")); n > 0 {
		limit = min(n, maxLimit)
	}
	budget := limit * 3
	epicKeys, err := epicScopeFromQuery(qv)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	wanted, err := parseArms(qv.Get("arms"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if hybrid {
		if qv.Get("arms") != "" {
			http.Error(w, "match=hybrid and arms are exclusive", http.StatusBadRequest)
			return
		}
		wanted = map[string]bool{armSemantic: true, armKeyword: true}
	}

	// Time window: explicit since/until win; otherwise a phrase in q.
	now := s.now()
	win, rest, hasWindow, err := s.window(qv, q, now, temporalLocation(ctx, s.db), !hybrid)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// An absent header is the trusted unfiltered view; a present header is
	// always filtered, with unresolved askers limited to public/unscoped nodes.
	// The API key remains the privilege boundary; identity is advisory.
	askerEEID, scopeSet, noFilter := askerScopeSet(ctx, s.db, s.aclBld, r.Header.Get("X-Asker-User"))
	acl := askerACL{noFilter: noFilter, resolved: askerEEID != 0, scopes: scopeSet}
	scopeArg := acl.scopeArg()
	var typesArg any
	if t := splitCSV(qv.Get("types")); len(t) > 0 {
		typesArg = t
	}
	filter := searchFilter{types: typesArg, scope: scopeArg, epic: epicScopeArg(epicKeys), resolved: acl.resolved}

	// One embedding of the topic (time phrase removed) serves the semantic
	// and temporal arms. A query that was only a time phrase has no topic.
	var vec []float32
	armErrs := map[string]string{}
	if s.embed == nil {
		armErrs[armSemantic] = "no embedder configured"
	} else if rest == "" {
		armErrs[armSemantic] = "query is only a time phrase"
	} else {
		v, err := s.embed.EmbedWithOptions(ctx, rest, graphEmbeddingOptions())
		if err != nil {
			armErrs[armSemantic] = "embed failed: " + err.Error()
		} else {
			vec = v
		}
	}
	if rest == "" {
		armErrs[armKeyword] = "query is only a time phrase"
	}
	if !hasWindow {
		armErrs[armTemporal] = "no time window in query or since/until"
	}

	// Run the arms. Semantic → graph is the one true dependency (graph seeds
	// are the top semantic hits), so graph runs in the semantic goroutine.
	// Skip decisions are final before any goroutine starts; goroutines only
	// write through record (under mu).
	if wanted[armGraph] && (armErrs[armSemantic] != "" || !wanted[armSemantic]) {
		armErrs[armGraph] = "needs the semantic arm for seeds"
	}
	var (
		mu    sync.Mutex
		wg    sync.WaitGroup
		lists = map[string][]armHit{}
	)
	record := func(arm string, hits []armHit, err error) {
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			armErrs[arm] = err.Error()
			return
		}
		lists[arm] = hits
	}
	runSemantic := wanted[armSemantic] && armErrs[armSemantic] == ""
	runGraph := wanted[armGraph] && armErrs[armGraph] == ""
	runKeyword := wanted[armKeyword] && armErrs[armKeyword] == ""
	runTemporal := wanted[armTemporal] && armErrs[armTemporal] == ""
	run := func(arm string, fn func() ([]armHit, error)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			hits, err := fn()
			record(arm, hits, err)
		}()
	}
	if runSemantic {
		run(armSemantic, func() ([]armHit, error) {
			arm := semanticArm
			if hybrid {
				arm = semanticArmFolded
			}
			hits, err := arm(ctx, s.db, vec, filter, budget)
			if err != nil {
				if runGraph {
					record(armGraph, nil, fmt.Errorf("semantic seeds unavailable: %w", err))
				}
				return nil, err
			}
			if runGraph {
				g, gerr := graphArm(ctx, s.db, s.exp, hits)
				record(armGraph, g, gerr)
			}
			return hits, nil
		})
	}
	if runKeyword {
		run(armKeyword, func() ([]armHit, error) {
			if hybrid {
				return keywordArmFolded(ctx, s.db, rest, filter, budget)
			}
			return keywordArm(ctx, s.db, rest, filter, budget)
		})
	}
	if runTemporal {
		run(armTemporal, func() ([]armHit, error) {
			return temporalArm(ctx, s.db, s.exp, win, vec, filter)
		})
	}
	wg.Wait()
	if hybrid {
		// arm_errors covers only the requested arms. A missing embedder is a
		// skip, not a failure; if every non-skipped arm failed, fail the request.
		active, failed := 0, 0
		var msgs []string
		for arm := range wanted {
			if !wanted[arm] {
				continue
			}
			msg, bad := armErrs[arm]
			if bad && msg == "no embedder configured" {
				continue
			}
			active++
			if bad {
				failed++
				msgs = append(msgs, arm+": "+msg)
			}
		}
		if active > 0 && failed == active {
			sort.Strings(msgs)
			http.Error(w, "search failed: "+strings.Join(msgs, "; "), http.StatusInternalServerError)
			return
		}
		for arm := range armErrs {
			if !wanted[arm] {
				delete(armErrs, arm)
			}
		}
		if err := canonicalizeThreads(ctx, s.db, lists, filter); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	// Fuse by rank, then load node metadata for the fused set (applying the
	// filter once more so graph-arm neighbours obey type/ACL/epic scope).
	ranked := make(map[string][]string, len(lists))
	armLocal := make(map[string]map[string]float64, len(lists))
	for arm, hits := range lists {
		ids := make([]string, len(hits))
		local := make(map[string]float64, len(hits))
		for i, h := range hits {
			ids[i] = h.ID
			local[h.ID] = h.Score
		}
		ranked[arm] = ids
		armLocal[arm] = local
	}
	fused := scoring.Fuse(ranked, scoring.RRFK)
	ids := make([]string, 0, len(fused))
	for id := range fused {
		ids = append(ids, id)
	}
	alphas, err := scoring.LoadBoostAlphas(ctx, s.db)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	results, err := s.hydrateResults(ctx, ids, filter, fused, armLocal, alphas, askerEEID, win, hasWindow, now)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sortByScore(results)
	hardWindow := hasWindow && explicitWindow(qv)
	if hardWindow {
		ids = ids[:0]
		for _, result := range results {
			ids = append(ids, result.NodeID)
		}
		eligible, err := nodesEligibleInWindow(ctx, s.db, ids, win)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		kept := results[:0]
		for _, result := range results {
			if eligible[result.NodeID] {
				kept = append(kept, result)
			}
		}
		results = kept
	}
	results, err = s.pinOwnKey(ctx, q, results, filter, fused, armLocal, alphas, askerEEID, win, hasWindow, now)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if len(results) > limit {
		results = results[:limit]
	}

	if hybrid {
		rows, err := hybridDisplay(ctx, s.db, results, acl)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		resp := hybridResponse{Results: rows, Total: len(rows), Arms: []string{}, Query: rest,
			ArmErrors: nilIfEmpty(armErrs), SemanticError: armErrs[armSemantic]}
		for _, arm := range allArms {
			if _, ok := lists[arm]; ok {
				resp.Arms = append(resp.Arms, arm)
			}
		}
		if hasWindow {
			resp.Window = &searchWindow{Start: win.Start, End: win.End, Hard: hardWindow}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
		return
	}

	resp := searchResponse{Results: results, Total: len(results), Arms: []string{}, Query: rest}
	for _, arm := range allArms {
		if _, ok := lists[arm]; ok {
			resp.Arms = append(resp.Arms, arm)
		}
	}
	if len(armErrs) > 0 {
		resp.ArmErrors = armErrs
	}
	if hasWindow {
		resp.Window = &searchWindow{Start: win.Start, End: win.End, Hard: hardWindow}
	}
	if resp.Results == nil {
		resp.Results = []searchResult{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func explicitWindow(qv url.Values) bool {
	return strings.TrimSpace(qv.Get("since")) != "" || strings.TrimSpace(qv.Get("until")) != ""
}

func nilIfEmpty(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	return m
}

// hydrateResults loads the fused ids in one query and computes the boosted
// score for each. Rows the filter rejects (graph-arm neighbours outside the
// requested types/scope/epic) are dropped here.
func (s *Search) hydrateResults(ctx context.Context, ids []string, f searchFilter,
	fused map[string]scoring.Fused, armLocal map[string]map[string]float64,
	alphas scoring.BoostAlphas, askerEEID int, win temporal.Window, hasWindow bool, now time.Time) ([]searchResult, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := s.db.Query(ctx, `
SELECT n.id, n.type, COALESCE(n.title,''), COALESCE(n.url,''),
       COALESCE(ai.summary,''),
       COALESCE(p.display_name,''),
       n.updated_at,
       COALESCE(n.created_at, n.first_seen_at) AS created_at,
       COALESCE(p.depth_from_root, 0),
       COALESCE(p.eeid, 0)
FROM graph.nodes n
LEFT JOIN graph.artifact_index ai ON ai.node_id = n.id
LEFT JOIN graph.people p ON p.id = n.author_person_id
WHERE n.id = ANY($1)
  AND `+f.sql(2, 3, 4), append([]any{ids}, f.args()...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type hydrated struct {
		id, typ, title, url, summary, authorName string
		updatedAt, createdAt                     time.Time
		depth                                    int16
		authorEEID                               int
	}
	var loaded []hydrated
	var authors []int
	for rows.Next() {
		var h hydrated
		if err := rows.Scan(&h.id, &h.typ, &h.title, &h.url, &h.summary, &h.authorName,
			&h.updatedAt, &h.createdAt, &h.depth, &h.authorEEID); err != nil {
			return nil, err
		}
		loaded = append(loaded, h)
		authors = append(authors, h.authorEEID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	team := teamScoresForSearch(ctx, s.db, askerEEID, authors)

	results := make([]searchResult, 0, len(loaded))
	for _, h := range loaded {
		fz := fused[h.id]
		c := scoring.Components{
			Sem:      scoring.Semantic(armLocal[armSemantic][h.id]),
			Rec:      scoring.Recency(h.updatedAt, now, 30*24*time.Hour),
			Edge:     armLocal[armGraph][h.id],
			Team:     team[h.authorEEID],
			Auth:     scoring.Authority(h.depth, 6),
			Temporal: 0.5,
			RRF:      fz.Score,
			Ranks:    fz.Ranks,
		}
		if hasWindow {
			c.Temporal = win.Proximity(h.createdAt)
		}
		results = append(results, searchResult{
			NodeID: h.id, ID: h.id, Type: h.typ, Title: h.title, URL: h.url,
			Summary:        h.summary,
			Score:          scoring.Boost(fz.Score, alphas, c.Rec, c.Team, c.Temporal, c.Auth),
			ScoreBreakdown: c,
			Author:         h.authorName, CreatedAt: h.createdAt,
		})
	}
	return results, nil
}

// window resolves the temporal window: explicit ?since/?until (RFC3339 or
// YYYY-MM-DD; a date-only `until` is inclusive) override a phrase parsed out
// of q. rest is q with the phrase removed (unchanged when since/until set).
func (s *Search) window(qv map[string][]string, q string, now time.Time, loc *time.Location, parseQ bool) (temporal.Window, string, bool, error) {
	get := func(k string) string {
		if v, ok := qv[k]; ok && len(v) > 0 {
			return strings.TrimSpace(v[0])
		}
		return ""
	}
	since, until := get("since"), get("until")
	if since == "" && until == "" {
		if !parseQ {
			return temporal.Window{}, q, false, nil
		}
		w, rest, ok := temporal.Parse(q, now, loc)
		if !ok {
			return temporal.Window{}, q, false, nil
		}
		return w, rest, true, nil
	}
	// One bound only leaves the other a placeholder: the window is open.
	win := temporal.Window{Start: time.Unix(0, 0).UTC(), End: now.Add(24 * time.Hour), Open: since == "" || until == ""}
	if since != "" {
		t, _, err := parseWhen(since, loc)
		if err != nil {
			return temporal.Window{}, q, false, fmt.Errorf("since: %w", err)
		}
		win.Start = t
	}
	if until != "" {
		t, dateOnly, err := parseWhen(until, loc)
		if err != nil {
			return temporal.Window{}, q, false, fmt.Errorf("until: %w", err)
		}
		if dateOnly {
			t = t.AddDate(0, 0, 1)
		}
		win.End = t
	}
	if !win.End.After(win.Start) {
		return temporal.Window{}, q, false, fmt.Errorf("until must be after since")
	}
	return win, q, true, nil
}

// parseWhen accepts RFC3339 (keeps its own offset) or YYYY-MM-DD (midnight in
// loc); dateOnly reports the latter.
func parseWhen(s string, loc *time.Location) (t time.Time, dateOnly bool, err error) {
	if t, err = time.Parse(time.RFC3339, s); err == nil {
		return t, false, nil
	}
	if t, err = time.ParseInLocation("2006-01-02", s, loc); err == nil {
		return t, true, nil
	}
	return time.Time{}, false, fmt.Errorf("want RFC3339 or YYYY-MM-DD, got %q", s)
}

// parseArms turns ?arms=semantic,keyword into the enabled set; empty = all.
func parseArms(csv string) (map[string]bool, error) {
	out := make(map[string]bool, len(allArms))
	names := splitCSV(csv)
	if len(names) == 0 {
		names = allArms
	}
	for _, n := range names {
		n = strings.ToLower(n)
		known := false
		for _, a := range allArms {
			if a == n {
				known = true
				break
			}
		}
		if !known {
			return nil, fmt.Errorf("unknown arm %q (want %s)", n, strings.Join(allArms, ","))
		}
		out[n] = true
	}
	return out, nil
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// personScoreForSearch is the simplified person scoring used in /search
// where there's no asker thread to anchor against.
func personScoreForSearch(ctx context.Context, db *pgxpool.Pool, asker, author int) float64 {
	if asker == 0 || author == 0 {
		return 0.1
	}
	if asker == author {
		return 1.0
	}
	if shareTeamGroup(ctx, db, asker, author) {
		return 0.9
	}
	if shareDeptGroup(ctx, db, asker, author) {
		return 0.7
	}
	d, _ := scoring.LookupDistance(ctx, db, asker, author)
	switch {
	case d <= 2:
		return 0.4
	case d <= 4:
		return 0.25
	default:
		return 0.1
	}
}

// teamScoresForSearch returns personScoreForSearch(asker, author) for every
// distinct author eeid in one query, instead of up to four per row. A zero
// asker or author scores 0.1 and the asker themself 1.0, both decided in Go;
// team/department overlap and org distance come from a single join.
func teamScoresForSearch(ctx context.Context, db *pgxpool.Pool, asker int, authors []int) map[int]float64 {
	out := make(map[int]float64, len(authors))
	var others []int32
	for _, a := range authors {
		if _, done := out[a]; done {
			continue
		}
		switch {
		case asker == 0 || a == 0:
			out[a] = 0.1
		case asker == a:
			out[a] = 1.0
		default:
			out[a] = 0.1 // default for an author the lookup cannot place
			others = append(others, int32(a))
		}
	}
	if len(others) == 0 {
		return out
	}
	rows, err := db.Query(ctx, `
SELECT au.eeid,
       COALESCE(ac.team_group_ids && bc.team_group_ids, false),
       COALESCE(ac.dept_group_ids && bc.dept_group_ids, false),
       pd.hops
FROM unnest($2::int[]) AS au(eeid)
LEFT JOIN graph.user_affinity_config ac ON ac.eeid = $1
LEFT JOIN graph.user_affinity_config bc ON bc.eeid = au.eeid
LEFT JOIN graph.person_distance pd
  ON pd.a_eeid = LEAST($1::int, au.eeid) AND pd.b_eeid = GREATEST($1::int, au.eeid)`, int32(asker), others)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var eeid int32
		var team, dept bool
		var hops *int32
		if err := rows.Scan(&eeid, &team, &dept, &hops); err != nil {
			return out
		}
		switch {
		case team:
			out[int(eeid)] = 0.9
		case dept:
			out[int(eeid)] = 0.7
		case hops == nil: // unknown pair: scoring.LookupDistance reports MaxInt32
			out[int(eeid)] = 0.1
		case *hops <= 2:
			out[int(eeid)] = 0.4
		case *hops <= 4:
			out[int(eeid)] = 0.25
		default:
			out[int(eeid)] = 0.1
		}
	}
	return out
}

// shareTeamGroup returns true if both eeids are in at least one common
// team-level group (team_group_ids from user_affinity_config).
func shareTeamGroup(ctx context.Context, db *pgxpool.Pool, asker, author int) bool {
	row := db.QueryRow(ctx, `
SELECT EXISTS (
  SELECT 1 FROM graph.user_affinity_config a
  JOIN graph.user_affinity_config b ON b.eeid = $2
  WHERE a.eeid = $1
    AND a.team_group_ids && b.team_group_ids
)`, asker, author)
	var ok bool
	row.Scan(&ok)
	return ok
}

// shareDeptGroup returns true if both eeids are in at least one common
// dept-level group (dept_group_ids from user_affinity_config).
func shareDeptGroup(ctx context.Context, db *pgxpool.Pool, asker, author int) bool {
	row := db.QueryRow(ctx, `
SELECT EXISTS (
  SELECT 1 FROM graph.user_affinity_config a
  JOIN graph.user_affinity_config b ON b.eeid = $2
  WHERE a.eeid = $1
    AND a.dept_group_ids && b.dept_group_ids
)`, asker, author)
	var ok bool
	row.Scan(&ok)
	return ok
}

// sortByScore orders results by score descending, breaking ties by created_at
// descending (newest first) so equally-relevant results surface the most recent.
func sortByScore(rs []searchResult) {
	sort.SliceStable(rs, func(i, j int) bool {
		if rs[i].Score != rs[j].Score {
			return rs[i].Score > rs[j].Score
		}
		return rs[i].CreatedAt.After(rs[j].CreatedAt)
	})
}

// lookupAsker resolves a trimmed X-Asker-User identity to an eeid.
// Missing people and NULL eeids resolve to zero; database errors are preserved.
func lookupAsker(ctx context.Context, db *pgxpool.Pool, ref string) (int, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return 0, nil
	}
	row := db.QueryRow(ctx, `
SELECT COALESCE(eeid, 0) FROM graph.people
WHERE slack_user_id = $1 OR email = $1 OR github_login = $1 OR jira_account_id = $1
LIMIT 1`, ref)
	var eeid int
	if err := row.Scan(&eeid); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil
		}
		return 0, err
	}
	return eeid, nil
}
