package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	graphtrace "github.com/agent-mem/agent-mem/internal/graph/trace"
)

// mountedGraph mounts the real graph routes with a trace recorder.
func mountedGraph(t *testing.T, pool *pgxpool.Pool) http.Handler {
	t.Helper()
	recorder := graphtrace.NewRecorder(discardTraceStore{}, graphtrace.Options{})
	t.Cleanup(func() { recorder.Close(time.Second) })
	r := chi.NewRouter()
	Mount(r, Deps{DB: pool, TraceRecorder: recorder})
	return r
}

func graphGet(h http.Handler, target string, headers map[string]string) *httptest.ResponseRecorder {
	return graphDo(h, http.MethodGet, target, "", headers)
}

func graphDo(h http.Handler, method, target, body string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// hiddenFixture: a hidden Slack root + reply + attachment, linked from a
// public control that is also an epic. Returns the mounted router.
func hiddenFixture(t *testing.T) http.Handler {
	t.Helper()
	pool := openTestDB(t)
	resetEpicACL(t, pool)
	seed := func(id, typ, title, scope, meta string) {
		seedKeywordNode(t, pool, id, title, scope, title+" body")
		aclExec(t, pool, `UPDATE graph.nodes SET type=$2, metadata=$3::jsonb, body=$4 WHERE id=$1`, id, typ, meta, title+" body")
	}
	seed("jira:HCTL-1", "jira", "hidtoken control", "public", `{}`)
	seed("slack:CHID:1.0", "slack", "hidtoken root", "slack:CHID", `{"thread_ts":"1.0"}`)
	seed("slack:CHID:1.1", "slack", "hidtoken reply", "slack:CHID", `{"thread_ts":"1.0"}`)
	seed("slack_file:FHID", "slack_file", "hidtoken file", "", `{}`)
	aclExec(t, pool, `INSERT INTO graph.edges (from_node_id,to_node_id,kind,metadata,machine_id) VALUES
		('jira:HCTL-1','slack:CHID:1.0','REFERENCES','{"why":"hidwhy secret"}','test'),
		('slack:CHID:1.0','slack_file:FHID','REFERENCES','{}','test')`)
	aclMember(t, pool, "jira:HCTL-1", "HCTL-1", viaEpicSelf)
	for _, id := range []string{"slack:CHID:1.0", "slack:CHID:1.1", "slack_file:FHID"} {
		aclMember(t, pool, id, "HCTL-1", viaKey)
	}
	aclPerson(t, pool, 5201, "outsider@example.com")
	aclPerson(t, pool, 5202, "insider@example.com", "slack:CHID")
	return mountedGraph(t, pool)
}

func hiddenRoutes() []struct{ name, method, target, body string } {
	return []struct{ name, method, target, body string }{
		{"search_legacy", "GET", "/api/graph/search?q=hidtoken&limit=50", ""},
		{"search_hybrid", "GET", "/api/graph/search?q=hidtoken&limit=50&match=hybrid", ""},
		{"resolve_control", "POST", "/api/graph/resolve", `{"seeds":["jira:HCTL-1"],"depth":2,"include_bodies":true}`},
		{"resolve_hidden_seed", "POST", "/api/graph/resolve", `{"seeds":["slack:CHID:1.0"],"depth":2,"include_bodies":true}`},
		{"node_root", "GET", "/api/graph/node?id=" + url.QueryEscape("slack:CHID:1.0"), ""},
		{"node_reply", "GET", "/api/graph/node?id=" + url.QueryEscape("slack:CHID:1.1"), ""},
		{"node_file", "GET", "/api/graph/node?id=" + url.QueryEscape("slack_file:FHID"), ""},
		{"node_control_edges", "GET", "/api/graph/node?id=" + url.QueryEscape("jira:HCTL-1"), ""},
		{"neighbors", "GET", "/api/graph/node/" + url.PathEscape("jira:HCTL-1") + "/neighbors?depth=2", ""},
		{"neighbors_cards", "GET", "/api/graph/node/" + url.PathEscape("jira:HCTL-1") + "/neighbors?depth=2&cards=1", ""},
		{"neighbors_hidden_root", "GET", "/api/graph/node/" + url.PathEscape("slack:CHID:1.0") + "/neighbors?depth=2", ""},
		{"epic", "GET", "/api/graph/epic/HCTL-1", ""},
	}
}

var hiddenNeedles = []string{"slack:CHID", "hidtoken root", "hidtoken reply", "FHID", "hidtoken file", "hidwhy"}

func TestACLRoutes_NonMemberSeesNoHiddenContent(t *testing.T) {
	h := hiddenFixture(t)
	for _, rt := range hiddenRoutes() {
		t.Run(rt.name, func(t *testing.T) {
			w := graphDo(h, rt.method, rt.target, rt.body, map[string]string{"X-Asker-User": "outsider@example.com", "Content-Type": "application/json"})
			if strings.HasPrefix(rt.name, "node_") && rt.name != "node_control_edges" || rt.name == "neighbors_hidden_root" {
				if w.Code != http.StatusNotFound {
					t.Fatalf("status=%d want 404 body=%s", w.Code, w.Body.String())
				}
			} else if w.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			body := w.Body.String()
			if rt.name == "resolve_hidden_seed" {
				// graph_trace.seeds echoes the id the caller itself sent.
				if !strings.Contains(body, `"artifacts":[]`) {
					t.Errorf("hidden seed returned artifacts: %s", body)
				}
				body = strings.ReplaceAll(body, `"seeds":["slack:CHID:1.0"]`, "")
			}
			for _, needle := range hiddenNeedles {
				if strings.Contains(body, needle) {
					t.Errorf("leaked %q: %s", needle, w.Body.String())
				}
			}
		})
	}
}

func TestACLRoutes_MemberAndAdminSeeHiddenContent(t *testing.T) {
	h := hiddenFixture(t)
	want := map[string]string{
		"search_legacy": "slack:CHID:1.0", "search_hybrid": "slack:CHID:1.0", "resolve_control": "slack:CHID:1.0",
		"resolve_hidden_seed": "slack:CHID:1.0", "node_root": "slack:CHID:1.0", "node_reply": "slack:CHID:1.1",
		"node_file": "slack_file:FHID", "node_control_edges": "slack:CHID:1.0", "neighbors": "slack:CHID:1.0",
		"neighbors_cards": "slack:CHID:1.0", "neighbors_hidden_root": "jira:HCTL-1", "epic": "slack:CHID:1.0",
	}
	for _, who := range []struct {
		name    string
		headers map[string]string
	}{
		{"member", map[string]string{"X-Asker-User": "insider@example.com"}},
		{"admin", map[string]string{adminHeader: "1"}},
	} {
		for _, rt := range hiddenRoutes() {
			t.Run(who.name+"/"+rt.name, func(t *testing.T) {
				hd := map[string]string{"Content-Type": "application/json"}
				for k, v := range who.headers {
					hd[k] = v
				}
				w := graphDo(h, rt.method, rt.target, rt.body, hd)
				if w.Code != http.StatusOK {
					t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
				}
				if !strings.Contains(w.Body.String(), want[rt.name]) {
					t.Errorf("missing %s: %s", want[rt.name], w.Body.String())
				}
			})
		}
	}
}

// Org scopes (D1): every employee with an eeid sees jira / github / confluence.
func TestACLRoutes_OrgScopes(t *testing.T) {
	pool := openTestDB(t)
	resetEpicACL(t, pool)
	scopes := map[string]string{
		"jira:ORG-BARE": "jira", "jira:ORG-PAY": "jira:PAY", "jira:ORG-GH": "github:wego/payments", "jira:ORG-CF": "confluence:X",
		"jira:ORG-GWS": "gws", "jira:ORG-EVIL": "jiraevil:X", "jira:ORG-DASH": "jira-x",
		"jira:ORG-SLK": "slack:CNONMEMBER", "jira:ORG-DM": "slack:D1",
	}
	for id, scope := range scopes {
		seedKeywordNode(t, pool, id, "orgtoken "+id, scope, "orgtoken "+id)
	}
	for _, n := range []struct{ id, typ, scope string }{{"jira_attachment:ORGA", "jira_attachment", ""}, {"slack_file:ORGS", "slack_file", ""}} {
		seedKeywordNode(t, pool, n.id, "orgtoken "+n.id, n.scope, "orgtoken "+n.id)
		aclExec(t, pool, `UPDATE graph.nodes SET type=$2 WHERE id=$1`, n.id, n.typ)
	}
	aclExec(t, pool, `INSERT INTO graph.edges (from_node_id,to_node_id,kind,metadata,machine_id) VALUES
		('jira:ORG-PAY','jira_attachment:ORGA','REFERENCES','{}','test'),
		('jira:ORG-SLK','slack_file:ORGS','REFERENCES','{}','test')`)
	aclPerson(t, pool, 5301, "orgasker@example.com")
	h := mountedGraph(t, pool)
	visible := map[string]bool{
		"jira:ORG-BARE": true, "jira:ORG-PAY": true, "jira:ORG-GH": true, "jira:ORG-CF": true,
		"jira:ORG-GWS": false, "jira:ORG-EVIL": false, "jira:ORG-DASH": false, "jira:ORG-SLK": false, "jira:ORG-DM": false,
		"jira_attachment:ORGA": true, "slack_file:ORGS": false,
	}
	asker := map[string]string{"X-Asker-User": "orgasker@example.com"}
	searchHits := map[string]bool{}
	var resp searchResponse
	w := graphGet(h, "/api/graph/search?q=orgtoken&limit=50", asker)
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &resp) != nil {
		t.Fatalf("search status=%d body=%s", w.Code, w.Body.String())
	}
	for _, r := range resp.Results {
		searchHits[r.NodeID] = true
	}
	for id, want := range visible {
		code := graphGet(h, "/api/graph/node?id="+url.QueryEscape(id), asker).Code
		if (code == 200) != want {
			t.Errorf("/node %s status=%d want visible=%v", id, code, want)
		}
		if searchHits[id] != want {
			t.Errorf("/search %s hit=%v want %v", id, searchHits[id], want)
		}
	}
}

// The Go predicate and the SQL predicate must agree on every scope.
func TestACLScopePredicatesAgree(t *testing.T) {
	pool := openTestDB(t)
	resetEpicACL(t, pool)
	scopes := []string{"", "public", "jira", "jira:PAY", "jira:", "github", "github:wego/payments", "confluence", "confluence:X",
		"gws", "jiraevil:X", "jira-x", "jiraX", "Jira:PAY", ":jira", "github-enterprise:o/r", "slack:C1", "slack:D1", "slack:CMEM", "gws:file1"}
	member := map[string]bool{"public": true, "slack:CMEM": true}
	arg := (askerACL{resolved: true, scopes: member}).scopeArg()
	check := func(scope string, got bool, what string) {
		t.Helper()
		var sp *string
		if scope != "" {
			sp = &scope
		}
		if want := scopeVisible(sp, member, false); got != want {
			t.Errorf("%s scope %q: SQL=%v Go=%v", what, scope, got, want)
		}
	}
	for i, scope := range scopes {
		id, att := "pred:"+string(rune('a'+i)), "jira_attachment:pred"+string(rune('a'+i))
		seedKeywordNode(t, pool, id, id, scope, "")
		seedKeywordNode(t, pool, att, att, "", "")
		aclExec(t, pool, `UPDATE graph.nodes SET type='jira_attachment' WHERE id=$1`, att)
		aclExec(t, pool, `INSERT INTO graph.edges (from_node_id,to_node_id,kind,metadata,machine_id) VALUES ($1,$2,'REFERENCES','{}','test')`, id, att)

		var direct bool
		if err := pool.QueryRow(context.Background(), `SELECT `+aclVisibleSQL("n", 1, 2)+` FROM graph.nodes n WHERE n.id=$3`, arg, true, id).Scan(&direct); err != nil {
			t.Fatal(err)
		}
		check(scope, direct, "aclVisibleSQL")
		// An attachment is visible exactly when its only parent is.
		var viaParent bool
		if err := pool.QueryRow(context.Background(), `SELECT `+aclVisibleSQL("n", 1, 2)+` FROM graph.nodes n WHERE n.id=$3`, arg, true, att).Scan(&viaParent); err != nil {
			t.Fatal(err)
		}
		check(scope, viaParent, "parentVisibleSQL(attachment)")
		node, err := nodeVisible(context.Background(), pool, askerACL{resolved: true, scopes: member}, att, "jira_attachment", nil)
		if err != nil {
			t.Fatal(err)
		}
		check(scope, node, "nodeVisible(attachment)")
	}
}

func TestAskerFilteredGETPath(t *testing.T) {
	for path, want := range map[string]bool{
		"/api/graph/search":                          true,
		"/api/graph/node":                            true,
		"/api/graph/epic/PAY-1":                      true,
		"/api/graph/node/jira%3AX/neighbors":         true,
		"/api/graph/node/jira%3AX%2Fy/neighbors":     true,
		"/api/graph/node//neighbors":                 false,
		"/api/graph/epic/":                           false,
		"/api/graph/epic/a/b":                        false,
		"/api/graph/node/x/subject-queries":          false,
		"/api/graph/node/x/y/neighbors":              false,
		"/api/graph/search/":                         false,
		"/api/graph/search-weights":                  false,
		"/api/graph/jobs":                            false,
		"/api/graph/":                                false,
		"/api/graph/resolve":                         false, // POST route; a GET to it is not a filtered read
		"/api/graphx/search":                         false,
		"/api/graph/nodes":                           false,
		"/api/graph/node/neighbors":                  false,
		"/api/graph/epic":                            false,
		"/api/graph/node/x/neighbors/more":           false,
		"/api/graph/SEARCH":                          false,
		"/api/graph/channel/topics":                  false,
		"/api/graph/node/%2e%2e/neighbors/../search": false,
	} {
		if got := askerFilteredGET(path); got != want {
			t.Errorf("askerFilteredGET(%q)=%v want %v", path, got, want)
		}
	}
}

var filteredGETPatterns = map[string]bool{
	"/api/graph/search":              true,
	"/api/graph/node":                true,
	"/api/graph/node/{id}/neighbors": true,
	"/api/graph/epic/{key}":          true,
}

// Every admin-only GET named by the plan.
var adminOnlyGETs = []string{
	"/api/graph/jobs", "/api/graph/topic-rules", "/api/graph/person", "/api/graph/traces/stats", "/api/graph/slack-users",
	"/api/graph/slack-user", "/api/graph/channels", "/api/graph/channels/recent", "/api/graph/channel", "/api/graph/channel/topics",
	"/api/graph/cluster/summary", "/api/graph/continents", "/api/graph/channel-filters", "/api/graph/eligibility-gate",
	"/api/graph/business-root", "/api/graph/temporal-timezone", "/api/graph/boost-alphas", "/api/graph/epic-briefs",
	"/api/graph/purpose", "/api/graph/slack-members", "/api/graph/jira-updates", "/api/graph/gh-key-search",
	"/api/graph/subscriptions", "/api/graph/pins", "/api/graph/pins/board", "/api/graph/node/{id}/subject-queries",
}

// Walks the mounted router: every GET under /api/graph/ is either one of the
// asker-filtered routes or admin-gated. A GET added later fails this test
// unless it is gated by default.
func TestACLRoutes_EveryGETIsFilteredOrAdminGated(t *testing.T) {
	pool := openTestDB(t)
	resetEpicACL(t, pool)
	aclPerson(t, pool, 5401, "walker@example.com")
	h := mountedGraph(t, pool)
	var gets []string
	err := chi.Walk(h.(chi.Routes), func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		route = strings.TrimSuffix(route, "/")
		if method == http.MethodGet && strings.HasPrefix(route, "/api/graph/") {
			gets = append(gets, route)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(gets)
	seen := map[string]bool{}
	for _, p := range gets {
		seen[p] = true
	}
	for p := range filteredGETPatterns {
		if !seen[p] {
			t.Errorf("filtered route %s not mounted (walked: %v)", p, gets)
		}
	}
	for _, p := range adminOnlyGETs {
		if !seen[p] {
			t.Errorf("admin route %s not mounted (walked: %v)", p, gets)
		}
	}
	sample := func(p string) string {
		p = strings.ReplaceAll(p, "{id}", "x")
		return strings.ReplaceAll(p, "{key}", "PAY-1")
	}
	for _, pattern := range gets {
		path := sample(pattern)
		t.Run(pattern, func(t *testing.T) {
			if filteredGETPatterns[pattern] {
				if !askerFilteredGET(path) {
					t.Fatalf("policy does not treat %s as filtered", path)
				}
				// No asker: the resolver (not the admin gate) answers.
				if w := graphGet(h, path, nil); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "asker_required") {
					t.Errorf("no asker: status=%d body=%s", w.Code, w.Body.String())
				}
				return
			}
			if askerFilteredGET(path) {
				t.Fatalf("policy treats unlisted route %s as filtered", path)
			}
			w := graphGet(h, path, nil)
			if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "admin_required") {
				t.Errorf("without admin: status=%d body=%s", w.Code, w.Body.String())
			}
			// A named asker is not admin.
			if w := graphGet(h, path, map[string]string{"X-Asker-User": "walker@example.com"}); w.Code != http.StatusForbidden {
				t.Errorf("asker without admin: status=%d", w.Code)
			}
			for _, bad := range []string{"true", "0", "yes", ""} {
				if w := graphGet(h, path, map[string]string{adminHeader: bad}); w.Code != http.StatusForbidden {
					t.Errorf("admin=%q: status=%d", bad, w.Code)
				}
			}
			if w := graphGet(h, path, map[string]string{adminHeader: "1"}); w.Code == http.StatusForbidden {
				t.Errorf("with admin: status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
	// An escaped node id still routes to the filtered neighbors handler.
	w := graphGet(h, "/api/graph/node/jira%3AX%2Fy/neighbors", map[string]string{"X-Asker-User": "walker@example.com"})
	if w.Code == http.StatusForbidden {
		t.Errorf("escaped neighbors path hit the admin gate: %s", w.Body.String())
	}
	// Non-GET methods are not part of the read policy.
	if w := graphDo(h, http.MethodPost, "/api/graph/search-weights", "{}", nil); w.Code == http.StatusForbidden && strings.Contains(w.Body.String(), "admin_required") {
		t.Errorf("POST was gated as a read: %s", w.Body.String())
	}
}

func TestACLTraces_AdminAndRejectedModes(t *testing.T) {
	pool := openTestDB(t)
	resetEpicACL(t, pool)
	seedKeywordNode(t, pool, "jira:TR-1", "trtoken public", "public", "trtoken public")
	aclExec(t, pool, `INSERT INTO graph.people (eeid, display_name, email, slack_user_id, machine_id) VALUES (5501,'Tracer','tracer@example.com','UTRACER','test'),(5502,'Other','other@example.com','UOTHER','test')`)
	search, err := NewSearch(pool)
	if err != nil {
		t.Fatal(err)
	}
	resolve, err := NewResolve(pool)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, route, method, target, body string
		headers                           map[string]string
		status                            int
		mode                              string
		wantSlack                         string
		items                             bool
	}{
		{"search_admin", "search", "GET", "/api/graph/search?q=trtoken", "", map[string]string{adminHeader: "1"}, 200, "admin", "", true},
		{"search_no_asker", "search", "GET", "/api/graph/search?q=trtoken", "", nil, 403, "rejected", "", false},
		{"search_unknown", "search", "GET", "/api/graph/search?q=trtoken", "", map[string]string{"X-Asker-User": "nobody@example.com"}, 403, "rejected", "", false},
		{"resolve_admin", "resolve", "POST", "/api/graph/resolve", `{"seeds":["jira:TR-1"]}`, map[string]string{adminHeader: "1"}, 200, "admin", "", true},
		{"resolve_slack_id", "resolve", "POST", "/api/graph/resolve", `{"seeds":["jira:TR-1"],"asker_slack_id":"UTRACER"}`, nil, 200, "filtered", "UTRACER", true},
		{"resolve_unknown_slack_id", "resolve", "POST", "/api/graph/resolve", `{"seeds":["jira:TR-1"],"asker_slack_id":"UNOBODY"}`, nil, 403, "rejected", "UNOBODY", false},
		{"resolve_conflict", "resolve", "POST", "/api/graph/resolve", `{"seeds":["jira:TR-1"],"asker_slack_id":"UTRACER","asker_eeid":5502}`, nil, 400, "rejected", "UTRACER", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &captureTraceStore{}
			recorder := graphtrace.NewRecorder(store, graphtrace.Options{})
			var next http.Handler = search
			if tc.route == "resolve" {
				next = resolve
			}
			r := httptest.NewRequest(tc.method, tc.target, bytes.NewBufferString(tc.body))
			for k, v := range tc.headers {
				r.Header.Set(k, v)
			}
			w := httptest.NewRecorder()
			withContextTrace(next, tc.route, "rev-test", recorder).ServeHTTP(w, r)
			recorder.Close(time.Second)
			rec := store.one(t)
			if w.Code != tc.status || rec.Status != tc.status || rec.ACLMode != tc.mode {
				t.Fatalf("status=%d trace status=%d acl_mode=%q want %d/%s body=%s", w.Code, rec.Status, rec.ACLMode, tc.status, tc.mode, w.Body.String())
			}
			items, _ := rec.Items.([]map[string]any)
			if tc.items && len(items) == 0 {
				t.Errorf("expected items for %s", tc.name)
			}
			if !tc.items && len(items) != 0 {
				t.Errorf("rejected trace has items: %v", items)
			}
			asker, _ := json.Marshal(rec.AskerInput)
			var decoded map[string]any
			if err := json.Unmarshal(asker, &decoded); err != nil {
				t.Fatal(err)
			}
			if tc.route == "resolve" {
				got, _ := decoded["body_asker_slack_id"].(string)
				if got != tc.wantSlack {
					t.Errorf("body_asker_slack_id=%q want %q (%s)", got, tc.wantSlack, asker)
				}
			}
		})
	}
}
