package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/agent-mem/agent-mem/internal/graph/acl"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func attachmentACLFixture(t *testing.T, pool *pgxpool.Pool) http.Handler {
	t.Helper()
	resetEpicACL(t, pool)
	for _, n := range []struct{ id, typ, scope string }{
		{"jira:ACL-CONTROL", "jira", "public"},
		{"slack:CPRIV:1", "slack", "slack:CPRIV"},
		{"slack:COTHER:2", "slack", "slack:COTHER"},
		{"slack:PUBLIC:3", "slack", ""},
		{"jira:PAYX-1", "jira", "jira:PAYX"},
		{"slack_file:F1", "slack_file", ""},
		{"slack_file:F3", "slack_file", ""},
		{"slack_file:F4", "slack_file", ""},
		{"slack_file:F5", "slack_file", ""},
		{"jira_attachment:A1", "jira_attachment", ""},
	} {
		seedKeywordNode(t, pool, n.id, "attachquokka resource", n.scope, "attachquokka indexed resource")
		aclExec(t, pool, `UPDATE graph.nodes SET type=$2, body='attachquokka body',url='https://example.com/' || id WHERE id=$1`, n.id, n.typ)
	}
	for _, e := range [][2]string{{"slack:CPRIV:1", "slack_file:F1"}, {"slack:COTHER:2", "slack_file:F1"}, {"slack:PUBLIC:3", "slack_file:F3"}, {"slack_file:F1", "slack_file:F5"}, {"jira:PAYX-1", "jira_attachment:A1"}} {
		aclExec(t, pool, `INSERT INTO graph.edges(from_node_id,to_node_id,kind,metadata,machine_id) VALUES($1,$2,'REFERENCES','{}','test')`, e[0], e[1])
	}
	aclPerson(t, pool, 81, "private@example.com", "slack:CPRIV")
	aclPerson(t, pool, 82, "plain@example.com")
	aclPerson(t, pool, 83, "jira@example.com", "jira:PAYX")
	r := chi.NewRouter()
	r.Method("GET", "/api/graph/node", NewNode(pool))
	search, err := NewSearch(pool)
	if err != nil {
		t.Fatal(err)
	}
	r.Method("GET", "/api/graph/search", search)
	r.Mount("/api/graph", NewNeighbors(pool))
	resolve, err := NewResolve(pool)
	if err != nil {
		t.Fatal(err)
	}
	r.Method("POST", "/api/graph/resolve", resolve)
	return r
}

func attachmentResolve(t *testing.T, h http.Handler, id string, eeid, budget int) resolveResponse {
	t.Helper()
	b, _ := json.Marshal(resolveRequest{Seeds: []string{id, "jira:ACL-CONTROL"}, AskerEEID: eeid, Depth: 1, BudgetTokens: budget, IncludeBodies: true})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/api/graph/resolve", bytes.NewReader(b)))
	if w.Code != 200 {
		t.Fatalf("resolve status=%d body=%s", w.Code, w.Body.String())
	}
	var resp resolveResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestAttachmentACL(t *testing.T) {
	pool := openTestDB(t)
	h := attachmentACLFixture(t, pool)
	files := []string{"slack_file:F1", "slack_file:F3", "slack_file:F4", "slack_file:F5", "jira_attachment:A1"}
	for _, tc := range []struct {
		name, header string
		eeid         int
		want         []bool
	}{
		{"unfiltered", "", 0, []bool{true, true, true, true, true}},
		{"unknown", "nobody@example.com", -1, []bool{false, false, false, false, false}},
		{"private_member", "private@example.com", 81, []bool{true, true, false, false, false}},
		{"no_memberships", "plain@example.com", 82, []bool{false, true, false, false, false}},
		{"jira_member", "jira@example.com", 83, []bool{false, true, false, false, true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for i, id := range files {
				t.Run(id, func(t *testing.T) {
					w := askerRequest(t, h, "/api/graph/node?id="+url.QueryEscape(id), tc.header)
					wantStatus := 404
					if tc.want[i] {
						wantStatus = 200
					}
					if w.Code != wantStatus {
						t.Errorf("node status=%d want=%d body=%s", w.Code, wantStatus, w.Body.String())
					}
					for _, mode := range []string{"default", "hybrid"} {
						w := askerRequest(t, h, "/api/graph/search?q=attachquokka&limit=50&match="+mode, tc.header)
						if w.Code != 200 {
							t.Fatalf("search status=%d body=%s", w.Code, w.Body.String())
						}
						var resp searchResponse
						if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
							t.Fatal(err)
						}
						got := map[string]bool{}
						for _, n := range resp.Results {
							got[n.NodeID] = true
						}
						if !got["jira:ACL-CONTROL"] {
							t.Fatalf("%s missing control: %s", mode, w.Body.String())
						}
						if got[id] != tc.want[i] {
							t.Errorf("%s %s visible=%v want=%v", mode, id, got[id], tc.want[i])
						}
					}
					// Resolve's numeric contract is distinct: zero is unfiltered, any nonzero is resolved.
					if tc.name != "unknown" {
						resp := attachmentResolve(t, h, id, tc.eeid, 4000)
						got := map[string]bool{}
						for _, n := range resp.Artifacts {
							got[n.NodeID] = true
						}
						if !got["jira:ACL-CONTROL"] {
							t.Fatal("resolve missing control")
						}
						if got[id] != tc.want[i] {
							t.Errorf("resolve visible=%v want=%v artifacts=%v", got[id], tc.want[i], resp.Artifacts)
						}
					}
				})
			}
			for _, parent := range []struct {
				id, file string
				index    int
			}{{"slack:CPRIV:1", "slack_file:F1", 0}, {"slack:PUBLIC:3", "slack_file:F3", 1}, {"slack_file:F1", "slack_file:F5", 3}, {"jira:PAYX-1", "jira_attachment:A1", 4}} {
				for _, surface := range []string{"edges", "neighbors"} {
					path := "/api/graph/node?id=" + url.QueryEscape(parent.id)
					if surface == "neighbors" {
						path = "/api/graph/node/" + url.QueryEscape(parent.id) + "/neighbors?kind=REFERENCES"
					}
					w := askerRequest(t, h, path, tc.header)
					if w.Code == 404 {
						continue
					}
					if w.Code != 200 {
						t.Fatalf("%s status=%d body=%s", surface, w.Code, w.Body.String())
					}
					got := false
					if surface == "edges" {
						var resp nodeResponse
						json.Unmarshal(w.Body.Bytes(), &resp)
						for _, e := range resp.EdgesOut {
							if e.To == parent.file {
								got = true
							}
						}
					} else {
						var resp struct {
							Items []neighborItem `json:"neighbors"`
						}
						json.Unmarshal(w.Body.Bytes(), &resp)
						for _, n := range resp.Items {
							if n.Node.NodeID == parent.file {
								got = true
							}
						}
					}
					if got != tc.want[parent.index] {
						t.Errorf("%s parent %s file %s visible=%v want=%v", surface, parent.id, parent.file, got, tc.want[parent.index])
					}
				}
			}
		})
	}
	t.Run("permitted_over_budget_seed", func(t *testing.T) {
		aclExec(t, pool, `INSERT INTO graph.artifact_bodies(node_id,body_full,machine_id) VALUES('slack_file:F3',repeat('largebody ',5000),'test')`)
		aclExec(t, pool, `INSERT INTO graph.artifact_bodies(node_id,body_full,machine_id) VALUES('jira:ACL-CONTROL','ok','test')`)
		resp := attachmentResolve(t, h, "slack_file:F3", 82, 1)
		for _, a := range resp.Artifacts {
			if a.NodeID == "slack_file:F3" {
				if a.Body != "" {
					t.Fatal("over-budget body emitted")
				}
				return
			}
		}
		t.Fatal("permitted seed fallback missing")
	})
}

func TestAttachmentACL_DynamicParents(t *testing.T) {
	pool := openTestDB(t)
	h := attachmentACLFixture(t, pool)
	assertNode := func(id, header string, want int) {
		t.Helper()
		w := askerRequest(t, h, "/api/graph/node?id="+url.QueryEscape(id), header)
		if w.Code != want {
			t.Fatalf("%s status=%d want=%d", id, w.Code, want)
		}
	}
	assertNode("slack_file:F1", "private@example.com", 200)
	aclExec(t, pool, `DELETE FROM graph.edges WHERE from_node_id='slack:CPRIV:1' AND to_node_id='slack_file:F1'`)
	assertNode("slack_file:F1", "private@example.com", 404)
	aclExec(t, pool, `INSERT INTO graph.edges(from_node_id,to_node_id,kind,metadata,machine_id) VALUES('slack:CPRIV:1','slack_file:F1','REFERENCES','{}','test')`)
	aclExec(t, pool, `UPDATE graph.nodes SET deleted_at=now() WHERE id='slack:CPRIV:1'`)
	assertNode("slack_file:F1", "private@example.com", 404)
	assertNode("slack_file:F3", "plain@example.com", 200)
	aclExec(t, pool, `UPDATE graph.nodes SET scope='slack:CPRIV' WHERE id='slack:PUBLIC:3'`)
	assertNode("slack_file:F3", "plain@example.com", 404)
	for _, s := range []any{nil, "", "public"} {
		aclExec(t, pool, `UPDATE graph.nodes SET scope=$1 WHERE id='slack:PUBLIC:3'`, s)
		assertNode("slack_file:F3", "plain@example.com", 200)
		assertNode("slack_file:F3", "nobody@example.com", 404)
	}
	aclExec(t, pool, `UPDATE graph.nodes SET deleted_at=NULL WHERE id='slack:CPRIV:1'`)
	aclExec(t, pool, `UPDATE graph.nodes SET scope='slack:CPRIV' WHERE id='slack:COTHER:2'`)
	assertNode("slack_file:F1", "private@example.com", 200)
	aclExec(t, pool, `DELETE FROM graph.edges WHERE from_node_id='slack:CPRIV:1' AND to_node_id='slack_file:F1'`)
	assertNode("slack_file:F1", "private@example.com", 200)
}

func TestAttachmentACL_ErrorsFailClosed(t *testing.T) {
	pool := openTestDB(t)
	attachmentACLFixture(t, pool)
	closed, err := pgxpool.New(context.Background(), pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	closed.Close()
	for _, tc := range []struct {
		name       string
		lookupDB   *pgxpool.Pool
		builder    *acl.Builder
		wantPublic bool
	}{
		{"builder_error", pool, acl.NewBuilder(closed, time.Minute), true},
		{"lookup_error", closed, acl.NewBuilder(pool, time.Minute), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eeid, scopes, noFilter := askerScopeSet(context.Background(), tc.lookupDB, tc.builder, "private@example.com")
			a := askerACL{noFilter: noFilter, resolved: eeid != 0, scopes: scopes}
			for _, id := range []string{"slack_file:F1", "slack_file:F3", "slack_file:F4"} {
				got, err := nodeVisible(context.Background(), pool, a, id, "slack_file", nil)
				want := id == "slack_file:F3" && tc.wantPublic
				if err != nil || got != want {
					t.Errorf("%s visible=%v err=%v want=%v", id, got, err, want)
				}
			}
			var control bool
			if err := pool.QueryRow(context.Background(), `SELECT `+aclVisibleSQL("n", 1, 2)+` FROM graph.nodes n WHERE n.id='jira:ACL-CONTROL'`, a.scopeArg(), a.resolved).Scan(&control); err != nil || !control {
				t.Fatalf("control visible=%v err=%v", control, err)
			}
		})
	}
	a := askerACL{resolved: true, scopes: map[string]bool{"public": true}}
	if visible, err := nodeVisible(context.Background(), closed, a, "slack_file:F3", "slack_file", nil); err == nil || visible {
		t.Fatalf("closed node probe visible=%v err=%v", visible, err)
	}
	if got, err := visibleAttachments(context.Background(), closed, a, []string{"slack_file:F3"}); err == nil || got["slack_file:F3"] {
		t.Fatalf("closed batch visible=%v err=%v", got, err)
	}
}
