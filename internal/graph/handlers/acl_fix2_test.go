package handlers

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/agent-mem/agent-mem/internal/graph/acl"
	"github.com/agent-mem/agent-mem/internal/graph/bfs"
)

// F1: a private-channel slack_file referenced by a visible org-scope Jira
// parent stays hidden from non-members: the file's own scope must pass too.
func TestACLRoutes_SlackFileNeedsOwnScopeEvenWithOrgParent(t *testing.T) {
	pool := openTestDB(t)
	resetEpicACL(t, pool)
	seedKeywordNode(t, pool, "jira:PAY-1", "sfiletoken parent", "jira:PAY", "sfiletoken parent body")
	seedKeywordNode(t, pool, "slack_file:FPRIVATE", "sfiletoken file", "slack:GPRIV", "sfiletoken file body")
	aclExec(t, pool, `UPDATE graph.nodes SET type='slack_file' WHERE id='slack_file:FPRIVATE'`)
	aclExec(t, pool, `INSERT INTO graph.edges (from_node_id,to_node_id,kind,metadata,machine_id) VALUES
		('jira:PAY-1','slack_file:FPRIVATE','REFERENCES','{}','test')`)
	aclMember(t, pool, "jira:PAY-1", "PAY-1", viaEpicSelf)
	aclMember(t, pool, "slack_file:FPRIVATE", "PAY-1", viaKey)
	aclPerson(t, pool, 5601, "nonmember@example.com")
	aclPerson(t, pool, 5602, "chanmember@example.com", "slack:GPRIV")
	h := mountedGraph(t, pool)

	routes := []struct{ name, method, target, body string }{
		{"node", "GET", "/api/graph/node?id=" + url.QueryEscape("slack_file:FPRIVATE"), ""},
		{"search", "GET", "/api/graph/search?q=sfiletoken&limit=50", ""},
		{"neighbors", "GET", "/api/graph/node/" + url.PathEscape("jira:PAY-1") + "/neighbors?depth=2", ""},
		{"neighbors_cards", "GET", "/api/graph/node/" + url.PathEscape("jira:PAY-1") + "/neighbors?depth=2&cards=1", ""},
		{"epic", "GET", "/api/graph/epic/PAY-1", ""},
		{"resolve", "POST", "/api/graph/resolve", `{"seeds":["jira:PAY-1"],"depth":2,"include_bodies":true}`},
	}
	for _, who := range []struct {
		name    string
		headers map[string]string
		see     bool
	}{
		{"nonmember", map[string]string{"X-Asker-User": "nonmember@example.com"}, false},
		{"member", map[string]string{"X-Asker-User": "chanmember@example.com"}, true},
		{"admin", map[string]string{adminHeader: "1"}, true},
	} {
		for _, rt := range routes {
			t.Run(who.name+"/"+rt.name, func(t *testing.T) {
				hd := map[string]string{"Content-Type": "application/json"}
				for k, v := range who.headers {
					hd[k] = v
				}
				w := graphDo(h, rt.method, rt.target, rt.body, hd)
				wantCode := http.StatusOK
				if rt.name == "node" && !who.see {
					wantCode = http.StatusNotFound
				}
				if w.Code != wantCode {
					t.Fatalf("status=%d want %d body=%s", w.Code, wantCode, w.Body.String())
				}
				if got := strings.Contains(w.Body.String(), "FPRIVATE"); got != who.see {
					t.Errorf("contains file=%v want %v: %s", got, who.see, w.Body.String())
				}
			})
		}
	}
}

// F3: a row with its own eeid is that employee; merged_into is not followed.
func TestAskerResolver_OwnEEIDWinsOverMergeChain(t *testing.T) {
	pool := openTestDB(t)
	resetEpicACL(t, pool)
	aclExec(t, pool, `INSERT INTO graph.people (eeid, display_name, slack_user_id, machine_id) VALUES (20, 'c', 'UC20', 'test')`)
	aclExec(t, pool, `INSERT INTO graph.people (display_name, slack_user_id, merged_into, machine_id)
		SELECT 'b', 'UB', id, 'test' FROM graph.people WHERE eeid = 20`)
	aclExec(t, pool, `INSERT INTO graph.people (eeid, display_name, slack_user_id, merged_into, machine_id)
		SELECT 10, 'a', 'UA10', id, 'test' FROM graph.people WHERE slack_user_id = 'UB'`)
	bld := acl.NewBuilder(pool, time.Minute)
	r := mustRequest()
	for _, tc := range []struct {
		ids  askerIdentities
		want int
	}{
		{askerIdentities{header: "10"}, 10},
		{askerIdentities{header: "UA10"}, 10},
		{askerIdentities{header: "UB"}, 20},
	} {
		_, eeid, _, aerr := resolveAsker(context.Background(), pool, bld, r, tc.ids)
		if aerr != nil || eeid != tc.want {
			t.Errorf("%+v: eeid=%d err=%v want %d", tc.ids, eeid, aerr, tc.want)
		}
	}
}

func mustRequest() *http.Request {
	r, _ := http.NewRequest(http.MethodGet, "/", nil)
	return r
}

// F6: a hidden thread reachable only as a Slack row linking a visible Jira
// source (addLinkedThreads) is absent from hybrid search for a non-member.
func TestACLSearch_HiddenThreadReachableOnlyByLink(t *testing.T) {
	pool := openTestDB(t)
	resetEpicACL(t, pool)
	seedKeywordNode(t, pool, "jira:LNK-1", "linktoken source", "jira:LNK", "linktoken source body")
	aclExec(t, pool, `UPDATE graph.nodes SET type='jira' WHERE id='jira:LNK-1'`)
	seedKeywordNode(t, pool, "slack:CLNK:1.0", "unrelated chatter", "slack:CLNK", "unrelated chatter body")
	aclExec(t, pool, `UPDATE graph.nodes SET type='slack', metadata='{"thread_ts":"1.0"}'::jsonb WHERE id='slack:CLNK:1.0'`)
	aclExec(t, pool, `INSERT INTO graph.edges (from_node_id,to_node_id,kind,metadata,machine_id) VALUES
		('slack:CLNK:1.0','jira:LNK-1','REFERENCES','{}','test')`)
	aclPerson(t, pool, 5701, "linkout@example.com")
	aclPerson(t, pool, 5702, "linkin@example.com", "slack:CLNK")
	h := mountedGraph(t, pool)
	target := "/api/graph/search?q=linktoken&limit=50&match=hybrid"

	w := graphGet(h, target, map[string]string{"X-Asker-User": "linkout@example.com"})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "jira:LNK-1") {
		t.Fatalf("non-member: status=%d body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "CLNK") {
		t.Errorf("hidden linked thread leaked: %s", w.Body.String())
	}
	w = graphGet(h, target, map[string]string{"X-Asker-User": "linkin@example.com"})
	if !strings.Contains(w.Body.String(), "slack:CLNK:1.0") {
		t.Errorf("member should see the linked thread (test premise): %s", w.Body.String())
	}
}

// F6: org scopes (D1) also hold on /neighbors, /epic and /resolve.
func TestACLRoutes_OrgScopesOnGraphRoutes(t *testing.T) {
	pool := openTestDB(t)
	resetEpicACL(t, pool)
	seedKeywordNode(t, pool, "jira:ORGR-1", "orgroute root", "public", "orgroute root body")
	linked := map[string]struct {
		scope string
		seen  bool
	}{
		"jira:ORGR-PAY":  {"jira:PAY", true},
		"jira:ORGR-GH":   {"github:wego/payments", true},
		"jira:ORGR-CF":   {"confluence:X", true},
		"jira:ORGR-EVIL": {"jiraevil:X", false},
		"jira:ORGR-GWS":  {"gws", false},
		"jira:ORGR-SLK":  {"slack:CNOPE", false},
	}
	aclMember(t, pool, "jira:ORGR-1", "ORGR-1", viaEpicSelf)
	for id, n := range linked {
		seedKeywordNode(t, pool, id, "orgroute "+id, n.scope, "orgroute "+id)
		aclExec(t, pool, `INSERT INTO graph.edges (from_node_id,to_node_id,kind,metadata,machine_id) VALUES ('jira:ORGR-1',$1,'REFERENCES','{}','test')`, id)
		aclMember(t, pool, id, "ORGR-1", viaKey)
	}
	aclPerson(t, pool, 5801, "orgroute@example.com")
	h := mountedGraph(t, pool)
	asker := map[string]string{"X-Asker-User": "orgroute@example.com", "Content-Type": "application/json"}
	routes := []struct{ name, method, target, body string }{
		{"neighbors", "GET", "/api/graph/node/" + url.PathEscape("jira:ORGR-1") + "/neighbors?depth=1", ""},
		{"epic", "GET", "/api/graph/epic/ORGR-1", ""},
		{"resolve", "POST", "/api/graph/resolve", `{"seeds":["jira:ORGR-1"],"depth":1,"include_bodies":true}`},
	}
	for _, rt := range routes {
		t.Run(rt.name, func(t *testing.T) {
			w := graphDo(h, rt.method, rt.target, rt.body, asker)
			if w.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			for id, n := range linked {
				if got := strings.Contains(w.Body.String(), id); got != n.seen {
					t.Errorf("%s: contains=%v want %v", id, got, n.seen)
				}
			}
		})
	}
}

// F6: asker resolves (people readable) but the neighbors root-node query then
// fails: 500, never a 200 with data.
func TestNeighbors_RootQueryErrorIs500(t *testing.T) {
	pool := openTestDB(t)
	h := seedAskerFixture(t, pool)
	_ = h
	aclExec(t, pool, `DO $$ BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname='acl_peopleonly') THEN CREATE ROLE acl_peopleonly; END IF; END $$`)
	aclExec(t, pool, `GRANT USAGE ON SCHEMA graph TO acl_peopleonly`)
	aclExec(t, pool, `GRANT SELECT ON graph.people TO acl_peopleonly`)
	cfg := pool.Config().Copy()
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, `SET ROLE acl_peopleonly`)
		return err
	}
	limited, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer limited.Close()
	handler := &neighborsHandler{
		db:     limited,
		exp:    bfs.NewExpander(pool),
		aclBld: acl.NewBuilder(pool, 5*time.Minute),
	}
	r := chi.NewRouter()
	r.Get("/api/graph/node/{id}/neighbors", handler.serve)
	w := askerRequest(t, r, "/api/graph/node/"+url.QueryEscape(askerRoot)+"/neighbors?kind=REFERENCES", "plain@example.com")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("root query error: status=%d body=%s", w.Code, w.Body.String())
	}
}
