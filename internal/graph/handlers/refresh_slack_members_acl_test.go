package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-mem/agent-mem/internal/graph/acl"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
)

const slackMembersACLTTL = 100 * time.Millisecond

func slackMembersACLReaders(t *testing.T, pool *pgxpool.Pool) http.Handler {
	t.Helper()
	node := NewNode(pool)
	node.aclBld = acl.NewBuilder(pool, slackMembersACLTTL)
	search, err := NewSearch(pool)
	if err != nil {
		t.Fatal(err)
	}
	search.aclBld = acl.NewBuilder(pool, slackMembersACLTTL)
	r := chi.NewRouter()
	r.Method("GET", "/api/graph/node", node)
	r.Method("GET", "/api/graph/search", search)
	return r
}

func assertSlackMembersACLNode(t *testing.T, h http.Handler, asker, id string, want int) {
	t.Helper()
	w := askerRequest(t, h, "/api/graph/node?id="+url.QueryEscape(id), asker)
	if w.Code != want {
		t.Fatalf("asker=%s node=%s status=%d want=%d body=%s", asker, id, w.Code, want, w.Body.String())
	}
}

func assertSlackMembersACLSearch(t *testing.T, h http.Handler, asker, query string, visibility map[string]bool) {
	t.Helper()
	for _, mode := range []string{"default", "hybrid"} {
		w := askerRequest(t, h, "/api/graph/search?q="+url.QueryEscape(query)+"&limit=50&match="+mode, asker)
		if w.Code != http.StatusOK {
			t.Fatalf("asker=%s search=%s status=%d body=%s", asker, mode, w.Code, w.Body.String())
		}
		var resp searchResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatal(err)
		}
		got := make(map[string]bool)
		for _, n := range resp.Results {
			got[n.NodeID] = true
		}
		for id, want := range visibility {
			if got[id] != want {
				t.Errorf("asker=%s search=%s node=%s visible=%v want=%v body=%s", asker, mode, id, got[id], want, w.Body.String())
			}
		}
	}
}

func TestRefreshSlackMembers_ACL(t *testing.T) {
	t.Run("grant_and_warmed_cache_revocation", func(t *testing.T) {
		pool := openTestDB(t)
		resetEpicACL(t, pool)
		const message = "slack:CPRIV:1"
		const attachment = "slack_file:MEMBERS"
		const control = "jira:MEMBERS-CONTROL"
		for _, n := range []struct{ id, typ, scope string }{
			{message, "slack", "slack:CPRIV"},
			// The attachment's own scope is not granted. Its only visibility
			// comes from the live message parent.
			{attachment, "slack_file", "slack:CATTACHMENT"},
			{control, "jira", "public"},
		} {
			seedKeywordNode(t, pool, n.id, "memberquokka resource", n.scope, "memberquokka indexed resource")
			aclExec(t, pool, `UPDATE graph.nodes SET type=$2,body='memberquokka body' WHERE id=$1`, n.id, n.typ)
		}
		aclExec(t, pool, `INSERT INTO graph.edges(from_node_id,to_node_id,kind,metadata,machine_id) VALUES($1,$2,'REFERENCES','{}','test')`, message, attachment)
		aclPerson(t, pool, 1001, "member@example.com")
		aclPerson(t, pool, 1002, "nonmember@example.com")
		aclExec(t, pool, `UPDATE graph.people SET slack_user_id=$2 WHERE eeid=$1`, 1001, "U1001")
		aclExec(t, pool, `UPDATE graph.people SET slack_user_id=$2 WHERE eeid=$1`, 1002, "U1002")

		var member atomic.Bool
		member.Store(true)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			members := []string{}
			if r.URL.Query().Get("channel") == "CPRIV" && member.Load() {
				members = append(members, "U1001")
			}
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(map[string]any{"ok": true, "members": members, "response_metadata": map[string]string{"next_cursor": ""}}); err != nil {
				t.Errorf("encode Slack response: %v", err)
			}
		}))
		t.Cleanup(srv.Close)
		refresh := refreshSlackMembersWithClient(Deps{DB: pool, Logger: zerolog.Nop(), MachineID: "test", SlackBotToken: "test-token"}, slackMembersClient{baseURL: srv.URL, http: srv.Client()})
		h := slackMembersACLReaders(t, pool)

		// Warm both independent readers before granting membership.
		assertSlackMembersACLNode(t, h, "U1001", message, http.StatusNotFound)
		assertSlackMembersACLSearch(t, h, "U1001", "memberquokka", map[string]bool{message: false, attachment: false, control: true})
		if err := refresh(context.Background(), []byte(`{"force":true}`)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * slackMembersACLTTL)
		assertSlackMembersACLNode(t, h, "U1001", message, http.StatusOK)
		assertSlackMembersACLNode(t, h, "U1001", attachment, http.StatusOK)
		assertSlackMembersACLSearch(t, h, "U1001", "memberquokka", map[string]bool{message: true, attachment: true, control: true})
		assertSlackMembersACLNode(t, h, "U1002", message, http.StatusNotFound)
		assertSlackMembersACLNode(t, h, "U1002", attachment, http.StatusNotFound)
		assertSlackMembersACLSearch(t, h, "U1002", "memberquokka", map[string]bool{message: false, attachment: false, control: true})

		// Rewarm both membership snapshots immediately before revoking.
		time.Sleep(2 * slackMembersACLTTL)
		assertSlackMembersACLNode(t, h, "U1001", message, http.StatusOK)
		assertSlackMembersACLSearch(t, h, "U1001", "memberquokka", map[string]bool{message: true, attachment: true, control: true})
		member.Store(false)
		if err := refresh(context.Background(), []byte(`{"force":true}`)); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * slackMembersACLTTL)
		assertSlackMembersACLNode(t, h, "U1001", message, http.StatusNotFound)
		assertSlackMembersACLNode(t, h, "U1001", attachment, http.StatusNotFound)
		assertSlackMembersACLSearch(t, h, "U1001", "memberquokka", map[string]bool{message: false, attachment: false, control: true})
	})

	t.Run("aged_grants_expire_without_a_job", func(t *testing.T) {
		pool := openTestDB(t)
		resetEpicACL(t, pool)
		for _, n := range []struct{ id, typ, scope string }{
			{"slack:CAGED:1", "slack", "slack:CAGED"},
			{"slack:GAGED:1", "slack", "slack:GAGED"},
			{"slack:CFRESH:1", "slack", "slack:CFRESH"},
			{"slack:D1:1", "slack", "slack:D1"},
			{"jira:PAY-1", "jira", "jira:PAY"},
			{"jira:EXPIRY-CONTROL", "jira", "public"},
		} {
			seedKeywordNode(t, pool, n.id, "expiryquokka resource", n.scope, "expiryquokka indexed resource")
			aclExec(t, pool, `UPDATE graph.nodes SET type=$2 WHERE id=$1`, n.id, n.typ)
		}
		aclPerson(t, pool, 1001, "aged@example.com", "slack:CAGED")
		aclPerson(t, pool, 1002, "fresh@example.com", "slack:CFRESH")
		aclPerson(t, pool, 1003, "other@example.com", "slack:D1", "jira:PAY")
		aclPerson(t, pool, 1004, "group@example.com", "slack:GAGED")
		h := slackMembersACLReaders(t, pool)
		// Cache grants while fresh, then age the persisted evidence without
		// running refresh or housekeeping. Readers must enforce the boundary.
		for _, tc := range []struct{ asker, id string }{
			{"aged@example.com", "slack:CAGED:1"},
			{"group@example.com", "slack:GAGED:1"},
		} {
			assertSlackMembersACLNode(t, h, tc.asker, tc.id, http.StatusOK)
			assertSlackMembersACLSearch(t, h, tc.asker, "expiryquokka", map[string]bool{tc.id: true, "jira:EXPIRY-CONTROL": true})
		}
		aclExec(t, pool, `UPDATE graph.member_scopes SET refreshed_at=now()-interval '25 hours' WHERE eeid IN (1001,1003,1004)`)
		aclExec(t, pool, `UPDATE graph.member_scopes SET refreshed_at=now()-interval '1 hour' WHERE eeid=1002`)
		time.Sleep(2 * slackMembersACLTTL)
		for _, tc := range []struct {
			asker, id string
			visible   bool
		}{
			{"aged@example.com", "slack:CAGED:1", false},
			{"group@example.com", "slack:GAGED:1", false},
			{"fresh@example.com", "slack:CFRESH:1", true},
			{"other@example.com", "slack:D1:1", true},
			{"other@example.com", "jira:PAY-1", true},
		} {
			want := http.StatusNotFound
			if tc.visible {
				want = http.StatusOK
			}
			assertSlackMembersACLNode(t, h, tc.asker, tc.id, want)
			assertSlackMembersACLSearch(t, h, tc.asker, "expiryquokka", map[string]bool{tc.id: tc.visible, "jira:EXPIRY-CONTROL": true})
		}
	})
}
