package handlers

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/rs/zerolog"
)

func TestIndexArtifact_TitleOnlyAndDistinct(t *testing.T) {
	pool, rec := decisionIndexTestDB(t)
	ctx := context.Background()
	deps := Deps{DB: pool, Gemini: rec, Logger: zerolog.Nop(), MachineID: "test"}
	for _, n := range []struct{ id, typ, title, body string }{
		{"jira:PAY-2307", "jira", "India GST", ""},
		{"jira:PAY-2308", "jira", "Fix refunds", "Background\n\nFix duplicate refunds"},
		{"jira:PAY-2309", "jira", "Avoid retries", "Background\n\nFix duplicate refunds"},
		{"jira:PAY-2310", "jira", "Repeated alert", "Background\n\nSame failure"},
		{"jira:PAY-2311", "jira", "Repeated alert", "Background\n\nSame failure"},
		{"slack:CEMPTY:1", "slack", "Ignored title", ""},
	} {
		if _, err := pool.Exec(ctx, `INSERT INTO graph.nodes(id,type,natural_key,title,body,machine_id) VALUES($1,$2,$1,$3,$4,'test')`, n.id, n.typ, n.title, n.body); err != nil {
			t.Fatal(err)
		}
		p, _ := json.Marshal(indexArtifactPayload{NodeID: n.id, Force: true, SkipJudging: true})
		if err := NewIndexArtifactHandler(deps).Handler(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	var summary string
	if err := pool.QueryRow(ctx, `SELECT summary FROM graph.artifact_index WHERE node_id='jira:PAY-2307'`).Scan(&summary); err != nil {
		t.Fatal(err)
	}
	if summary != "India GST" {
		t.Fatalf("title-only summary=%q", summary)
	}
	var distinct, embedded int
	if err := pool.QueryRow(ctx, `SELECT count(DISTINCT summary),count(embedding) FROM graph.artifact_index WHERE node_id IN ('jira:PAY-2308','jira:PAY-2309')`).Scan(&distinct, &embedded); err != nil {
		t.Fatal(err)
	}
	if distinct != 2 || embedded != 2 {
		t.Fatalf("distinct/embedded=%d/%d, want 2/2", distinct, embedded)
	}
	var slackCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM graph.artifact_index WHERE node_id='slack:CEMPTY:1'`).Scan(&slackCount); err != nil {
		t.Fatal(err)
	}
	if slackCount != 0 {
		t.Fatalf("empty Slack indexed: %d", slackCount)
	}
	for _, id := range []string{"jira:PAY-2311", "jira:PAY-2310"} {
		p, _ := json.Marshal(indexArtifactPayload{NodeID: id, Force: true, SkipJudging: true})
		if err := NewIndexArtifactHandler(deps).Handler(ctx, p); err != nil {
			t.Fatal(err)
		}
		var count int
		if err := pool.QueryRow(ctx, `SELECT count(embedding) FROM graph.artifact_index WHERE node_id IN ('jira:PAY-2310','jira:PAY-2311')`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("duplicate reindex %s representatives=%d", id, count)
		}
	}
	for _, id := range []string{"jira:PAY-2307", "jira:PAY-2310", "jira:PAY-2311"} {
		var ids []string
		if err := pool.QueryRow(ctx, `SELECT identifiers FROM graph.artifact_index WHERE node_id=$1`, id).Scan(&ids); err != nil {
			t.Fatal(err)
		}
		if len(ids) != 1 || ids[0] != id[len("jira:"):] {
			t.Fatalf("%s own identifiers=%v", id, ids)
		}
	}
}
