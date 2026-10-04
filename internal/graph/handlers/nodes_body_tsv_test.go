package handlers

import (
	"context"
	"testing"
)

// TestNodesBodyTSV_Cutoff pins the 20,000-character window of idx_nodes_body_tsv:
// a term inside it is found by the index expression, one past it is not.
func TestNodesBodyTSV_Cutoff(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `INSERT INTO graph.nodes (id, type, natural_key, metadata, machine_id, body) VALUES
		('body:in',  'jira', 'body:in',  '{}', 'test', repeat('x ', 9950) || 'nearterm ' || repeat('y ', 3000)),
		('body:out', 'jira', 'body:out', '{}', 'test', repeat('x ', 10050) || 'farterm '  || repeat('y ', 3000))`); err != nil {
		t.Fatal(err)
	}
	// Positions: 'nearterm' starts at char 19900, 'farterm' at 20100.
	var pos1, pos2 int
	if err := pool.QueryRow(ctx, `SELECT position('nearterm' in (SELECT body FROM graph.nodes WHERE id='body:in')),
		position('farterm' in (SELECT body FROM graph.nodes WHERE id='body:out'))`).Scan(&pos1, &pos2); err != nil {
		t.Fatal(err)
	}
	if pos1 != 19901 || pos2 != 20101 {
		t.Fatalf("fixture positions %d/%d, want 19901/20101 (1-based)", pos1, pos2)
	}
	find := func(term string) int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM graph.nodes
			WHERE to_tsvector('simple'::regconfig, left(coalesce(body, ''), 20000)) @@ websearch_to_tsquery('simple', $1)`, term).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if find("nearterm") != 1 {
		t.Error("term at char 19,900 not found")
	}
	if find("farterm") != 0 {
		t.Error("term at char 20,100 found; cutoff is not 20,000")
	}
}
