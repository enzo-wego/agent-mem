package handlers

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// Each canary sits alone in its own field. All data is synthetic.
var maskFixtureCanaries = struct {
	title, summary, body, edgeWhy, overview, decision, question, participant  string
	brief, highlight, openItem, seed, query                                   string
	titleRaw, summaryRaw, bodyRaw, edgeWhyRaw, overviewRaw, decisionRaw       string
	questionRaw, participantRaw, briefRaw, highlightRaw, openItemRaw, seedRaw string
}{
	titleRaw: "masktok title t1@example.com", title: "masktok title [email]",
	summaryRaw: "masktok summary +6581234567", summary: "masktok summary [phone]",
	bodyRaw: "masktok body card:4111111111111111", body: "masktok body card:[card]",
	edgeWhyRaw: "masktok why mailto:w1@example.com", edgeWhy: "masktok why mailto:[email]",
	overviewRaw: "masktok overview o1@example.com", overview: "masktok overview [email]",
	decisionRaw: "masktok decision d1@example.com", decision: "masktok decision [email]",
	questionRaw: "masktok question q1@example.com", question: "masktok question [email]",
	participantRaw: "+6591234567", participant: "[phone]",
	briefRaw: "masktok brief b1@example.com", brief: "masktok brief [email]",
	highlightRaw: "masktok highlight h1@example.com", highlight: "masktok highlight [email]",
	openItemRaw: "masktok open +6581234567", openItem: "masktok open [phone]",
	seedRaw: "person:seed1@example.com", seed: "person:[email]",
}

func maskFixture(t *testing.T) http.Handler {
	t.Helper()
	c := maskFixtureCanaries
	pool := openTestDB(t)
	resetEpicACL(t, pool)
	seedKeywordNode(t, pool, "jira:MSK-1", c.titleRaw, "public", c.summaryRaw)
	aclExec(t, pool, `UPDATE graph.nodes SET body=$2 WHERE id=$1`, "jira:MSK-1", c.bodyRaw)
	seedKeywordNode(t, pool, "jira:MSK-2", "masktok plain", "public", "masktok plain summary")
	seedKeywordNode(t, pool, "slack:CMSK:1.0", "masktok slack", "slack:CMSK", "")
	aclExec(t, pool, `UPDATE graph.nodes SET type='slack', metadata=$2::jsonb WHERE id=$1`, "slack:CMSK:1.0",
		`{"thread_ts":"1.0","author":{"display_name":"`+c.participantRaw+`"}}`)
	aclExec(t, pool, `INSERT INTO graph.edges (from_node_id,to_node_id,kind,metadata,machine_id) VALUES
		('jira:MSK-1','jira:MSK-2','REFERENCES',$1::jsonb,'test'),
		('jira:MSK-1','slack:CMSK:1.0','REFERENCES','{}','test')`, `{"why":"`+c.edgeWhyRaw+`"}`)
	delSummaries := func() { aclExec(t, pool, `DELETE FROM graph.thread_summaries WHERE channel_id='CMSK'`) }
	delSummaries()
	t.Cleanup(delSummaries)
	aclExec(t, pool, `INSERT INTO graph.thread_summaries(channel_id,thread_ts,signature,summary,overview,decisions,open_questions)
		VALUES ('CMSK','1.0','1:1','masktok thread summary',$1,$2::jsonb,$3::jsonb)`,
		c.overviewRaw, `[{"text":"`+c.decisionRaw+`","by":"x","date":"2026-01-01","ts":"1.0"}]`, `["`+c.questionRaw+`"]`)
	for _, id := range []string{"jira:MSK-1", "jira:MSK-2", "slack:CMSK:1.0"} {
		body := "masktok plain body"
		if id == "jira:MSK-1" {
			body = c.bodyRaw
		}
		aclExec(t, pool, `INSERT INTO graph.artifact_bodies(node_id,body_full,machine_id) VALUES ($1,$2,'test')`, id, body)
	}
	aclMember(t, pool, "jira:MSK-1", "MSK-1", viaEpicSelf)
	aclMember(t, pool, "slack:CMSK:1.0", "MSK-1", viaKey)
	aclExec(t, pool, `INSERT INTO graph.epic_briefs (epic_key, brief, highlights, open_items, sources) VALUES ('MSK-1',$1,$2::jsonb,$3::jsonb,$4)`,
		c.briefRaw, `[{"text":"`+c.highlightRaw+`","sources":[]}]`, `[{"text":"`+c.openItemRaw+`","sources":[]}]`, []string{"jira:MSK-1"})
	aclPerson(t, pool, 5701, "maskasker@example.com", "slack:CMSK")
	return mountedGraph(t, pool)
}

func TestMaskRoutes_NamedAskerMaskedAdminRaw(t *testing.T) {
	h := maskFixture(t)
	c := maskFixtureCanaries
	node := url.QueryEscape("jira:MSK-1")
	q := url.QueryEscape("masktok")
	type route struct {
		name, method, target, body string
		want, raw                  []string // masked strings that must appear; raw strings that must vanish
		structural                 []string
	}
	routes := []route{
		{"search_legacy", "GET", "/api/graph/search?q=" + q + "&limit=50", "",
			[]string{c.title, c.summary}, []string{c.titleRaw, c.summaryRaw}, []string{"jira:MSK-1"}},
		{"search_hybrid", "GET", "/api/graph/search?q=" + q + "&limit=50&match=hybrid", "",
			[]string{c.title}, []string{c.titleRaw}, []string{"jira:MSK-1"}},
		{"search_query_echo", "GET", "/api/graph/search?q=" + url.QueryEscape("masktok qecho@example.com") + "&limit=5", "",
			[]string{"[email]"}, []string{"qecho@example.com"}, nil},
		{"resolve_bodies", "POST", "/api/graph/resolve", `{"seeds":["jira:MSK-1","` + c.seedRaw + `"],"depth":2,"include_bodies":true}`,
			[]string{c.title, c.body, c.edgeWhy, c.seed}, []string{c.titleRaw, c.bodyRaw, c.edgeWhyRaw, c.seedRaw, "seed1@example.com"}, []string{"jira:MSK-1", "slack:CMSK:1.0"}},
		{"node", "GET", "/api/graph/node?id=" + node, "",
			[]string{c.title, c.summary}, []string{c.titleRaw, c.summaryRaw}, []string{"jira:MSK-1", "jira:MSK-2", "slack:CMSK:1.0"}},
		{"neighbors", "GET", "/api/graph/node/" + url.PathEscape("jira:MSK-1") + "/neighbors?depth=2", "",
			[]string{c.edgeWhy, c.overview, c.decision, c.question}, []string{c.edgeWhyRaw, c.overviewRaw, c.decisionRaw, c.questionRaw}, []string{"slack:CMSK:1.0", "jira:MSK-2"}},
		{"neighbors_cards", "GET", "/api/graph/node/" + url.PathEscape("jira:MSK-1") + "/neighbors?depth=2&cards=1", "",
			[]string{c.edgeWhy, c.overview, c.participant}, []string{c.edgeWhyRaw, c.overviewRaw, c.participantRaw}, []string{"slack:CMSK:1.0"}},
		{"epic", "GET", "/api/graph/epic/MSK-1", "",
			[]string{c.brief, c.highlight, c.openItem, c.title}, []string{c.briefRaw, c.highlightRaw, c.openItemRaw, c.titleRaw}, []string{"jira:MSK-1", "slack:CMSK:1.0"}},
	}
	asker := map[string]string{"X-Asker-User": "maskasker@example.com", "Content-Type": "application/json"}
	admin := map[string]string{adminHeader: "1", "Content-Type": "application/json"}
	for _, rt := range routes {
		t.Run(rt.name, func(t *testing.T) {
			w := graphDo(h, rt.method, rt.target, rt.body, asker)
			if w.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			body := w.Body.String()
			for _, s := range rt.want {
				if !strings.Contains(body, s) {
					t.Errorf("masked field %q missing: %s", s, body)
				}
			}
			for _, s := range rt.raw {
				if strings.Contains(body, s) {
					t.Errorf("leaked %q: %s", s, body)
				}
			}
			for _, s := range rt.structural {
				if !strings.Contains(body, s) {
					t.Errorf("structural id %q missing: %s", s, body)
				}
			}
			aw := graphDo(h, rt.method, rt.target, rt.body, admin)
			if aw.Code != http.StatusOK {
				t.Fatalf("admin status=%d body=%s", aw.Code, aw.Body.String())
			}
			for _, s := range rt.raw {
				if !strings.Contains(aw.Body.String(), s) {
					t.Errorf("admin should see raw %q: %s", s, aw.Body.String())
				}
			}
			if strings.Contains(aw.Body.String(), "[email]") || strings.Contains(aw.Body.String(), "[phone]") || strings.Contains(aw.Body.String(), "[card]") {
				t.Errorf("admin response masked: %s", aw.Body.String())
			}
		})
	}
}
