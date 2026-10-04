package handlers_test

import (
	"context"
	"net/http"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/agent-mem/agent-mem/internal/graph/handlers"
)

// breakKeywordArm renames artifact_index.tsv so the keyword arm's SQL errors
// while the semantic arm (embedding column) keeps working; restored on cleanup.
func breakKeywordArm(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `ALTER TABLE graph.artifact_index RENAME COLUMN tsv TO tsv_broken`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := pool.Exec(context.Background(), `ALTER TABLE graph.artifact_index RENAME COLUMN tsv_broken TO tsv`); err != nil {
			t.Errorf("restore tsv: %v", err)
		}
	})
}

func TestSearch_HybridAllArmsFailIs500(t *testing.T) {
	pool := testDB(t)
	seedPANBodies(t, pool)
	breakKeywordArm(t, pool)
	h, err := handlers.NewSearchWithEmbedder(pool, failingEmbedder{})
	if err != nil {
		t.Fatal(err)
	}
	code, _ := shRaw(t, h, "q=PAN&match=hybrid", "")
	if code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500", code)
	}
	// No embedder is a skip: the lone keyword failure still fails the request.
	h, err = handlers.NewSearch(pool)
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := shRaw(t, h, "q=PAN&match=hybrid", ""); code != http.StatusInternalServerError {
		t.Fatalf("no-embedder status %d, want 500", code)
	}
}

func TestSearch_HybridKeywordFailureWithSemanticOK(t *testing.T) {
	pool := testDB(t)
	spNode(t, pool, "jira:PAY-Z", "jira", "Z", "PAN handling", "", "", "{}", 0, false)
	spEmbed(t, pool, "jira:PAY-Z", spUnitVec())
	breakKeywordArm(t, pool)
	h, err := handlers.NewSearchWithEmbedder(pool, fixedSearchEmbedder{vector: spUnitVec()})
	if err != nil {
		t.Fatal(err)
	}
	code, top := shRaw(t, h, "q=PAN&match=hybrid", "")
	if code != http.StatusOK {
		t.Fatalf("status %d, want 200", code)
	}
	errs, _ := top["arm_errors"].(map[string]any)
	if errs["keyword"] == nil || errs["semantic"] != nil {
		t.Errorf("arm_errors = %v, want keyword only", errs)
	}
	if ids := shOrdered(shResults(top)); !reflect.DeepEqual(ids, []string{"jira:PAY-Z"}) {
		t.Errorf("results = %v", ids)
	}
}
