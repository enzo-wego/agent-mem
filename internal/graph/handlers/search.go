package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"

	"github.com/agent-mem/agent-mem/internal/gemini"
	"github.com/agent-mem/agent-mem/internal/graph/acl"
	"github.com/agent-mem/agent-mem/internal/graph/scoring"
)

// Embedder is the minimal interface Search needs to embed a query string.
// *gemini.Client satisfies it via its Embed method.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
	EmbedWithOptions(ctx context.Context, text string, opts gemini.EmbedOptions) ([]float32, error)
}

// Search handles GET /api/graph/search.
type Search struct {
	db      *pgxpool.Pool
	embed   Embedder
	aclBld  *acl.Builder
	weights scoring.Weights
}

// NewSearch creates a Search handler. embed may be nil (semantic scoring
// will be skipped and only recency/team/authority used).
func NewSearch(db *pgxpool.Pool) (*Search, error) {
	w, err := scoring.LoadWeights(context.Background(), db)
	if err != nil {
		return nil, err
	}
	return &Search{
		db:      db,
		embed:   nil, // wired at server level via NewSearchWithEmbedder
		aclBld:  acl.NewBuilder(db, 5*time.Minute),
		weights: w,
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

func (s *Search) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query().Get("q")
	if q == "" {
		http.Error(w, "q required", http.StatusBadRequest)
		return
	}
	typesFilter := splitCSV(r.URL.Query().Get("types"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 50 {
		limit = 10
	}

	// noFilter when no asker principal is asserted (eeid 0 = the trusted
	// dashboard/integration calling behind the API key). A real asker
	// (eeid != 0) is always filtered: even with zero memberships they see only
	// "public" (plus their scopes), never the whole graph. (The API key is the
	// privilege boundary; asker identity is advisory until authenticated.)
	askerEEID := lookupAskerEEID(ctx, s.db, r.Header.Get("X-Asker-User"))
	var scopeArg any
	if askerEEID != 0 {
		scopes, _ := s.aclBld.For(ctx, askerEEID)
		scopeArg = append(scopes, "public")
	}

	if r.URL.Query().Get("match") == "hybrid" {
		s.serveHybrid(w, r, q, typesFilter, scopeArg, askerEEID)
		return
	}

	// Embed the query if an embedder is available.
	var queryVec []float32
	if s.embed != nil {
		v, err := s.embed.EmbedWithOptions(ctx, q, graphEmbeddingOptions())
		if err != nil {
			http.Error(w, "embed failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		queryVec = v
	}

	var rows interface {
		Next() bool
		Scan(...any) error
		Close()
		Err() error
	}
	var err error

	if queryVec != nil {
		// Semantic search: order by vector cosine distance.
		const query = `
SELECT n.id, n.type, COALESCE(n.title,''), COALESCE(n.url,''),
       COALESCE(ai.summary,''),
       COALESCE(p.display_name,''),
       1.0 - (ai.embedding <=> $1) AS cosine,
       n.updated_at,
       COALESCE(n.created_at, n.first_seen_at) AS created_at,
       COALESCE(p.depth_from_root, 0),
       COALESCE(p.is_bot, false),
       COALESCE(p.eeid, 0)
FROM graph.artifact_index ai
JOIN graph.nodes n ON n.id = ai.node_id
LEFT JOIN graph.people p ON p.id = n.author_person_id
WHERE n.deleted_at IS NULL
  AND ai.embedding IS NOT NULL
  AND ($2::text[] IS NULL OR n.type = ANY($2))
  AND ($3::text[] IS NULL OR n.scope IS NULL OR n.scope = '' OR n.scope = ANY($3))
ORDER BY ai.embedding <=> $1
LIMIT $4
`
		var typesArg any
		if len(typesFilter) > 0 {
			typesArg = typesFilter
		}
		rows, err = s.db.Query(ctx, query,
			pgvector.NewVector(queryVec),
			typesArg, scopeArg, limit*3)
	} else {
		// Keyword/title search fallback when no embedder.
		const query = `
SELECT n.id, n.type, COALESCE(n.title,''), COALESCE(n.url,''),
       COALESCE(ai.summary,''),
       COALESCE(p.display_name,''),
       0.5 AS cosine,
       n.updated_at,
       COALESCE(n.created_at, n.first_seen_at) AS created_at,
       COALESCE(p.depth_from_root, 0),
       COALESCE(p.is_bot, false),
       COALESCE(p.eeid, 0)
FROM graph.nodes n
LEFT JOIN graph.artifact_index ai ON ai.node_id = n.id
LEFT JOIN graph.people p ON p.id = n.author_person_id
WHERE n.deleted_at IS NULL
  AND ($1::text[] IS NULL OR n.type = ANY($1))
  AND ($2::text[] IS NULL OR n.scope IS NULL OR n.scope = '' OR n.scope = ANY($2))
  AND (n.title ILIKE '%' || $3 || '%' OR n.body ILIKE '%' || $3 || '%')
ORDER BY n.updated_at DESC
LIMIT $4
`
		var typesArg any
		if len(typesFilter) > 0 {
			typesArg = typesFilter
		}
		rows, err = s.db.Query(ctx, query, typesArg, scopeArg, q, limit*3)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	now := time.Now()
	var results []searchResult
	for rows.Next() {
		var (
			id, typ, title, url, summary, authorName string
			cosine                                   float64
			updatedAt                                time.Time
			createdAt                                time.Time
			depth                                    int16
			isBot                                    bool
			authorEEID                               int
		)
		if err := rows.Scan(&id, &typ, &title, &url, &summary, &authorName,
			&cosine, &updatedAt, &createdAt, &depth, &isBot, &authorEEID); err != nil {
			continue
		}
		c := scoring.Components{
			Sem:  scoring.Semantic(cosine),
			Rec:  scoring.Recency(updatedAt, now, 30*24*time.Hour),
			Edge: 0, // /search has no graph context — leave 0
			Team: personScoreForSearch(ctx, s.db, askerEEID, authorEEID),
			Auth: scoring.Authority(depth, 6),
		}
		score := scoring.Combine(s.weights, c)
		results = append(results, searchResult{
			NodeID: id, ID: id, Type: typ, Title: title, URL: url,
			Summary: summary, Score: score, ScoreBreakdown: c,
			Author: authorName, CreatedAt: createdAt,
		})
	}
	if rows.Err() != nil {
		http.Error(w, rows.Err().Error(), http.StatusInternalServerError)
		return
	}

	// Re-rank by combined score, return top `limit`.
	sortByScore(results)
	if len(results) > limit {
		results = results[:limit]
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"results": results,
		"total":   len(results),
	})
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

// lookupAskerEEID resolves the X-Asker-User header (slack uid or email)
// to an eeid by joining graph.people. Returns 0 if not found.
func lookupAskerEEID(ctx context.Context, db *pgxpool.Pool, ref string) int {
	if ref == "" {
		return 0
	}
	row := db.QueryRow(ctx, `
SELECT COALESCE(eeid, 0) FROM graph.people
WHERE slack_user_id = $1 OR email = $1 OR github_login = $1 OR jira_account_id = $1
LIMIT 1`, ref)
	var eeid int
	row.Scan(&eeid)
	return eeid
}

// hybridResult is the /search?match=hybrid result row. It is a separate struct
// so default-mode JSON keeps exactly today's key set.
type hybridResult struct {
	NodeID           string             `json:"node_id"`
	ID               string             `json:"id"`
	Type             string             `json:"type"`
	Title            string             `json:"title"`
	URL              string             `json:"url"`
	Summary          string             `json:"summary"`
	Score            float64            `json:"score"`
	ScoreBreakdown   scoring.Components `json:"score_breakdown"`
	Author           string             `json:"author,omitempty"`
	CreatedAt        time.Time          `json:"created_at"`
	Match            []string           `json:"match"`
	ThreadRoot       string             `json:"thread_root,omitempty"`
	Channel          string             `json:"channel,omitempty"`
	RootAuthor       string             `json:"root_author,omitempty"`
	MsgCount         int                `json:"msg_count,omitempty"`
	Participants     []string           `json:"participants,omitempty"`
	ParticipantCount int                `json:"participant_count,omitempty"`
	FirstTSMs        int64              `json:"first_ts_ms,omitempty"`
	LastTSMs         int64              `json:"last_ts_ms,omitempty"`
}

// hybridHit is one raw row from either side of the hybrid query.
type hybridHit struct {
	id, typ, title, url, summary, author, scope, threadTS, body string
	cosine                                                      float64
	updatedAt, createdAt                                        time.Time
	depth                                                       int16
	isBot                                                       bool
	eeid                                                        int
}

const hybridCols = `n.id, n.type, COALESCE(n.title,''), COALESCE(n.url,''),
       COALESCE(ai.summary,''),
       COALESCE(p.display_name,''),
       %s AS cosine,
       n.updated_at,
       COALESCE(n.created_at, n.first_seen_at) AS created_at,
       COALESCE(p.depth_from_root, 0),
       COALESCE(p.is_bot, false),
       COALESCE(p.eeid, 0),
       COALESCE(n.scope,''), COALESCE(n.metadata->>'thread_ts',''), LEFT(COALESCE(n.body,''),200)`

// hybridKeywordRegexp builds the word-boundary keyword pattern: \m / \M are
// added only when the query starts / ends with a word character, so "PAN" does
// not match "company" but "#payments" and "c++" still match.
func hybridKeywordRegexp(q string) string {
	q = strings.TrimSpace(q)
	if q == "" {
		return ""
	}
	isWord := func(r rune) bool {
		return r == '_' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
	}
	rs := []rune(q)
	kw := regexp.QuoteMeta(q)
	if isWord(rs[0]) {
		kw = `\m` + kw
	}
	if isWord(rs[len(rs)-1]) {
		kw += `\M`
	}
	return kw
}

func scanHybridHits(rows interface {
	Next() bool
	Scan(...any) error
	Close()
	Err() error
}) ([]hybridHit, error) {
	defer rows.Close()
	var hits []hybridHit
	for rows.Next() {
		var h hybridHit
		if err := rows.Scan(&h.id, &h.typ, &h.title, &h.url, &h.summary, &h.author,
			&h.cosine, &h.updatedAt, &h.createdAt, &h.depth, &h.isBot, &h.eeid,
			&h.scope, &h.threadTS, &h.body); err != nil {
			continue
		}
		hits = append(hits, h)
	}
	return hits, rows.Err()
}

func isSlackType(t string) bool { return t == "slack" || t == "slack_thread" }

// hybridRootID maps a Slack hit to its thread root id (channel id from scope).
func hybridRootID(h hybridHit) string {
	if !isSlackType(h.typ) || !strings.HasPrefix(h.scope, "slack:") {
		return ""
	}
	ts := h.threadTS
	if ts == "" {
		parts := strings.Split(h.id, ":")
		if len(parts) != 3 {
			return ""
		}
		ts = parts[2]
	}
	return "slack:" + strings.TrimPrefix(h.scope, "slack:") + ":" + ts
}

// serveHybrid is the opt-in keyword+semantic search used by the /search page.
func (s *Search) serveHybrid(w http.ResponseWriter, r *http.Request, q string, typesFilter []string, scopeArg any, askerEEID int) {
	ctx := r.Context()
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 50
	}
	if limit > 100 {
		limit = 100
	}
	var typesArg any
	if len(typesFilter) > 0 {
		typesArg = typesFilter
	}

	// Semantic side: only with an embedder, and no ILIKE fallback if it fails.
	var semHits []hybridHit
	semanticErr := ""
	if s.embed != nil {
		vec, err := s.embed.EmbedWithOptions(ctx, q, graphEmbeddingOptions())
		if err != nil {
			semanticErr = err.Error()
		} else {
			rows, err := s.db.Query(ctx, `
SELECT `+strings.Replace(hybridCols, "%s", "1.0 - (ai.embedding <=> $1)", 1)+`
FROM graph.artifact_index ai
JOIN graph.nodes n ON n.id = ai.node_id
LEFT JOIN graph.people p ON p.id = n.author_person_id
WHERE n.deleted_at IS NULL
  AND ai.embedding IS NOT NULL
  AND ($2::text[] IS NULL OR n.type = ANY($2))
  AND ($3::text[] IS NULL OR n.scope IS NULL OR n.scope = '' OR n.scope = ANY($3))
ORDER BY ai.embedding <=> $1
LIMIT $4`, pgvector.NewVector(vec), typesArg, scopeArg, limit*3)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			semHits, err = scanHybridHits(rows)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
	}

	// Keyword side.
	// ponytail: unindexed word-boundary regex over graph.nodes title/body.
	// Measured in prod 2026-10-02: 49,762 nodes / 140 MB body = 0.47-0.94 s.
	// Ceiling: when this exceeds ~2 s, add a pg_trgm GIN index on n.body/n.title.
	var kwHits []hybridHit
	if kw := hybridKeywordRegexp(q); kw != "" {
		rows, err := s.db.Query(ctx, `
SELECT `+strings.Replace(hybridCols, "%s", "0.5", 1)+`
FROM graph.nodes n
LEFT JOIN graph.artifact_index ai ON ai.node_id = n.id
LEFT JOIN graph.people p ON p.id = n.author_person_id
WHERE n.deleted_at IS NULL
  AND ($1::text[] IS NULL OR n.type = ANY($1))
  AND ($2::text[] IS NULL OR n.scope IS NULL OR n.scope = '' OR n.scope = ANY($2))
  AND (n.title ~* $3 OR n.body ~* $3)
ORDER BY COALESCE(n.created_at, n.first_seen_at) DESC
LIMIT 200`, typesArg, scopeArg, kw)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		kwHits, err = scanHybridHits(rows)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}

	// Fold Slack hits into their thread root when the root exists and is visible.
	rootSet := map[string]bool{}
	var rootIDs []string
	for _, hs := range [][]hybridHit{semHits, kwHits} {
		for _, h := range hs {
			if rid := hybridRootID(h); rid != "" && !rootSet[rid] {
				rootSet[rid] = true
				rootIDs = append(rootIDs, rid)
			}
		}
	}
	roots := map[string]hybridHit{}
	if len(rootIDs) > 0 {
		rows, err := s.db.Query(ctx, `
SELECT `+strings.Replace(hybridCols, "%s", "0.5", 1)+`
FROM graph.nodes n
LEFT JOIN graph.artifact_index ai ON ai.node_id = n.id
LEFT JOIN graph.people p ON p.id = n.author_person_id
WHERE n.id = ANY($1) AND n.deleted_at IS NULL
  AND ($2::text[] IS NULL OR n.scope IS NULL OR n.scope = '' OR n.scope = ANY($2))`,
			rootIDs, scopeArg)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		rh, err := scanHybridHits(rows)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		for _, h := range rh {
			roots[h.id] = h
		}
	}

	type merged struct {
		base    hybridHit // the root node when folded, else the hit itself
		rootID  string    // thread root id for Slack results
		kw, sem bool
		maxCos  float64
	}
	byKey := map[string]*merged{}
	var order []*merged
	add := func(h hybridHit, sem bool) {
		key := h.id
		base := h
		rid := hybridRootID(h)
		if rid != "" {
			if root, ok := roots[rid]; ok {
				key, base = rid, root
			}
		}
		m := byKey[key]
		if m == nil {
			m = &merged{base: base, rootID: rid}
			byKey[key] = m
			order = append(order, m)
		}
		if sem {
			m.sem = true
			if h.cosine > m.maxCos {
				m.maxCos = h.cosine
			}
		} else {
			m.kw = true
		}
	}
	for _, h := range semHits {
		add(h, true)
	}
	for _, h := range kwHits {
		add(h, false)
	}

	now := time.Now()
	results := make([]hybridResult, 0, len(order))
	for _, m := range order {
		cos := 0.5
		if m.sem {
			cos = m.maxCos
		}
		b := m.base
		c := scoring.Components{
			Sem:  scoring.Semantic(cos),
			Rec:  scoring.Recency(b.updatedAt, now, 30*24*time.Hour),
			Edge: 0,
			Team: personScoreForSearch(ctx, s.db, askerEEID, b.eeid),
			Auth: scoring.Authority(b.depth, 6),
		}
		var match []string
		if m.kw {
			match = append(match, "keyword")
		}
		if m.sem {
			match = append(match, "semantic")
		}
		title := b.title
		if title == "" {
			title = firstLine(b.body, 120)
		}
		res := hybridResult{
			NodeID: b.id, ID: b.id, Type: b.typ, Title: title, URL: b.url,
			Summary: b.summary, Score: scoring.Combine(s.weights, c), ScoreBreakdown: c,
			Author: b.author, CreatedAt: b.createdAt, Match: match,
		}
		if m.rootID != "" {
			res.ThreadRoot = m.rootID
			if res.URL == "" {
				res.URL = slackPermalink(m.rootID)
			}
		} else {
			ms := b.createdAt.UnixMilli()
			res.FirstTSMs, res.LastTSMs = ms, ms
		}
		results = append(results, res)
	}
	sort.SliceStable(results, func(i, j int) bool {
		if results[i].Score != results[j].Score {
			return results[i].Score > results[j].Score
		}
		return results[i].CreatedAt.After(results[j].CreatedAt)
	})
	if len(results) > limit {
		results = results[:limit]
	}

	// Slack enrichment for the kept results only: one query each.
	var slackRoots []string
	for _, res := range results {
		if res.ThreadRoot != "" {
			slackRoots = append(slackRoots, res.ThreadRoot)
		}
	}
	if len(slackRoots) > 0 {
		cards, err := threadCards(ctx, s.db, slackRoots)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		chans := make([]string, 0, len(slackRoots))
		tss := make([]string, 0, len(slackRoots))
		for _, rid := range slackRoots {
			if c, t, ok := slackRootParts(rid); ok {
				chans, tss = append(chans, c), append(tss, t)
			}
		}
		type tsum struct{ summary, overview string }
		sums := map[string]tsum{}
		srows, err := s.db.Query(ctx, `
SELECT channel_id, thread_ts, summary, overview
FROM graph.thread_summaries
WHERE (channel_id, thread_ts) IN (SELECT unnest($1::text[]), unnest($2::text[]))`, chans, tss)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		for srows.Next() {
			var c, t string
			var ts tsum
			if err := srows.Scan(&c, &t, &ts.summary, &ts.overview); err == nil {
				sums[c+":"+t] = ts
			}
		}
		srows.Close()
		for i := range results {
			rid := results[i].ThreadRoot
			if rid == "" {
				continue
			}
			ch, t, _ := slackRootParts(rid)
			if ts, ok := sums[ch+":"+t]; ok {
				if ts.summary != "" {
					results[i].Title = ts.summary
				}
				if ts.overview != "" {
					results[i].Summary = ts.overview
				}
			}
			if c, ok := cards[rid]; ok {
				results[i].Channel = c.Channel
				results[i].RootAuthor = c.RootAuthor
				results[i].MsgCount = c.MsgCount
				results[i].Participants = c.Participants
				results[i].ParticipantCount = c.ParticipantCount
				results[i].FirstTSMs = c.FirstTSMs
				results[i].LastTSMs = c.LastTSMs
			}
		}
	}

	resp := map[string]any{"results": results, "total": len(results)}
	if semanticErr != "" {
		resp["semantic_error"] = semanticErr
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}
