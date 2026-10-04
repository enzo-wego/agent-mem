package handlers

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// seedAliasEpic: epic PAY-1000 with a public issue, a private thread (root +
// reply) and the issue PAY-1234 that maps to it. As in production the business
// root holds a membership row for every one of them.
func seedAliasEpic(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	aclNode(t, pool, businessRootID, "business", "", `{}`, "2026-07-01")
	aclMember(t, pool, businessRootID, businessRootID, viaEpicSelf)
	aclNode(t, pool, "jira:PAY-1000", "jira", "", `{}`, "2026-07-01")
	aclMember(t, pool, "jira:PAY-1000", "PAY-1000", viaEpicSelf)
	aclNode(t, pool, "jira:PAY-1234", "jira", "", `{}`, "2026-09-10")
	aclMember(t, pool, "jira:PAY-1234", "PAY-1000", viaEpicSelf)
	aclNode(t, pool, aclPriv+":1.0", "slack", aclPriv, `{"thread_ts":"1.0"}`, "2026-07-01")
	aclMember(t, pool, aclPriv+":1.0", "PAY-1000", viaKey)
	aclNode(t, pool, aclPriv+":1.5", "slack", aclPriv, `{"thread_ts":"1.0"}`, "2026-07-02")
	aclMember(t, pool, aclPriv+":1.5", "PAY-1000", viaKey)
	for _, id := range []string{"jira:PAY-1000", "jira:PAY-1234", aclPriv + ":1.0", aclPriv + ":1.5"} {
		aclMember(t, pool, id, businessRootID, viaEpicSelf)
	}
	aclExec(t, pool, `INSERT INTO graph.jira_epic_map (issue_key, epic_key, epic_summary, machine_id)
	                  VALUES ('PAY-1234','PAY-1000','Epic thousand','test')`)
	aclPerson(t, pool, 41, "plain@example.com")
	aclPerson(t, pool, 42, "priv@example.com", aclPriv)
}

func TestEpicEndpoint_ResolvesIssueKey(t *testing.T) {
	pool := openTestDB(t)
	resetEpicACL(t, pool)
	seedAliasEpic(t, pool)

	t.Run("resolves", func(t *testing.T) {
		code, _, resp, _ := aclGet(t, "PAY-1234", "")
		if code != 200 || resp.EpicKey != "PAY-1000" || resp.NodeID != "jira:PAY-1000" || resp.ResolvedFrom != "PAY-1234" {
			t.Fatalf("status %d resp %+v", code, resp)
		}
		_, _, direct, _ := aclGet(t, "PAY-1000", "")
		if direct.ResolvedFrom != "" || direct.Total != resp.Total || direct.Total == 0 {
			t.Fatalf("direct total %d resolved_from %q vs alias total %d", direct.Total, direct.ResolvedFrom, resp.Total)
		}
	})
	t.Run("unknown", func(t *testing.T) {
		code, body, _, _ := aclGet(t, "PAY-9999", "")
		if code != 404 || strings.TrimSpace(body) != "unknown epic PAY-9999" {
			t.Fatalf("status %d body %q", code, body)
		}
	})
	t.Run("alias_denied", func(t *testing.T) {
		aclExec(t, pool, `UPDATE graph.nodes SET scope=$1::text WHERE id = 'jira:PAY-1000'`, aclPriv)
		t.Cleanup(func() { aclExec(t, pool, `UPDATE graph.nodes SET scope=NULL WHERE id = 'jira:PAY-1000'`) })
		code, body, _, _ := aclGet(t, "PAY-1234", "plain@example.com")
		if code != 404 || strings.TrimSpace(body) != "unknown epic PAY-1234" || strings.Contains(body, "PAY-1000") {
			t.Fatalf("status %d body %q", code, body)
		}
		if code, _, resp, _ := aclGet(t, "pay-1234", "priv@example.com"); code != 200 || resp.ResolvedFrom != "PAY-1234" {
			t.Fatalf("allowed asker: status %d resp %+v", code, resp)
		}
	})
	t.Run("alias_unresolved_asker", func(t *testing.T) {
		code, _, resp, m := aclGet(t, "PAY-1234", "nobody@example.com")
		if code != 200 || resp.Total != 1 || len(resp.Members["slack"]) != 0 || len(resp.Members["jira"]) != 1 {
			t.Fatalf("status %d total=%d members=%v", code, resp.Total, resp.Members)
		}
		assertKeys(t, m, false, "first_at", "last_at")
	})
	t.Run("alias_filtered_members", func(t *testing.T) {
		_, _, plain, _ := aclGet(t, "PAY-1234", "plain@example.com")
		if plain.Total != 1 || plain.Replies != 0 || len(plain.Members["slack"]) != 0 {
			t.Fatalf("plain asker saw the private thread: %+v", plain)
		}
		_, _, priv, _ := aclGet(t, "PAY-1234", "priv@example.com")
		if priv.Total != 3 || priv.Replies != 1 || len(priv.Members["slack"]) != 1 {
			t.Fatalf("scoped asker: total=%d replies=%d members=%v", priv.Total, priv.Replies, priv.Members)
		}
	})
	t.Run("alias_brief_withheld", func(t *testing.T) {
		aclExec(t, pool, `INSERT INTO graph.epic_briefs (epic_key, brief, highlights, open_items, sources)
			VALUES ('PAY-1000','the brief','[{"text":"h","sources":[]}]','[{"text":"o","sources":[]}]',$1)`,
			[]string{"jira:PAY-1234", aclPriv + ":1.0"})
		_, _, _, m := aclGet(t, "PAY-1234", "plain@example.com")
		assertKeys(t, m, false, briefKeys...)
		_, _, _, m = aclGet(t, "PAY-1234", "priv@example.com")
		assertKeys(t, m, true, briefKeys...)
	})
}
