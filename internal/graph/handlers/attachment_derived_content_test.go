package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

func TestSummarizeThread_SkipsJiraAttachment(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)
	ctx := context.Background()
	for _, n := range []struct{ id, typ, title, body, scope string }{
		{"slack:CDERIVED:100.000001", "slack", "", "https://example.com/resource", "slack:CDERIVED"},
		{"jira_attachment:derived", "jira_attachment", "zqxattachtitle", "zqxattachbody", ""},
		{"slack_file:derived", "slack_file", "zqxslacktitle", "zqxslackbody", ""},
		{"jira:derived-control", "jira", "Visible linked ticket", "Visible ticket excerpt", ""},
	} {
		if _, err := pool.Exec(ctx, `INSERT INTO graph.nodes (id,type,natural_key,title,body,scope,machine_id) VALUES ($1,$2,$1,$3,$4,NULLIF($5,''),'test')`, n.id, n.typ, n.title, n.body, n.scope); err != nil {
			t.Fatal(err)
		}
		if n.typ != "slack" {
			if _, err := pool.Exec(ctx, `INSERT INTO graph.edges (from_node_id,to_node_id,kind,machine_id) VALUES ('slack:CDERIVED:100.000001',$1,'REFERENCES','test')`, n.id); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, excerpts := range []bool{false, true} {
		block := linkedResourceBlock(ctx, Deps{DB: pool}, "CDERIVED", "100.000001", excerpts)
		for _, token := range []string{"zqxattachtitle", "zqxattachbody", "zqxslacktitle", "zqxslackbody"} {
			if strings.Contains(block, token) {
				t.Errorf("withExcerpt=%v leaked %q: %s", excerpts, token, block)
			}
		}
		if !strings.Contains(block, "Visible linked ticket") || (excerpts && !strings.Contains(block, "Visible ticket excerpt")) {
			t.Fatalf("withExcerpt=%v missing permitted resource: %s", excerpts, block)
		}
	}
}

func TestAttachmentACL_NoDerivedLeak(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)
	ctx := context.Background()
	const token = "zqxattachsecret"
	const root = "slack:CNOLEAK:100.000001"
	if _, err := pool.Exec(ctx, `DELETE FROM graph.thread_summaries WHERE channel_id='CNOLEAK'`); err != nil {
		t.Fatal(err)
	}
	for _, n := range []struct{ id, typ, title, body, scope string }{
		{root, "slack", "Public thread control", "https://example.com/resource", "slack:CNOLEAK"},
		{"jira_attachment:noleak", "jira_attachment", token + " linked title", token + " linked body", ""},
		{"slack_file:noleak-orphan", "slack_file", token + " orphan title", token + " orphan body", ""},
	} {
		if _, err := pool.Exec(ctx, `INSERT INTO graph.nodes (id,type,natural_key,title,body,scope,metadata,machine_id) VALUES ($1,$2,$1,$3,$4,NULLIF($5,''),'{}','test')`, n.id, n.typ, n.title, n.body, n.scope); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO graph.edges (from_node_id,to_node_id,kind,machine_id) VALUES ($1,'jira_attachment:noleak','REFERENCES','test')`, root); err != nil {
		t.Fatal(err)
	}
	gem := &mockGemini{generateResult: func() (string, error) {
		return `{"topic":"Public thread control","overview":"A public resource was shared.","highlights":[],"decisions":[]}`, nil
	}, embedResult: func() ([]float32, error) {
		vector := make([]float32, GraphEmbeddingDims)
		vector[0] = 1
		return vector, nil
	}}
	deps := Deps{DB: pool, Gemini: gem, Logger: zerolog.Nop(), MachineID: "test"}
	if err := NewSummarizeThreadHandler(deps).Handler(ctx, []byte(`{"channel_id":"CNOLEAK","thread_ts":"100.000001"}`)); err != nil {
		t.Fatal(err)
	}
	if gem.generateUser == "" || !strings.Contains(gem.generateUser, "https://example.com/resource") {
		t.Fatalf("summary prompt did not exercise the thread: %q", gem.generateUser)
	}
	if strings.Contains(gem.generateUser, token) {
		t.Fatalf("attachment token leaked into actual summary prompt: %s", gem.generateUser)
	}
	// Summarization and indexing need the channel scope; the parent becomes
	// public afterward and the attachment is re-parented to a hidden thread, so
	// a named non-member sees the public parent but neither attachment.
	for _, id := range []string{root, "jira_attachment:noleak", "slack_file:noleak-orphan"} {
		payload, err := json.Marshal(map[string]any{"node_id": id, "force": true})
		if err != nil {
			t.Fatal(err)
		}
		if err := NewIndexArtifactHandler(deps).Handler(ctx, payload); err != nil {
			t.Fatal(err)
		}
		if id != root {
			if _, err := pool.Exec(ctx, `UPDATE graph.artifact_index SET identifiers=ARRAY[$2::text] WHERE node_id=$1`, id, token); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE graph.nodes SET scope='public' WHERE id=$1`, root); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO graph.nodes (id,type,natural_key,title,body,scope,metadata,machine_id) VALUES ('slack:CHIDDEN:1','slack','slack:CHIDDEN:1','hidden parent','hidden','slack:CHIDDEN','{}','test')`,
		`DELETE FROM graph.edges WHERE from_node_id='` + root + `' AND to_node_id='jira_attachment:noleak'`,
		`INSERT INTO graph.edges (from_node_id,to_node_id,kind,machine_id) VALUES ('slack:CHIDDEN:1','jira_attachment:noleak','REFERENCES','test')`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	search, err := NewSearch(pool)
	if err != nil {
		t.Fatal(err)
	}
	aclPerson(t, pool, 9301, "plain-derived@example.com")
	for _, mode := range []string{"", "hybrid"} {
		unfiltered := httptest.NewRecorder()
		search.ServeHTTP(unfiltered, adminRequest(http.MethodGet, "/api/graph/search?q="+token+"&match="+mode, nil))
		if unfiltered.Code != http.StatusOK || !strings.Contains(unfiltered.Body.String(), "slack_file:noleak-orphan") || !strings.Contains(unfiltered.Body.String(), "jira_attachment:noleak") {
			t.Fatalf("mode=%q unfiltered attachment controls missing: %s", mode, unfiltered.Body.String())
		}
		for _, query := range []string{token, "Public thread control"} {
			req := adminRequest(http.MethodGet, "/api/graph/search?q="+url.QueryEscape(query)+"&match="+mode, nil)
			setAsker(req, "plain-derived@example.com")
			rr := httptest.NewRecorder()
			search.ServeHTTP(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("mode=%q query=%q status=%d: %s", mode, query, rr.Code, rr.Body.String())
			}
			var response map[string]json.RawMessage
			if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(response["results"]), token) {
				t.Fatalf("mode=%q query=%q leaked attachment-derived result fields: %s", mode, query, rr.Body.String())
			}
			if query != token && !strings.Contains(string(response["results"]), root) {
				t.Fatalf("mode=%q missing public parent control: %s", mode, rr.Body.String())
			}
		}
	}
}
