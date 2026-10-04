package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
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
	if header != "" {
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
	}{
		{"absent", "", true},
		{"unknown", "nobody@example.com", false},
		{"null_eeid", "null@example.com", false},
		{"member", "member@example.com", true},
		{"trimmed_member", " member@example.com ", true},
		{"whitespace", " \t ", true},
		{"no_memberships", "plain@example.com", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
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

func TestAskerScopeSet_ErrorsFailClosed(t *testing.T) {
	pool := openTestDB(t)
	seedAskerFixture(t, pool)
	closed, err := pgxpool.New(context.Background(), pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	closed.Close()
	t.Run("lookup_error", func(t *testing.T) {
		if _, err := lookupAsker(context.Background(), closed, "member@example.com"); err == nil {
			t.Fatal("expected lookup error")
		}
		eeid, scopes, noFilter := askerScopeSet(context.Background(), closed, acl.NewBuilder(pool, time.Minute), "member@example.com")
		assertAskerClosed(t, eeid, scopes, noFilter, 0)
	})
	t.Run("acl_build_error", func(t *testing.T) {
		// Lookup uses the healthy pool; only the builder's database is closed.
		eeid, scopes, noFilter := askerScopeSet(context.Background(), pool, acl.NewBuilder(closed, time.Minute), "member@example.com")
		assertAskerClosed(t, eeid, scopes, noFilter, 71)
	})
	t.Run("lookup_results", func(t *testing.T) {
		for _, tc := range []struct {
			ref  string
			eeid int
		}{
			{"", 0}, {" \t ", 0}, {"nobody@example.com", 0},
			{"null@example.com", 0}, {" member@example.com ", 71},
		} {
			eeid, err := lookupAsker(context.Background(), pool, tc.ref)
			if err != nil || eeid != tc.eeid {
				t.Errorf("lookup %q: eeid=%d err=%v, want %d", tc.ref, eeid, err, tc.eeid)
			}
		}
	})
}

func assertAskerClosed(t *testing.T, eeid int, scopes map[string]bool, noFilter bool, wantEEID int) {
	t.Helper()
	if eeid != wantEEID || noFilter || !reflect.DeepEqual(scopes, map[string]bool{"public": true}) {
		t.Fatalf("eeid=%d scopes=%v noFilter=%v", eeid, scopes, noFilter)
	}
	for _, scope := range []*string{nil, stringPtr(""), stringPtr("public")} {
		if !scopeVisible(scope, scopes, noFilter) {
			t.Error("public/unscoped denied")
		}
	}
	if scopeVisible(stringPtr(aclPriv), scopes, noFilter) {
		t.Error("private visible")
	}
}

func TestAskerUnresolved_NoMetadataLeak(t *testing.T) {
	pool := openTestDB(t)
	h := seedAskerFixture(t, pool)
	for _, edge := range [][2]string{{askerPublic, askerPrivate}, {askerPrivate, askerPublic}, {askerPublic, askerUnscoped}, {askerUnscoped, askerPublic}} {
		aclExec(t, pool, `INSERT INTO graph.edges (from_node_id,to_node_id,kind,metadata,machine_id) VALUES ($1,$2,'REFERENCES','{"why":"private-root explanation","topic":"private topic"}','test')`, edge[0], edge[1])
	}
	t.Run("node_edges", func(t *testing.T) {
		for _, header := range []string{"nobody@example.com", ""} {
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
		w := askerRequest(t, h, path, "nobody@example.com")
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
		// Keep expansion and ACL building healthy: ignoring the root-query
		// error must reach a 200 response, not another database failure.
		handler := &neighborsHandler{
			db:     closedPool,
			exp:    bfs.NewExpander(pool),
			aclBld: acl.NewBuilder(pool, 5*time.Minute),
		}
		r := chi.NewRouter()
		r.Get("/api/graph/node/{id}/neighbors", handler.serve)
		// A nonempty header keeps the root guard enabled even when the
		// identity lookup fails on the closed pool.
		w := askerRequest(t, r, "/api/graph/node/"+url.QueryEscape(askerRoot)+"/neighbors?kind=REFERENCES", "nobody@example.com")
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("root query error: status=%d body=%s", w.Code, w.Body.String())
		}
	})
}

func stringPtr(s string) *string { return &s }
