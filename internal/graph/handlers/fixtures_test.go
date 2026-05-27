package handlers_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// loadFixtureTabbyIncident seeds a representative Tabby incident graph:
//   - jira:PAY-2128 (the Jira ticket)
//   - slack:C08S954G2LX:1778119437.328319 (Slack incident thread)
//   - gh_pr:wego/payments#1960 (the fix PR)
//   - cf:3861872666 (Confluence postmortem)
//
// Edges: Slack → Jira (REFERENCES), Jira → PR (REFERENCES), CF → Jira (REFERENCES).
// Bodies are seeded so hydration can load them within budget.
func loadFixtureTabbyIncident(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()

	nodes := []struct {
		id    string
		typ   string
		title string
		body  string
	}{
		{
			"jira:PAY-2128",
			"jira",
			"Tabby installments_count missing — TRY currency failing",
			"Root cause: installments_count field missing in Tabby API response for TRY currency. Enzo filed this after Lei reported auth failures.",
		},
		{
			"slack:C08S954G2LX:1778119437.328319",
			"slack_thread",
			"#payments-geeks: TRY currency Tabby auth failures",
			"Lei: seeing Tabby authorizations failing for TRY. Enzo: filed PAY-2128. looks like installments_count is absent.",
		},
		{
			"gh_pr:wego/payments#1960",
			"gh_pr",
			"fix(tabby): fallback for missing installments_count",
			"This PR adds a fallback when installments_count is absent in the Tabby response for TRY currency.",
		},
		{
			"cf:3861872666",
			"cf_page",
			"Postmortem: Tabby TRY currency incident May 2026",
			"Timeline and root cause analysis for the Tabby TRY installments_count incident. PAY-2128 was the tracking ticket.",
		},
	}

	for _, n := range nodes {
		_, err := pool.Exec(ctx, `
INSERT INTO graph.nodes (id, type, natural_key, title, machine_id)
VALUES ($1, $2, $1, $3, 'test')
ON CONFLICT (id) DO NOTHING`, n.id, n.typ, n.title)
		if err != nil {
			t.Fatalf("loadFixtureTabbyIncident node %s: %v", n.id, err)
		}
		_, err = pool.Exec(ctx, `
INSERT INTO graph.artifact_bodies (node_id, body_full, machine_id)
VALUES ($1, $2, 'test')
ON CONFLICT (node_id) DO UPDATE SET body_full = EXCLUDED.body_full`, n.id, n.body)
		if err != nil {
			t.Fatalf("loadFixtureTabbyIncident body %s: %v", n.id, err)
		}
	}

	edges := [][3]string{
		{"slack:C08S954G2LX:1778119437.328319", "jira:PAY-2128", "REFERENCES"},
		{"jira:PAY-2128", "gh_pr:wego/payments#1960", "REFERENCES"},
		{"cf:3861872666", "jira:PAY-2128", "REFERENCES"},
	}
	for _, e := range edges {
		_, err := pool.Exec(ctx, `
INSERT INTO graph.edges (from_node_id, to_node_id, kind, machine_id)
VALUES ($1, $2, $3, 'test')
ON CONFLICT (from_node_id, to_node_id, kind) DO NOTHING`, e[0], e[1], e[2])
		if err != nil {
			t.Fatalf("loadFixtureTabbyIncident edge %s->%s: %v", e[0], e[1], err)
		}
	}
}
