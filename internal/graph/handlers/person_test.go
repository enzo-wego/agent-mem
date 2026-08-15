package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// personTestDB opens the guarded scratch test database (openTestDB refuses any
// DSN whose db name lacks "test") and clears the graph tables before each test.
func personTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)
	// truncateGraphHandlerTables does not cover the derived-role cache; clear it
	// too so a prior run's row (keyed by eeid) cannot collide on re-run.
	if _, err := pool.Exec(context.Background(), "DELETE FROM graph.person_derived_roles"); err != nil {
		t.Fatalf("truncate person_derived_roles: %v", err)
	}
	return pool
}

type personSeed struct {
	eeid          *int
	displayName   string
	email         string
	slackUserID   string
	githubLogin   string
	isBot         bool
	depthFromRoot *int
	reportsTo     *int
	mergedInto    *int64
}

func seedPerson(t *testing.T, pool *pgxpool.Pool, p personSeed) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(), `
INSERT INTO graph.people
  (eeid, display_name, email, slack_user_id, github_login,
   is_bot, depth_from_root, reports_to, merged_into, machine_id)
VALUES ($1, $2, NULLIF($3,'')::citext, NULLIF($4,''), NULLIF($5,''),
        $6, $7, $8, $9, 'test')
RETURNING id`,
		p.eeid, p.displayName, p.email, p.slackUserID, p.githubLogin,
		p.isBot, p.depthFromRoot, p.reportsTo, p.mergedInto).Scan(&id)
	if err != nil {
		t.Fatalf("seedPerson %q: %v", p.displayName, err)
	}
	return id
}

func seedDerivedRole(t *testing.T, pool *pgxpool.Pool, eeid int, domain, label string, confidence float64, evidence string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
INSERT INTO graph.person_derived_roles
  (eeid, domain, role_label, confidence, evidence, machine_id)
VALUES ($1, $2, $3, $4, $5::jsonb, 'test')`,
		eeid, domain, label, confidence, evidence)
	if err != nil {
		t.Fatalf("seedDerivedRole %d: %v", eeid, err)
	}
}

func seedAuthoredNode(t *testing.T, pool *pgxpool.Pool, id, typ, title string, author int64) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
INSERT INTO graph.nodes (id, type, natural_key, title, author_person_id, machine_id)
VALUES ($1, $2, $1, $3, $4, 'test')`, id, typ, title, author)
	if err != nil {
		t.Fatalf("seedAuthoredNode %s: %v", id, err)
	}
}

func intPtr(v int) *int       { return &v }
func int64Ptr(v int64) *int64 { return &v }

// personLookup issues a GET /api/graph/person and returns status, raw body, and
// the decoded response (decoded only on 200).
func personLookup(t *testing.T, pool *pgxpool.Pool, rawQuery string) (int, string, personResponse) {
	t.Helper()
	h := NewPerson(pool)
	r := httptest.NewRequest("GET", "/api/graph/person?"+rawQuery, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	body := w.Body.String()
	var resp personResponse
	if w.Code == http.StatusOK {
		if err := json.Unmarshal([]byte(body), &resp); err != nil {
			t.Fatalf("decode body %q: %v", body, err)
		}
	}
	return w.Code, body, resp
}

func queryFor(q string) string {
	return url.Values{"q": {q}}.Encode()
}

func TestPerson_NameMatch(t *testing.T) {
	pool := personTestDB(t)
	id := seedPerson(t, pool, personSeed{
		eeid: intPtr(982), displayName: "Lei Zheng",
		email: "lei@wego.com", slackUserID: "UUK3WPNNQ", depthFromRoot: intPtr(3),
	})
	seedDerivedRole(t, pool, 982, "payments", "backend engineer", 0.9, `{"activity_share":0.9}`)
	seedAuthoredNode(t, pool, "slack:C08:1", "slack", "TRY currency thread", id)

	code, _, resp := personLookup(t, pool, queryFor("Lei"))
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if resp.Total != 1 || len(resp.People) != 1 {
		t.Fatalf("total=%d people=%d", resp.Total, len(resp.People))
	}
	p := resp.People[0]
	if p.PersonID != id || p.DisplayName != "Lei Zheng" {
		t.Errorf("person = id %d %q, want id %d Lei Zheng", p.PersonID, p.DisplayName, id)
	}
	if p.EEID == nil || *p.EEID != 982 {
		t.Errorf("eeid = %v, want 982", p.EEID)
	}
	if p.DerivedRole == nil || p.DerivedRole.Domain != "payments" || p.DerivedRole.RoleLabel != "backend engineer" {
		t.Errorf("derived_role = %#v", p.DerivedRole)
	}
	if len(p.RecentArtifacts) != 1 || p.RecentArtifacts[0].ID != "slack:C08:1" {
		t.Errorf("recent_artifacts = %#v", p.RecentArtifacts)
	}
}

func TestPerson_EmailMatchIsCaseInsensitive(t *testing.T) {
	pool := personTestDB(t)
	id := seedPerson(t, pool, personSeed{displayName: "Ann Example", email: "ann@wego.com"})

	// CITEXT: an exact-but-differently-cased email still resolves.
	code, _, resp := personLookup(t, pool, queryFor("Ann@Wego.com"))
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if len(resp.People) != 1 || resp.People[0].PersonID != id {
		t.Fatalf("people = %#v, want id %d", resp.People, id)
	}
	if resp.People[0].Email != "ann@wego.com" {
		t.Errorf("email = %q", resp.People[0].Email)
	}
}

func TestPerson_EEIDMatch(t *testing.T) {
	pool := personTestDB(t)
	id := seedPerson(t, pool, personSeed{eeid: intPtr(555), displayName: "Bob Digits"})
	// A name substring that also matches must not shadow the eeid path.
	seedPerson(t, pool, personSeed{eeid: intPtr(556), displayName: "555 Not This"})

	code, _, resp := personLookup(t, pool, queryFor("555"))
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if len(resp.People) != 1 || resp.People[0].PersonID != id {
		t.Fatalf("people = %#v, want id %d", resp.People, id)
	}
}

func TestPerson_SlackIDMatch(t *testing.T) {
	pool := personTestDB(t)
	id := seedPerson(t, pool, personSeed{displayName: "Chu Yeow", slackUserID: "U07ABC123"})

	code, _, resp := personLookup(t, pool, queryFor("U07ABC123"))
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if len(resp.People) != 1 || resp.People[0].PersonID != id {
		t.Fatalf("people = %#v, want id %d", resp.People, id)
	}
}

func TestPerson_MergedResolvesToCanonical(t *testing.T) {
	pool := personTestDB(t)
	canonical := seedPerson(t, pool, personSeed{eeid: intPtr(700), displayName: "Ross Canonical"})
	// The looked-up attribute (email) lives on the merged row; the response must
	// still be the canonical person.
	seedPerson(t, pool, personSeed{
		displayName: "Ross Old", email: "ross@wego.com", mergedInto: int64Ptr(canonical),
	})

	code, _, resp := personLookup(t, pool, queryFor("ross@wego.com"))
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if len(resp.People) != 1 {
		t.Fatalf("people = %#v", resp.People)
	}
	if resp.People[0].PersonID != canonical || resp.People[0].DisplayName != "Ross Canonical" {
		t.Errorf("resolved to id %d %q, want canonical id %d Ross Canonical",
			resp.People[0].PersonID, resp.People[0].DisplayName, canonical)
	}
}

func TestPerson_ManagerChainWithCycleGuard(t *testing.T) {
	pool := personTestDB(t)
	// Alice(1) reports to Bob(2); Bob(2) reports to Alice(1) — a reporting cycle.
	seedPerson(t, pool, personSeed{eeid: intPtr(1), displayName: "Alice Cycle", reportsTo: intPtr(2)})
	seedPerson(t, pool, personSeed{eeid: intPtr(2), displayName: "Bob Cycle", reportsTo: intPtr(1)})

	code, _, resp := personLookup(t, pool, queryFor("Alice Cycle"))
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if len(resp.People) != 1 {
		t.Fatalf("people = %#v", resp.People)
	}
	p := resp.People[0]
	// The cycle guard stops after Bob; it must not loop back to Alice.
	if len(p.ManagerChain) != 1 || p.ManagerChain[0].EEID != 2 || p.ManagerChain[0].DisplayName != "Bob Cycle" {
		t.Errorf("manager_chain = %#v, want exactly [Bob(2)]", p.ManagerChain)
	}
	if p.DirectReports != 1 {
		t.Errorf("direct_reports = %d, want 1", p.DirectReports)
	}
}

func TestPerson_NoMatchReturnsEmptyList(t *testing.T) {
	pool := personTestDB(t)
	seedPerson(t, pool, personSeed{displayName: "Somebody Else"})

	code, body, resp := personLookup(t, pool, queryFor("Zzznobody"))
	if code != http.StatusOK {
		t.Fatalf("status %d body %s", code, body)
	}
	if resp.Total != 0 || len(resp.People) != 0 {
		t.Fatalf("total=%d people=%#v", resp.Total, resp.People)
	}
	// Empty must serialize as [], never null.
	if !strings.Contains(body, `"people":[]`) {
		t.Errorf("body = %s, want empty people array", body)
	}
}

func TestPerson_MissingDerivedRoleOmitted(t *testing.T) {
	pool := personTestDB(t)
	seedPerson(t, pool, personSeed{eeid: intPtr(321), displayName: "No Role Person"})

	code, body, resp := personLookup(t, pool, queryFor("No Role Person"))
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if len(resp.People) != 1 {
		t.Fatalf("people = %#v", resp.People)
	}
	p := resp.People[0]
	if p.DerivedRole != nil {
		t.Errorf("derived_role = %#v, want nil", p.DerivedRole)
	}
	if p.ManagerChain == nil || len(p.ManagerChain) != 0 {
		t.Errorf("manager_chain = %#v, want empty non-nil", p.ManagerChain)
	}
	if strings.Contains(body, `"derived_role"`) {
		t.Errorf("body = %s, derived_role should be omitted", body)
	}
	if !strings.Contains(body, `"manager_chain":[]`) {
		t.Errorf("body = %s, manager_chain should be []", body)
	}
}

func TestPerson_BadRequests(t *testing.T) {
	pool := personTestDB(t)
	cases := map[string]string{
		"empty q":       "q=",
		"blank q":       queryFor("   "),
		"limit zero":    "q=Lei&limit=0",
		"limit too big": "q=Lei&limit=99",
		"limit nan":     "q=Lei&limit=abc",
	}
	for name, rawQuery := range cases {
		t.Run(name, func(t *testing.T) {
			code, _, _ := personLookup(t, pool, rawQuery)
			if code != http.StatusBadRequest {
				t.Errorf("%s: status %d, want 400", name, code)
			}
		})
	}
}
