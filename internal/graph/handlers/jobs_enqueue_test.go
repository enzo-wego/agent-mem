package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

func TestJobsEnqueue_EpicBriefDryRunOnly(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)
	t.Cleanup(func() { truncateGraphHandlerTables(t, pool) })
	h := NewJobsEnqueueHandler(Deps{DB: pool, Logger: zerolog.Nop(), MachineID: "test"})

	post := func(payload string) int {
		body := `{"type":"refresh_epic_brief","payload":` + payload + `}`
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/graph/jobs/enqueue", strings.NewReader(body)))
		return rec.Code
	}
	queued := func() int {
		var n int
		_ = pool.QueryRow(t.Context(), `SELECT count(*) FROM graph.jobs WHERE type='refresh_epic_brief' AND status='queued'`).Scan(&n)
		return n
	}
	cases := []struct {
		name, payload string
		code, jobs    int
	}{
		{"no_dry_run", `{"epic_key":"PAY-1"}`, 400, 0},
		{"dry_run_false", `{"epic_key":"PAY-1","dry_run":false}`, 400, 0},
		{"no_epic_key", `{"dry_run":true}`, 400, 0},
		{"non_jira_key", `{"epic_key":"business:payments","dry_run":true}`, 400, 0},
		{"dry_run_ok", `{"epic_key":"PAY-1","dry_run":true}`, 200, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			truncateGraphHandlerTables(t, pool)
			if code := post(c.payload); code != c.code {
				t.Fatalf("status = %d, want %d", code, c.code)
			}
			if n := queued(); n != c.jobs {
				t.Fatalf("queued jobs = %d, want %d", n, c.jobs)
			}
		})
	}
}
