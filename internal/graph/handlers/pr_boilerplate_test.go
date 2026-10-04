package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/agent-mem/agent-mem/internal/graph/extractor"
	"github.com/agent-mem/agent-mem/internal/graph/fetchers"
	"github.com/agent-mem/agent-mem/internal/graph/identity"
	"github.com/agent-mem/agent-mem/internal/graph/normalizer"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
)

const prTemplateTestLine = "- [ ] For GIN indexes, set `WITH (fastupdate = off)` (see `docs/ai/database-operations.md#gin-indexes-disable-fastupdate`) — the default `fastupdate=on` caused the PAY-2142 incident"

func prBoilerplateTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)
	t.Cleanup(func() { truncateGraphHandlerTables(t, pool) })
	return pool
}

func seedPRBoilerplateBodies(t *testing.T, pool *pgxpool.Pool, count int) {
	t.Helper()
	for i := 1; i <= count; i++ {
		line := prTemplateTestLine
		if i%2 == 0 {
			line = strings.ReplaceAll(strings.ReplaceAll(line, "—", "-"), "[ ]", "~[ ]")
		}
		id := fmt.Sprintf("gh_pr:wego/payments#%d", i)
		if _, err := pool.Exec(context.Background(), `INSERT INTO graph.nodes (id, type, natural_key, body, machine_id) VALUES ($1, 'gh_pr', $2, $3, 'test')`, id, fmt.Sprintf("wego/payments#%d", i), line); err != nil {
			t.Fatal(err)
		}
	}
}

func TestStripPRBoilerplate_DropsTemplateLine(t *testing.T) {
	pool := prBoilerplateTestDB(t)
	seedPRBoilerplateBodies(t, pool, 10)
	body := prTemplateTestLine + "\nFixes PAY-9001"
	if got := stripPRBoilerplate(context.Background(), pool, "gh_pr:wego/payments#11", body); got != "Fixes PAY-9001" {
		t.Fatalf("filtered body = %q, want real reference only", got)
	}
}

func TestStripPRBoilerplate_BelowThresholdKept(t *testing.T) {
	pool := prBoilerplateTestDB(t)
	seedPRBoilerplateBodies(t, pool, 9)
	if got := stripPRBoilerplate(context.Background(), pool, "gh_pr:wego/payments#11", prTemplateTestLine); got != prTemplateTestLine {
		t.Fatalf("below-threshold body changed: %q", got)
	}
}

func TestStripPRBoilerplate_SelfNotCounted(t *testing.T) {
	pool := prBoilerplateTestDB(t)
	seedPRBoilerplateBodies(t, pool, 10)
	if got := stripPRBoilerplate(context.Background(), pool, "gh_pr:wego/payments#10", prTemplateTestLine); got != prTemplateTestLine {
		t.Fatalf("own row counted toward threshold: %q", got)
	}
}

func TestStripPRBoilerplate_NoCandidatesNoQuery(t *testing.T) {
	body := "# Changes\nNo ticket references here.\nPAY-1\n"
	if got := stripPRBoilerplate(context.Background(), nil, "gh_pr:wego/payments#11", body); got != body {
		t.Fatalf("body without candidates changed: %q", got)
	}
}

func TestFetchBody_PRTemplateKeyNotLinked(t *testing.T) {
	pool := prBoilerplateTestDB(t)
	seedPRBoilerplateBodies(t, pool, 10)
	body := prTemplateTestLine + "\nFixes PAY-9001"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/wego/payments/pulls/11" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"number": 11, "title": "Update indexes", "body": body,
			"html_url":   "https://github.com/wego/payments/pull/11",
			"created_at": "2026-10-01T10:00:00Z", "updated_at": "2026-10-03T10:00:00Z",
		}); err != nil {
			t.Error(err)
		}
	}))
	defer srv.Close()
	log := zerolog.Nop()
	deps := Deps{
		DB: pool, Logger: log, MachineID: "test",
		Fetchers:    fetchers.NewRegistry(fetchers.Config{GHBaseURL: srv.URL, GHToken: "t", HTTPClient: srv.Client()}, log),
		Normalizers: normalizer.NewDefault(nil),
		Extractor:   extractor.New(pool, log), Identity: identity.NewService(pool, log),
	}
	if err := NewFetchBodyHandler(deps).Handler(context.Background(), []byte(`{"node_id":"gh_pr:wego/payments#11"}`)); err != nil {
		t.Fatalf("fetch_body: %v", err)
	}
	for _, tc := range []struct {
		key  string
		want bool
	}{{"PAY-2142", false}, {"PAY-9001", true}} {
		var exists bool
		err := pool.QueryRow(context.Background(), `SELECT EXISTS (SELECT 1 FROM graph.edges WHERE from_node_id='gh_pr:wego/payments#11' AND to_node_id=$1 AND kind='REFERENCES')`, "jira:"+tc.key).Scan(&exists)
		if err != nil {
			t.Fatal(err)
		}
		if exists != tc.want {
			t.Errorf("REFERENCES edge to %s exists = %v, want %v", tc.key, exists, tc.want)
		}
	}
	var stored, full string
	if err := pool.QueryRow(context.Background(), `SELECT n.body, b.body_full FROM graph.nodes n JOIN graph.artifact_bodies b ON b.node_id=n.id WHERE n.id='gh_pr:wego/payments#11'`).Scan(&stored, &full); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stored, "PAY-2142") || !strings.Contains(full, "PAY-2142") {
		t.Fatal("filter removed template from persisted full bodies")
	}
}
