package handlers

import (
	"bytes"
	"context"
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

// resolverFixture seeds the people the resolver table needs and one public
// node both /search and /resolve can return.
func resolverFixture(t *testing.T) (*pgxpool.Pool, http.Handler) {
	t.Helper()
	pool := openTestDB(t)
	resetEpicACL(t, pool)
	seedKeywordNode(t, pool, "jira:RES-1", "resquokka public", "public", "resquokka public body")
	person := func(eeid any, email, slack string) {
		aclExec(t, pool, `INSERT INTO graph.people (eeid, display_name, email, slack_user_id, machine_id) VALUES ($1, $2::text, $2::text, NULLIF($3,''), 'test')`, eeid, email, slack)
	}
	person(5101, "alice@example.com", "U5101")
	person(5102, "bob@example.com", "U5102")
	person(nil, "noeeid@example.com", "UNULL")
	// merge chain: UMERGED1 -> 5103 (1 hop).
	person(5103, "target@example.com", "UTARGET")
	aclExec(t, pool, `INSERT INTO graph.people (eeid, display_name, slack_user_id, merged_into, machine_id)
		SELECT NULL, 'merged dup', 'UMERGED1', id, 'test' FROM graph.people WHERE eeid = 5103`)
	// loop: ULOOP1 <-> ULOOP2.
	aclExec(t, pool, `INSERT INTO graph.people (display_name, slack_user_id, machine_id) VALUES ('loop a','ULOOP1','test'),('loop b','ULOOP2','test')`)
	aclExec(t, pool, `UPDATE graph.people SET merged_into = (SELECT id FROM graph.people WHERE slack_user_id='ULOOP2') WHERE slack_user_id='ULOOP1'`)
	aclExec(t, pool, `UPDATE graph.people SET merged_into = (SELECT id FROM graph.people WHERE slack_user_id='ULOOP1') WHERE slack_user_id='ULOOP2'`)
	// chain of 4 hops ending at a valid eeid: more than maxMergeHops.
	person(5104, "end@example.com", "UEND")
	prev := "UEND"
	for _, name := range []string{"UCHAIN4", "UCHAIN3", "UCHAIN2", "UCHAIN1"} {
		aclExec(t, pool, `INSERT INTO graph.people (display_name, slack_user_id, merged_into, machine_id)
			SELECT 'chain', $1, id, 'test' FROM graph.people WHERE slack_user_id = $2`, name, prev)
		prev = name
	}
	// missing merge target: FK normally prevents it, so bypass it in one tx.
	aclExec(t, pool, `INSERT INTO graph.people (display_name, slack_user_id, machine_id) VALUES ('gone target','UGONE','test')`)
	aclExec(t, pool, `INSERT INTO graph.people (display_name, slack_user_id, merged_into, machine_id)
		SELECT 'orphan', 'UORPHAN', id, 'test' FROM graph.people WHERE slack_user_id = 'UGONE'`)
	tx, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(context.Background(), `SET LOCAL session_replication_role = replica`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(context.Background(), `DELETE FROM graph.people WHERE slack_user_id = 'UGONE'`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}

	search, err := NewSearch(pool)
	if err != nil {
		t.Fatal(err)
	}
	resolve, err := NewResolve(pool)
	if err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	r.Method("GET", "/api/graph/search", search)
	r.Method("POST", "/api/graph/resolve", resolve)
	return pool, r
}

type resolverCase struct {
	name    string
	headers map[string]string
	body    map[string]any // /resolve only
	status  int
	code    string // error code for non-200
}

func callResolver(t *testing.T, h http.Handler, route string, tc resolverCase) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if route == "search" {
		r = httptest.NewRequest(http.MethodGet, "/api/graph/search?q=resquokka", nil)
	} else {
		body := map[string]any{"seeds": []string{"jira:RES-1"}, "depth": 1}
		for k, v := range tc.body {
			body[k] = v
		}
		b, _ := json.Marshal(body)
		r = httptest.NewRequest(http.MethodPost, "/api/graph/resolve", bytes.NewReader(b))
	}
	for k, v := range tc.headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func assertResolverCase(t *testing.T, w *httptest.ResponseRecorder, tc resolverCase) {
	t.Helper()
	if w.Code != tc.status {
		t.Fatalf("status=%d want %d body=%s", w.Code, tc.status, w.Body.String())
	}
	if tc.code != "" && !strings.Contains(w.Body.String(), `"`+tc.code+`"`) {
		t.Fatalf("body=%s want code %s", w.Body.String(), tc.code)
	}
}

func TestAskerResolver_StepsTable(t *testing.T) {
	_, h := resolverFixture(t)
	for _, tc := range []resolverCase{
		// 1 admin
		{name: "admin_header_only", headers: map[string]string{adminHeader: "1"}, status: 200},
		{name: "admin_conflicting_identity_still_admin", headers: map[string]string{adminHeader: "1", "X-Asker-User": "alice@example.com"},
			body: map[string]any{"asker_slack_id": "U5102", "asker_eeid": 5101}, status: 200},
		{name: "admin_unknown_identity_not_checked", headers: map[string]string{adminHeader: "1", "X-Asker-User": "nobody@example.com"}, status: 200},
		// admin header must be exactly "1"
		{name: "admin_true_is_not_admin", headers: map[string]string{adminHeader: "true"}, status: 403, code: "asker_required"},
		{name: "admin_zero_is_not_admin", headers: map[string]string{adminHeader: "0"}, status: 403, code: "asker_required"},
		{name: "admin_value_with_space_is_not_admin", headers: map[string]string{adminHeader: " 1"}, status: 403, code: "asker_required"},
		// 2 no identity
		{name: "no_identity", status: 403, code: "asker_required"},
		{name: "whitespace_header", headers: map[string]string{"X-Asker-User": " \t "}, status: 403, code: "asker_required"},
		// 3 invalid
		{name: "header_zero", headers: map[string]string{"X-Asker-User": "0"}, status: 400, code: "asker_invalid"},
		{name: "header_digits_overflow_int32", headers: map[string]string{"X-Asker-User": "2147483648"}, status: 400, code: "asker_invalid"},
		{name: "header_digits_huge", headers: map[string]string{"X-Asker-User": "99999999999999999999"}, status: 400, code: "asker_invalid"},
		// 8 filtered
		{name: "header_email", headers: map[string]string{"X-Asker-User": "alice@example.com"}, status: 200},
		{name: "header_trimmed", headers: map[string]string{"X-Asker-User": "  alice@example.com\t"}, status: 200},
		{name: "header_slack_id", headers: map[string]string{"X-Asker-User": "U5101"}, status: 200},
		{name: "header_eeid_digits", headers: map[string]string{"X-Asker-User": "5101"}, status: 200},
		// 5 unknown
		{name: "header_unknown", headers: map[string]string{"X-Asker-User": "nobody@example.com"}, status: 403, code: "asker_unknown"},
		{name: "header_digits_unknown_eeid", headers: map[string]string{"X-Asker-User": "777777"}, status: 403, code: "asker_unknown"},
		{name: "header_person_without_eeid", headers: map[string]string{"X-Asker-User": "noeeid@example.com"}, status: 403, code: "asker_unknown"},
		// merge chains
		{name: "merge_one_hop", headers: map[string]string{"X-Asker-User": "UMERGED1"}, status: 200},
		{name: "merge_loop", headers: map[string]string{"X-Asker-User": "ULOOP1"}, status: 403, code: "asker_unknown"},
		{name: "merge_missing_target", headers: map[string]string{"X-Asker-User": "UORPHAN"}, status: 403, code: "asker_unknown"},
		{name: "merge_too_many_hops", headers: map[string]string{"X-Asker-User": "UCHAIN1"}, status: 403, code: "asker_unknown"},
		{name: "merge_three_hops_ok", headers: map[string]string{"X-Asker-User": "UCHAIN2"}, status: 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertResolverCase(t, callResolver(t, h, "search", tc), tc)
			assertResolverCase(t, callResolver(t, h, "resolve", tc), tc)
		})
	}
}

func TestAskerResolver_ResolveBodyIdentities(t *testing.T) {
	_, h := resolverFixture(t)
	for _, tc := range []resolverCase{
		{name: "body_slack_id", body: map[string]any{"asker_slack_id": "U5101"}, status: 200},
		{name: "body_slack_id_trimmed", body: map[string]any{"asker_slack_id": "  U5101 "}, status: 200},
		{name: "body_eeid", body: map[string]any{"asker_eeid": 5101}, status: 200},
		{name: "body_eeid_zero_is_absent", body: map[string]any{"asker_eeid": 0}, status: 403, code: "asker_required"},
		{name: "body_whitespace_slack_is_absent", body: map[string]any{"asker_slack_id": "  "}, status: 403, code: "asker_required"},
		{name: "body_eeid_negative", body: map[string]any{"asker_eeid": -1}, status: 400, code: "asker_invalid"},
		{name: "body_eeid_over_int32", body: map[string]any{"asker_eeid": 2147483648}, status: 400, code: "asker_invalid"},
		{name: "body_eeid_unknown", body: map[string]any{"asker_eeid": 777777}, status: 403, code: "asker_unknown"},
		{name: "body_slack_unknown", body: map[string]any{"asker_slack_id": "UNOPE"}, status: 403, code: "asker_unknown"},
		{name: "header_valid_body_unknown", headers: map[string]string{"X-Asker-User": "alice@example.com"}, body: map[string]any{"asker_slack_id": "UNOPE"}, status: 403, code: "asker_unknown"},
		{name: "header_valid_body_eeid_unknown", headers: map[string]string{"X-Asker-User": "alice@example.com"}, body: map[string]any{"asker_eeid": 777777}, status: 403, code: "asker_unknown"},
		{name: "header_and_body_same_person", headers: map[string]string{"X-Asker-User": "alice@example.com"}, body: map[string]any{"asker_slack_id": "U5101", "asker_eeid": 5101}, status: 200},
		{name: "header_and_body_different_people", headers: map[string]string{"X-Asker-User": "alice@example.com"}, body: map[string]any{"asker_slack_id": "U5102"}, status: 400, code: "asker_conflict"},
		{name: "two_body_fields_different_people", body: map[string]any{"asker_slack_id": "U5101", "asker_eeid": 5102}, status: 400, code: "asker_conflict"},
		{name: "header_vs_body_eeid_conflict", headers: map[string]string{"X-Asker-User": "5101"}, body: map[string]any{"asker_eeid": 5102}, status: 400, code: "asker_conflict"},
		{name: "merged_dup_and_target_agree", body: map[string]any{"asker_slack_id": "UMERGED1", "asker_eeid": 5103}, status: 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertResolverCase(t, callResolver(t, h, "resolve", tc), tc)
		})
	}
}

func TestAskerResolver_ResolvedEEIDAndScopes(t *testing.T) {
	pool, _ := resolverFixture(t)
	aclExec(t, pool, `INSERT INTO graph.member_scopes (eeid, scope) VALUES (5101, 'slack:CMEM')`)
	bld := acl.NewBuilder(pool, time.Minute)
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	a, eeid, mode, aerr := resolveAsker(context.Background(), pool, bld, r, askerIdentities{header: "UMERGED1"})
	if aerr != nil || eeid != 5103 || mode != "filtered" || a.noFilter || !a.resolved {
		t.Fatalf("merged: eeid=%d mode=%s noFilter=%v err=%v", eeid, mode, a.noFilter, aerr)
	}
	a, eeid, _, aerr = resolveAsker(context.Background(), pool, bld, r, askerIdentities{header: "alice@example.com"})
	if aerr != nil || eeid != 5101 || !a.scopes["slack:CMEM"] || !a.scopes["public"] {
		t.Fatalf("alice: eeid=%d scopes=%v err=%v", eeid, a.scopes, aerr)
	}
	r.Header.Set(adminHeader, "1")
	a, _, mode, aerr = resolveAsker(context.Background(), pool, bld, r, askerIdentities{})
	if aerr != nil || mode != "admin" || !a.noFilter {
		t.Fatalf("admin: mode=%s noFilter=%v err=%v", mode, a.noFilter, aerr)
	}
}

func TestAskerResolver_DBLookupErrorIs503(t *testing.T) {
	pool, _ := resolverFixture(t)
	closed, err := pgxpool.New(context.Background(), pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	closed.Close()
	for _, ids := range []askerIdentities{
		{header: "alice@example.com"}, {header: "5101"}, {slackID: "U5101"}, {eeid: 5101},
	} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		_, _, mode, aerr := resolveAsker(context.Background(), closed, acl.NewBuilder(pool, time.Minute), r, ids)
		if aerr == nil || aerr.status != http.StatusServiceUnavailable || aerr.code != "acl_unavailable" || mode != "rejected" {
			t.Errorf("%+v: err=%v mode=%s, want 503 acl_unavailable", ids, aerr, mode)
		}
	}
}

// Unknown asker and a failing scope builder are denied on all five routes.
func TestAskerResolver_DeniedOnAllFiveRoutes(t *testing.T) {
	pool, _ := resolverFixture(t)
	closed, err := pgxpool.New(context.Background(), pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	closed.Close()
	broken := acl.NewBuilder(closed, time.Minute) // For() fails; lookups use the healthy pool.
	search, _ := NewSearch(pool)
	search.aclBld = broken
	resolve, _ := NewResolve(pool)
	resolve.aclBld = broken
	node := NewNode(pool)
	node.aclBld = broken
	epic := NewEpic(pool)
	epic.aclBld = broken
	nb := &neighborsHandler{db: pool, exp: bfs.NewExpander(pool), aclBld: broken}
	r := chi.NewRouter()
	r.Method("GET", "/api/graph/search", search)
	r.Method("POST", "/api/graph/resolve", resolve)
	r.Method("GET", "/api/graph/node", node)
	r.Method("GET", "/api/graph/epic/{key}", epic)
	r.Get("/api/graph/node/{id}/neighbors", nb.serve)

	routes := []struct{ method, target, body string }{
		{"GET", "/api/graph/search?q=resquokka", ""},
		{"POST", "/api/graph/resolve", `{"seeds":["jira:RES-1"]}`},
		{"GET", "/api/graph/node?id=jira:RES-1", ""},
		{"GET", "/api/graph/epic/PAY-1", ""},
		{"GET", "/api/graph/node/" + url.PathEscape("jira:RES-1") + "/neighbors", ""},
	}
	for _, rt := range routes {
		for _, tc := range []struct {
			asker, code string
			status      int
		}{
			{"nobody@example.com", "asker_unknown", 403},
			{"alice@example.com", "acl_unavailable", 503}, // known asker, scope builder error
		} {
			req := httptest.NewRequest(rt.method, rt.target, strings.NewReader(rt.body))
			req.Header.Set("X-Asker-User", tc.asker)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.code) {
				t.Errorf("%s %s asker=%s: status=%d body=%s", rt.method, rt.target, tc.asker, w.Code, w.Body.String())
			}
		}
	}
}
