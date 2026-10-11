package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/agent-mem/agent-mem/internal/graph/acl"
	"github.com/agent-mem/agent-mem/internal/graph/bfs"
)

const (
	askerPublic   = "jira:ACL-PUBLIC"
	askerUnscoped = "jira:ACL-UNSCOPED"
	askerPrivate  = "jira:ACL-PRIVATE"
	askerRoot     = "jira:ACL-ROOT"
)

func seedAskerFixture(t *testing.T, pool *pgxpool.Pool) http.Handler {
	t.Helper()
	resetEpicACL(t, pool)
	for _, n := range []struct{ id, scope string }{
		{askerPublic, "public"}, {askerUnscoped, ""}, {askerPrivate, aclPriv}, {askerRoot, "public"},
	} {
		seedKeywordNode(t, pool, n.id, "aclquokka resource", n.scope, "aclquokka searchable enriched resource")
		aclExec(t, pool, `UPDATE graph.nodes SET body = 'aclquokka fetched resource body', url = 'https://example.com/' || id WHERE id = $1`, n.id)
	}
	for _, id := range []string{askerPublic, askerUnscoped, askerPrivate} {
		aclExec(t, pool, `INSERT INTO graph.edges (from_node_id,to_node_id,kind,metadata,machine_id) VALUES ($1,$2,'REFERENCES','{}','test')`, askerRoot, id)
	}
	aclPerson(t, pool, 71, "member@example.com", aclPriv)
	aclPerson(t, pool, 72, "plain@example.com")
	aclExec(t, pool, `INSERT INTO graph.people (email, display_name, machine_id) VALUES ('null@example.com','No EEID','test')`)
	r := chi.NewRouter()
	r.Method("GET", "/api/graph/node", NewNode(pool))
	search, err := NewSearch(pool)
	if err != nil {
		t.Fatal(err)
	}
	r.Method("GET", "/api/graph/search", search)
	r.Mount("/api/graph", NewNeighbors(pool))
	return r
}

func askerRequest(t *testing.T, h http.Handler, path, header string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, path, nil)
	if header == "" {
		r.Header.Set(adminHeader, "1")
	} else {
		r.Header.Set("X-Asker-User", header)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestAskerUnresolved_FailsClosed(t *testing.T) {
	pool := openTestDB(t)
	h := seedAskerFixture(t, pool)
	for _, tc := range []struct {
		name, header string
		private      bool
		deny         string // non-empty: every route is rejected with this code
	}{
		{"admin", "", true, ""},
		{"unknown", "nobody@example.com", false, "asker_unknown"},
		{"null_eeid", "null@example.com", false, "asker_unknown"},
		{"member", "member@example.com", true, ""},
		{"trimmed_member", " member@example.com ", true, ""},
		{"whitespace", " \t ", false, "asker_required"},
		{"no_memberships", "plain@example.com", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.deny != "" {
				for _, path := range []string{
					"/api/graph/node?id=" + url.QueryEscape(askerPublic),
					"/api/graph/search?q=aclquokka",
					"/api/graph/search?q=aclquokka&match=hybrid",
					"/api/graph/node/" + url.QueryEscape(askerRoot) + "/neighbors",
				} {
					w := askerRequest(t, h, path, tc.header)
					if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), `"`+tc.deny+`"`) {
						t.Errorf("%s: status=%d body=%s, want 403 %s", path, w.Code, w.Body.String(), tc.deny)
					}
				}
				return
			}
			t.Run("node", func(t *testing.T) {
				for _, id := range []string{askerPublic, askerUnscoped, askerPrivate} {
					w := askerRequest(t, h, "/api/graph/node?id="+url.QueryEscape(id), tc.header)
					if id == askerPrivate && !tc.private {
						if w.Code != http.StatusNotFound || w.Body.String() != "not found\n" {
							t.Fatalf("hidden node: status=%d body=%s", w.Code, w.Body.String())
						}
						continue
					}
					if w.Code != http.StatusOK {
						t.Fatalf("%s status=%d body=%s", id, w.Code, w.Body.String())
					}
					var resp nodeResponse
					if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
						t.Fatal(err)
					}
					if resp.NodeID != id {
						t.Fatalf("node=%q want %q", resp.NodeID, id)
					}
				}
			})
			for _, mode := range []string{"neighbors", "search", "search_hybrid"} {
				t.Run(mode, func(t *testing.T) {
					path := "/api/graph/search?q=aclquokka&limit=50"
					if mode == "search_hybrid" {
						path += "&match=hybrid"
					}
					if mode == "neighbors" {
						path = "/api/graph/node/" + url.QueryEscape(askerRoot) + "/neighbors?kind=REFERENCES"
					}
					w := askerRequest(t, h, path, tc.header)
					if w.Code != http.StatusOK {
						t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
					}
					got := map[string]bool{}
					if mode == "neighbors" {
						var resp struct {
							Items []neighborItem `json:"neighbors"`
						}
						if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
							t.Fatal(err)
						}
						for _, item := range resp.Items {
							got[item.Node.NodeID] = true
						}
					} else {
						var resp searchResponse
						if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
							t.Fatal(err)
						}
						for _, item := range resp.Results {
							got[item.NodeID] = true
						}
					}
					for _, id := range []string{askerPublic, askerUnscoped} {
						if !got[id] {
							t.Errorf("missing %s in %v", id, got)
						}
					}
					if got[askerPrivate] != tc.private {
						t.Errorf("private visible=%v want %v; %v", got[askerPrivate], tc.private, got)
					}
				})
			}
		})
	}
}

func TestAskerUnresolved_NoMetadataLeak(t *testing.T) {
	pool := openTestDB(t)
	h := seedAskerFixture(t, pool)
	for _, edge := range [][2]string{{askerPublic, askerPrivate}, {askerPrivate, askerPublic}, {askerPublic, askerUnscoped}, {askerUnscoped, askerPublic}} {
		aclExec(t, pool, `INSERT INTO graph.edges (from_node_id,to_node_id,kind,metadata,machine_id) VALUES ($1,$2,'REFERENCES','{"why":"private-root explanation","topic":"private topic"}','test')`, edge[0], edge[1])
	}
	t.Run("node_edges", func(t *testing.T) {
		for _, header := range []string{"plain@example.com", ""} {
			w := askerRequest(t, h, "/api/graph/node?id="+url.QueryEscape(askerPublic), header)
			if w.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			var resp nodeResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			for _, dir := range []struct {
				name     string
				edges    []edgeRef
				incoming bool
			}{{"in", resp.EdgesIn, true}, {"out", resp.EdgesOut, false}} {
				got := map[string]bool{}
				for _, edge := range dir.edges {
					id := edge.To
					if dir.incoming {
						id = edge.From
					}
					got[id] = true
				}
				if got[askerPrivate] != (header == "") {
					t.Errorf("%s private visibility: %v", dir.name, got)
				}
				for _, id := range []string{askerUnscoped} {
					if !got[id] {
						t.Errorf("%s missing permitted endpoint %s: %v", dir.name, id, got)
					}
				}
			}
		}
	})
	t.Run("neighbors_private_root", func(t *testing.T) {
		path := "/api/graph/node/" + url.QueryEscape(askerPrivate) + "/neighbors?kind=REFERENCES"
		w := askerRequest(t, h, path, "plain@example.com")
		if w.Code != http.StatusNotFound || w.Body.String() != "not found\n" {
			t.Fatalf("hidden root: status=%d body=%s", w.Code, w.Body.String())
		}
		w = askerRequest(t, h, path, "")
		if w.Code != http.StatusOK {
			t.Fatalf("trusted root: status=%d body=%s", w.Code, w.Body.String())
		}
		var resp struct {
			Items []neighborItem `json:"neighbors"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, item := range resp.Items {
			if item.Node.NodeID == askerPublic {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing public neighbor: %s", w.Body.String())
		}
	})
	t.Run("neighbors_root_query_error", func(t *testing.T) {
		closedPool := openTestDB(t)
		closedPool.Close()
		// The asker lookup shares the closed pool, so the request fails closed
		// before the root query: 503, never a 200.
		handler := &neighborsHandler{
			db:     closedPool,
			exp:    bfs.NewExpander(pool),
			aclBld: acl.NewBuilder(pool, 5*time.Minute),
		}
		r := chi.NewRouter()
		r.Get("/api/graph/node/{id}/neighbors", handler.serve)
		w := askerRequest(t, r, "/api/graph/node/"+url.QueryEscape(askerRoot)+"/neighbors?kind=REFERENCES", "nobody@example.com")
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("root query error: status=%d body=%s", w.Code, w.Body.String())
		}
	})
}

func stringPtr(s string) *string { return &s }
