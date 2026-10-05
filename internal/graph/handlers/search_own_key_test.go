package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"
	"github.com/rs/zerolog"

	"github.com/agent-mem/agent-mem/internal/gemini"
	"github.com/agent-mem/agent-mem/internal/graph/scoring"
)

const ownKeyAsker = "own-key-search@example.com"

type ownKeyEmbedder struct{ vec []float32 }

func (e ownKeyEmbedder) Embed(context.Context, string) ([]float32, error) {
	return e.vec, nil
}

func (e ownKeyEmbedder) EmbedWithOptions(context.Context, string, gemini.EmbedOptions) ([]float32, error) {
	return e.vec, nil
}

type ownKeySearchRow struct {
	searchResult
	Match []string `json:"match"`
}

func ownKeyRequest(t *testing.T, s *Search, q, mode, asker string, params url.Values) []ownKeySearchRow {
	t.Helper()
	v := url.Values{"q": {q}, "limit": {"20"}}
	if mode == "hybrid" {
		v.Set("match", mode)
	}
	for k, values := range params {
		v[k] = values
	}
	r := httptest.NewRequest(http.MethodGet, "/api/graph/search?"+v.Encode(), nil)
	if asker != "" {
		r.Header.Set("X-Asker-User", asker)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("search status %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Results   []ownKeySearchRow `json:"results"`
		ArmErrors map[string]string `json:"arm_errors"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	for _, arm := range []string{armKeyword, armSemantic} {
		if err := resp.ArmErrors[arm]; err != "" {
			t.Fatalf("%s arm failed: %s", arm, err)
		}
	}
	return resp.Results
}

func ownKeySameResult(a, b searchResult) bool {
	sameTime := a.CreatedAt.Equal(b.CreatedAt)
	a.CreatedAt, b.CreatedAt = time.Time{}, time.Time{}
	return sameTime && reflect.DeepEqual(a, b)
}

func ownKeyIDs(rows []ownKeySearchRow) []string {
	ids := make([]string, len(rows))
	for i, row := range rows {
		ids[i] = row.NodeID
	}
	return ids
}

// Restore pre-existing settings rather than leaking non-default boosts into
// other search tests, including when this test fails partway through.
func ownKeyBoostSettings(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	rows, err := pool.Query(ctx, `SELECT key, value FROM public.settings WHERE key = ANY($1)`, scoring.BoostAlphaKeys[:])
	if err != nil {
		t.Fatal(err)
	}
	before := map[string]string{}
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		before[key] = value
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		aclExec(t, pool, `DELETE FROM public.settings WHERE key = ANY($1)`, scoring.BoostAlphaKeys[:])
		for key, value := range before {
			aclExec(t, pool, `INSERT INTO public.settings (key, value) VALUES ($1, $2)`, key, value)
		}
	})
	for i, value := range []string{"0.8", "0.8", "0", "0"} {
		aclExec(t, pool, `INSERT INTO public.settings (key, value) VALUES ($1, $2)
			ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`, scoring.BoostAlphaKeys[i], value)
	}
}

func ownKeyFixture(t *testing.T, pool *pgxpool.Pool, owner, key string, indexed bool) *Search {
	t.Helper()
	truncateGraphHandlerTables(t, pool)
	t.Cleanup(func() { truncateGraphHandlerTables(t, pool) })
	ctx := context.Background()
	now := time.Now().UTC()
	var author int64
	if err := pool.QueryRow(ctx, `INSERT INTO graph.people (eeid, display_name, email, machine_id)
		VALUES (9302333, 'Own key asker', $1, 'test') RETURNING id`, ownKeyAsker).Scan(&author); err != nil {
		t.Fatal(err)
	}
	typ := "jira"
	if strings.HasPrefix(owner, "gh_pr:") {
		typ = "gh_pr"
	}
	aclExec(t, pool, `INSERT INTO graph.nodes (id, type, natural_key, title, body, scope, url, metadata,
		created_at, updated_at, machine_id) VALUES ($1, $2, $1, 'Owner refund behavior',
		'Restores idempotent refunds', 'public', 'https://example.com/owner', '{}', $3, $3, 'test')`,
		owner, typ, now.Add(-365*24*time.Hour))
	if indexed {
		aclExec(t, pool, `INSERT INTO graph.artifact_index (node_id, summary, summary_kind, machine_id)
			VALUES ($1, 'Owner refund behavior restores idempotent refunds', 'heuristic', 'test')`, owner)
	}
	for _, n := range []struct{ id, title, summary string }{
		{"jira:SEARCH-90", "Refund summary competitor", strings.Repeat(key+" ", 5) + "refund rollout"},
		{"jira:SEARCH-91", key + " newer title refund rollout", "refund rollout title competitor"},
		{"jira:SEARCH-92", key + " refund rollout both arms", key + " refund rollout semantic competitor"},
	} {
		aclExec(t, pool, `INSERT INTO graph.nodes (id, type, natural_key, title, body, scope, metadata,
			author_person_id, created_at, updated_at, machine_id)
			VALUES ($1, 'jira', $1, $2, 'refund rollout', 'public', '{}', $3, $4, $5, 'test')`,
			n.id, n.title, author, now.Add(-8*24*time.Hour), now.Add(-time.Hour))
		aclExec(t, pool, `INSERT INTO graph.artifact_index (node_id, summary, summary_kind, machine_id)
			VALUES ($1, $2, 'heuristic', 'test')`, n.id, n.summary)
	}
	vec := make([]float32, GraphEmbeddingDims)
	vec[0], vec[1] = 1, 0.01
	aclExec(t, pool, `UPDATE graph.artifact_index SET embedding = $2 WHERE node_id = $1`,
		"jira:SEARCH-92", pgvector.NewVector(vec))
	// Exercise the actual indexing caller. Manually seeding identifiers would
	// let an own-key extraction regression pass the keyword-arm assertion.
	if err := backfillIdentifiersHandler(Deps{DB: pool, Logger: zerolog.Nop()})(ctx, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	queryVec := make([]float32, GraphEmbeddingDims)
	queryVec[0] = 1
	s, err := NewSearchWithEmbedder(pool, ownKeyEmbedder{vec: queryVec})
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return now }
	return s
}

// Reconstruct the pre-precedence path using the real arms, fusion and hydration.
// Comparing against it proves pinning preserves scores, provenance and every
// competitor's relative order, rather than merely checking position one.
func ownKeyUnpinned(t *testing.T, s *Search, q, mode string) []searchResult {
	t.Helper()
	ctx := context.Background()
	win, rest, hasWindow, err := s.window(nil, strings.TrimSpace(q), s.now(), temporalLocation(ctx, s.db), mode != "hybrid")
	if err != nil {
		t.Fatal(err)
	}
	f := searchFilter{scope: []string{"public"}, resolved: true}
	vec, err := s.embed.EmbedWithOptions(ctx, rest, graphEmbeddingOptions())
	if err != nil {
		t.Fatal(err)
	}
	keyword, semantic := keywordArm, semanticArm
	if mode == "hybrid" {
		keyword, semantic = keywordArmFolded, semanticArmFolded
	}
	kh, err := keyword(ctx, s.db, rest, f, 60)
	if err != nil {
		t.Fatal(err)
	}
	sh, err := semantic(ctx, s.db, vec, f, 60)
	if err != nil {
		t.Fatal(err)
	}
	lists := map[string][]armHit{armKeyword: kh, armSemantic: sh}
	if mode == "hybrid" {
		if err := canonicalizeThreads(ctx, s.db, lists, f); err != nil {
			t.Fatal(err)
		}
	}
	ranked := map[string][]string{}
	local := map[string]map[string]float64{}
	for arm, hits := range lists {
		local[arm] = map[string]float64{}
		for _, hit := range hits {
			ranked[arm] = append(ranked[arm], hit.ID)
			local[arm][hit.ID] = hit.Score
		}
	}
	fused := scoring.Fuse(ranked, scoring.RRFK)
	ids := make([]string, 0, len(fused))
	for id := range fused {
		ids = append(ids, id)
	}
	alphas, err := scoring.LoadBoostAlphas(ctx, s.db)
	if err != nil {
		t.Fatal(err)
	}
	results, err := s.hydrateResults(ctx, ids, f, fused, local, alphas, 9302333, win, hasWindow, s.now())
	if err != nil {
		t.Fatal(err)
	}
	sortByScore(results)
	return results
}

func TestSearch_JiraOwnKey(t *testing.T) {
	pool := openTestDB(t)
	ownKeyBoostSettings(t, pool)
	for _, mode := range []string{"default", "hybrid"} {
		t.Run(mode, func(t *testing.T) {
			for _, tc := range []struct {
				name, owner, key string
				queries          []string
			}{
				{"jira", "jira:PAY-2333", "PAY-2333", []string{"PAY-2333", "pay-2333", "  PAY-2333 "}},
				{"pr", "gh_pr:wego/payments#2357", "wego/payments#2357", []string{"wego/payments#2357", "WEGO/Payments#2357"}},
				{"pr_underscore", "gh_pr:wego/pay_ops#12", "wego/pay_ops#12", []string{"wego/pay_ops#12", "WEGO/Pay_Ops#12"}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					s := ownKeyFixture(t, pool, tc.owner, tc.key, true)
					// Independent of the HTTP precedence path: the owner is a
					// keyword candidate despite no own key in title/body/summary.
					hits, err := keywordArm(context.Background(), pool, tc.key, searchFilter{}, 60)
					if err != nil {
						t.Fatal(err)
					}
					found := false
					for _, hit := range hits {
						found = found || hit.ID == tc.owner
					}
					if !found {
						t.Fatalf("owner missing from keyword arm before pinning: %+v", hits)
					}
					for _, q := range tc.queries {
						t.Run(q, func(t *testing.T) {
							baseline := ownKeyUnpinned(t, s, q, mode)
							if len(baseline) < 4 || baseline[0].NodeID != "jira:SEARCH-92" {
								t.Fatalf("fixture must favour both-arms competitor: %+v", baseline)
							}
							competitor := baseline[0]
							if len(competitor.ScoreBreakdown.Ranks) != 2 || competitor.ScoreBreakdown.Team != 1 || competitor.ScoreBreakdown.Rec < 0.9 {
								t.Fatalf("competitor lacks both arms / recency / team advantage: %+v", competitor)
							}
							rows := ownKeyRequest(t, s, q, mode, ownKeyAsker, nil)
							wantIDs := []string{tc.owner}
							for _, old := range baseline {
								if old.NodeID != tc.owner {
									wantIDs = append(wantIDs, old.NodeID)
								}
							}
							if got := ownKeyIDs(rows); !reflect.DeepEqual(got, wantIDs) {
								t.Fatalf("query %q ids %v, want %v", q, got, wantIDs)
							}
							for _, row := range rows {
								for _, old := range baseline {
									if row.NodeID == old.NodeID && !ownKeySameResult(row.searchResult, old) {
										t.Fatalf("pin changed score or metadata: got %+v, before %+v", row.searchResult, old)
									}
								}
							}
							if mode == "hybrid" && !reflect.DeepEqual(rows[0].Match, []string{armKeyword}) {
								t.Fatalf("owner match invented an arm: %v", rows[0].Match)
							}
							limited := ownKeyRequest(t, s, q, mode, ownKeyAsker, url.Values{"limit": {"1"}})
							if len(limited) != 1 || limited[0].NodeID != tc.owner {
								t.Fatalf("pin must precede truncation: %+v", limited)
							}
						})
					}
				})
			}
			t.Run("missing_index", func(t *testing.T) {
				s := ownKeyFixture(t, pool, "jira:PAY-2333", "PAY-2333", false)
				rows := ownKeyRequest(t, s, "PAY-2333", mode, ownKeyAsker, nil)
				if len(rows) != 4 || rows[0].NodeID != "jira:PAY-2333" {
					t.Fatalf("missing-index owner not hydrated: %+v", rows)
				}
				owner := rows[0]
				if owner.Score != 0 || owner.ScoreBreakdown.RRF != 0 || len(owner.ScoreBreakdown.Ranks) != 0 || len(owner.Match) != 0 {
					t.Fatalf("fallback invented retrieval provenance: %+v", owner)
				}
				if owner.Title != "Owner refund behavior" || owner.URL != "https://example.com/owner" || owner.Type != "jira" || owner.ID != owner.NodeID || owner.CreatedAt.IsZero() {
					t.Fatalf("fallback skipped existing metadata hydration: %+v", owner)
				}
			})
			for _, excluded := range []string{"acl_unknown_private", "types", "epic", "deleted", "missing_node"} {
				t.Run(excluded, func(t *testing.T) {
					s := ownKeyFixture(t, pool, "jira:PAY-2333", "PAY-2333", false)
					params := url.Values{}
					asker := ownKeyAsker
					switch excluded {
					case "acl_unknown_private":
						// No jira_attachment nodes: do not trigger the known
						// attachment duplicate-row baseline failure.
						aclExec(t, pool, `UPDATE graph.nodes SET scope = 'slack:OWNKEYPRIVATE' WHERE id = 'jira:PAY-2333'`)
						asker = "unknown-own-key@example.com"
					case "types":
						aclExec(t, pool, `UPDATE graph.nodes SET type = 'gh_pr' WHERE id LIKE 'jira:SEARCH-%'`)
						params.Set("types", "gh_pr")
					case "epic":
						aclMember(t, pool, "jira:PAY-2333", "PAY-8000", "key")
						for _, id := range []string{"jira:SEARCH-90", "jira:SEARCH-91", "jira:SEARCH-92"} {
							aclMember(t, pool, id, "PAY-8001", "key")
						}
						params.Set("epic", "PAY-8001")
					case "deleted":
						aclExec(t, pool, `UPDATE graph.nodes SET deleted_at = now() WHERE id = 'jira:PAY-2333'`)
					case "missing_node":
						aclExec(t, pool, `DELETE FROM graph.nodes WHERE id = 'jira:PAY-2333'`)
					}
					rows := ownKeyRequest(t, s, "PAY-2333", mode, asker, params)
					if len(rows) != 3 {
						t.Fatalf("eligible competitors must remain: %+v", rows)
					}
					for _, row := range rows {
						if row.NodeID == "jira:PAY-2333" {
							t.Fatalf("ineligible owner pinned: %+v", row)
						}
					}
				})
			}
			t.Run("nonexact_ranking", func(t *testing.T) {
				s := ownKeyFixture(t, pool, "jira:PAY-2333", "PAY-2333", true)
				for _, q := range []string{"PAY-2333 last week", "refund", "PAY-2333 rollout", "xPAY-2333y", "PAY-2333!"} {
					baseline := ownKeyUnpinned(t, s, q, mode)
					params := url.Values{}
					if mode == "default" {
						params.Set("arms", "keyword,semantic")
					}
					rows := ownKeyRequest(t, s, q, mode, ownKeyAsker, params)
					if len(rows) != len(baseline) {
						t.Fatalf("nonexact query %q changed result count: got %+v, before %+v", q, rows, baseline)
					}
					for i, row := range rows {
						if !ownKeySameResult(row.searchResult, baseline[i]) {
							t.Fatalf("nonexact query %q changed ranking: got %+v, before %+v", q, rows, baseline)
						}
					}
				}
			})
		})
	}
}
