package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	aclEpicKey = "PAY-200"
	aclPriv    = "slack:CPRIV"
)

var briefKeys = []string{"brief", "highlights", "open_items", "brief_updated_at"}

func resetEpicACL(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	clean := func() {
		truncateGraphHandlerTables(t, pool)
		for _, tbl := range []string{"graph.epic_briefs", "graph.member_scopes"} {
			if _, err := pool.Exec(context.Background(), "DELETE FROM "+tbl); err != nil {
				t.Fatalf("clean %s: %v", tbl, err)
			}
		}
	}
	clean()
	t.Cleanup(clean)
}

func aclExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec: %v\n%s", err, sql)
	}
}

func aclNode(t *testing.T, pool *pgxpool.Pool, id, typ, scope, meta, created string) {
	t.Helper()
	var sc any
	if scope != "" {
		sc = scope
	}
	aclExec(t, pool, `INSERT INTO graph.nodes (id, type, natural_key, title, scope, metadata, machine_id, created_at)
		VALUES ($1,$2,$1,$1,$3,$4::jsonb,'test',$5::timestamptz)`, id, typ, sc, meta, created)
}

func aclMember(t *testing.T, pool *pgxpool.Pool, nodeID, epic, via string) {
	t.Helper()
	aclExec(t, pool, `INSERT INTO graph.epic_membership (node_id, epic_key, via, confidence, first_at, last_at)
		VALUES ($1,$2,$3,1,'2026-07-01','2026-09-10')`, nodeID, epic, via)
}

func aclPerson(t *testing.T, pool *pgxpool.Pool, eeid int, email string, scopes ...string) {
	t.Helper()
	aclExec(t, pool, `INSERT INTO graph.people (eeid, display_name, email, machine_id) VALUES ($1,$2::text,$2::text,'test')`, eeid, email)
	for _, s := range scopes {
		aclExec(t, pool, `INSERT INTO graph.member_scopes (eeid, scope) VALUES ($1,$2)`, eeid, s)
	}
}

func aclBrief(t *testing.T, pool *pgxpool.Pool, sources ...string) {
	t.Helper()
	aclExec(t, pool, `INSERT INTO graph.epic_briefs (epic_key, brief, highlights, open_items, sources)
		VALUES ($1,'the brief','[{"text":"h","sources":[]}]','[{"text":"o","sources":[]}]',$2)`, aclEpicKey, sources)
}

// aclGet returns status, body, decoded struct and generic map.
func aclGet(t *testing.T, key, asker string) (int, string, epicResponse, map[string]any) {
	t.Helper()
	pool := openTestDB(t)
	r := chi.NewRouter()
	r.Method("GET", "/api/graph/epic/{key}", NewEpic(pool))
	req := httptest.NewRequest("GET", "/api/graph/epic/"+key, nil)
	if asker != "" {
		req.Header.Set("X-Asker-User", asker)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	body := w.Body.String()
	var resp epicResponse
	var m map[string]any
	_ = json.Unmarshal([]byte(body), &resp)
	_ = json.Unmarshal([]byte(body), &m)
	return w.Code, body, resp, m
}

func assertKeys(t *testing.T, m map[string]any, present bool, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if _, ok := m[k]; ok != present {
			t.Errorf("key %q present=%v, want %v", k, ok, present)
		}
	}
}

func seedACLEpic(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	aclNode(t, pool, "jira:"+aclEpicKey, "jira", "", `{}`, "2026-07-01")
	aclMember(t, pool, "jira:"+aclEpicKey, aclEpicKey, viaEpicSelf)
	aclNode(t, pool, "jira:PAY-201", "jira", "", `{}`, "2026-09-10")
	aclMember(t, pool, "jira:PAY-201", aclEpicKey, viaEpicSelf)
	aclNode(t, pool, aclPriv+":1.0", "slack", aclPriv, `{"thread_ts":"1.0"}`, "2026-07-01")
	aclMember(t, pool, aclPriv+":1.0", aclEpicKey, viaKey)
	aclNode(t, pool, aclPriv+":1.5", "slack", aclPriv, `{"thread_ts":"1.0"}`, "2026-07-02")
	aclMember(t, pool, aclPriv+":1.5", aclEpicKey, viaKey)
	aclPerson(t, pool, 41, "plain@example.com")
	aclPerson(t, pool, 42, "priv@example.com", aclPriv)
}

func TestEpicACL_MembersFiltered(t *testing.T) {
	pool := openTestDB(t)
	resetEpicACL(t, pool)
	seedACLEpic(t, pool)

	code, _, resp, m := aclGet(t, aclEpicKey, "plain@example.com")
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	n := 0
	for _, v := range resp.Members {
		n += len(v)
	}
	if n != 1 || resp.Total != 1 || resp.Replies != 0 || resp.ByVia[viaEpicSelf] != 1 || len(resp.ByVia) != 1 {
		t.Errorf("denied: members=%d total=%d replies=%d by_via=%v", n, resp.Total, resp.Replies, resp.ByVia)
	}
	assertKeys(t, m, false, "first_at", "last_at")

	code, _, resp, m = aclGet(t, aclEpicKey, "priv@example.com")
	if code != 200 || resp.Total != 3 || resp.Replies != 1 || len(resp.Members["slack"]) != 1 || len(resp.Members["jira"]) != 1 {
		t.Errorf("allowed: status %d total=%d replies=%d members=%v", code, resp.Total, resp.Replies, resp.Members)
	}
	assertKeys(t, m, false, "first_at", "last_at")

	code, _, resp, m = aclGet(t, aclEpicKey, "")
	if code != 200 || resp.Total != 3 || resp.Replies != 1 {
		t.Errorf("unfiltered: status %d total=%d replies=%d", code, resp.Total, resp.Replies)
	}
	assertKeys(t, m, true, "first_at", "last_at")
}

func TestEpicACL_UnresolvedAsker(t *testing.T) {
	pool := openTestDB(t)
	resetEpicACL(t, pool)
	seedACLEpic(t, pool)
	aclBrief(t, pool, "jira:PAY-201", aclPriv+":1.0")

	code, _, resp, m := aclGet(t, aclEpicKey, "nobody@example.com")
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	if resp.Total != 1 || len(resp.Members["slack"]) != 0 || len(resp.Members["jira"]) != 1 {
		t.Errorf("members=%v total=%d", resp.Members, resp.Total)
	}
	assertKeys(t, m, false, "first_at", "last_at")
	assertKeys(t, m, false, briefKeys...)

	// All-public sources: the brief is served to the unresolved asker.
	aclExec(t, pool, `UPDATE graph.epic_briefs SET sources = ARRAY['jira:PAY-201']`)
	_, _, _, m = aclGet(t, aclEpicKey, "nobody@example.com")
	assertKeys(t, m, true, briefKeys...)
}

func TestEpicACL_EpicScopeDenied(t *testing.T) {
	pool := openTestDB(t)
	resetEpicACL(t, pool)
	seedACLEpic(t, pool)
	aclNode(t, pool, businessRootID, "business", "", `{}`, "2026-07-01")
	aclMember(t, pool, businessRootID, businessRootID, viaEpicSelf)
	aclExec(t, pool, `UPDATE graph.nodes SET scope=$1::text WHERE id IN ('jira:'||$2::text, $3::text)`, aclPriv, aclEpicKey, businessRootID)

	t.Run("epic_denied", func(t *testing.T) {
		code, body, _, _ := aclGet(t, aclEpicKey, "plain@example.com")
		if code != 404 || strings.TrimSpace(body) != "unknown epic "+aclEpicKey {
			t.Errorf("status %d body %q", code, body)
		}
	})
	t.Run("business_denied", func(t *testing.T) {
		code, body, _, _ := aclGet(t, "payments", "plain@example.com")
		if code != 404 || strings.TrimSpace(body) != "unknown epic "+businessRootID {
			t.Errorf("status %d body %q", code, body)
		}
	})
	t.Run("allowed", func(t *testing.T) {
		for _, k := range []string{aclEpicKey, "payments"} {
			if code, body, _, _ := aclGet(t, k, "priv@example.com"); code != 200 {
				t.Errorf("%s: status %d body %s", k, code, body)
			}
		}
	})
}

func TestEpicACL_BriefSources(t *testing.T) {
	pool := openTestDB(t)
	resetEpicACL(t, pool)
	seedACLEpic(t, pool)
	aclNode(t, pool, "jira:PAY-210", "jira", "", `{}`, "2026-08-01")
	aclNode(t, pool, "slack:CDEL:1.0", "slack", aclPriv, `{}`, "2026-08-01")
	aclNode(t, pool, "slack:CDELPUB:1.0", "slack", "public", `{}`, "2026-08-01")
	aclExec(t, pool, `UPDATE graph.nodes SET deleted_at = NOW() WHERE id IN ('slack:CDEL:1.0','slack:CDELPUB:1.0')`)

	cases := []struct {
		name         string
		sources      []string
		deniedServed bool // plain asker
		allowedServe bool // asker with slack:CPRIV
	}{
		{"all_visible", []string{"jira:PAY-201", "jira:PAY-210"}, true, true},
		{"private_source", []string{"jira:PAY-201", aclPriv + ":1.0"}, false, true},
		{"missing_source", []string{"jira:PAY-201", "jira:PAY-NOPE"}, false, false},
		{"deleted_public", []string{"jira:PAY-201", "slack:CDELPUB:1.0"}, true, true},
		{"deleted_private_denied", []string{"jira:PAY-201", "slack:CDEL:1.0"}, false, true},
		{"deleted_private_allowed", []string{"jira:PAY-201", "slack:CDEL:1.0"}, false, true},
		{"unfiltered", []string{"jira:PAY-NOPE", aclPriv + ":1.0"}, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			aclExec(t, pool, `DELETE FROM graph.epic_briefs`)
			aclBrief(t, pool, tc.sources...)
			asker, want := "plain@example.com", tc.deniedServed
			switch tc.name {
			case "deleted_private_allowed":
				asker, want = "priv@example.com", tc.allowedServe
			case "unfiltered":
				asker, want = "", true
			}
			code, _, resp, m := aclGet(t, aclEpicKey, asker)
			if code != 200 || resp.Total == 0 {
				t.Fatalf("status %d total %d (members must be served)", code, resp.Total)
			}
			assertKeys(t, m, want, briefKeys...)
		})
	}
}

func TestEpicACL_BriefLookupError(t *testing.T) {
	pool := openTestDB(t)
	resetEpicACL(t, pool)
	seedACLEpic(t, pool)
	aclBrief(t, pool, "jira:PAY-201")

	orig := epicSourceScopes
	called := false
	epicSourceScopes = func(ctx context.Context, db *pgxpool.Pool, ids []string) (map[string]epicSourceNode, error) {
		called = true
		return nil, errors.New("boom")
	}
	t.Cleanup(func() { epicSourceScopes = orig })

	code, _, resp, m := aclGet(t, aclEpicKey, "plain@example.com")
	if !called || code != http.StatusOK || resp.Total == 0 {
		t.Errorf("called=%v status=%d total=%d", called, code, resp.Total)
	}
	assertKeys(t, m, false, briefKeys...)

	called = false
	code, _, _, m = aclGet(t, aclEpicKey, "")
	if code != 200 || called {
		t.Errorf("unfiltered: status %d lookup called=%v", code, called)
	}
	assertKeys(t, m, true, briefKeys...)
}
