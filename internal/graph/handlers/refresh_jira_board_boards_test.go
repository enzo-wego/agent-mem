package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rs/zerolog"
)

func TestPutBusinessRoot_RejectsUnknownBoard(t *testing.T) {
	pool := openTestDB(t)
	var prev string
	hadPrev := pool.QueryRow(context.Background(), `SELECT value FROM settings WHERE key=$1`, businessRootProjectKey).Scan(&prev) == nil
	t.Cleanup(func() {
		if hadPrev {
			_, _ = pool.Exec(context.Background(), `UPDATE settings SET value=$2 WHERE key=$1`, businessRootProjectKey, prev)
		} else {
			_, _ = pool.Exec(context.Background(), `DELETE FROM settings WHERE key=$1`, businessRootProjectKey)
		}
	})
	h := NewChannels(pool)
	put := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.putBusinessRoot(w, httptest.NewRequest("PUT", "/api/graph/business-root", strings.NewReader(body)))
		return w
	}
	w := put(`{"project":"FLT"}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "no Jira board configured for FLT; supported: PAY") {
		t.Fatalf("FLT: %d %s", w.Code, w.Body.String())
	}
	if w := put(`{"project":"PAY"}`); w.Code != http.StatusOK {
		t.Fatalf("PAY: %d %s", w.Code, w.Body.String())
	}
}

func TestRefreshJiraBoard_UnknownProjectNoOp(t *testing.T) {
	pool := winReset(t)
	ctx := context.Background()
	var prev string
	hadPrev := pool.QueryRow(ctx, `SELECT value FROM settings WHERE key=$1`, businessRootProjectKey).Scan(&prev) == nil
	t.Cleanup(func() {
		if hadPrev {
			_, _ = pool.Exec(ctx, `UPDATE settings SET value=$2 WHERE key=$1`, businessRootProjectKey, prev)
		} else {
			_, _ = pool.Exec(ctx, `DELETE FROM settings WHERE key=$1`, businessRootProjectKey)
		}
	})

	var reqs atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqs.Add(1)
		http.Error(w, "unexpected", 500)
	}))
	defer srv.Close()
	t.Setenv("AGENT_MEM_JIRA_BASE_URL", srv.URL)
	t.Setenv("AGENT_MEM_JIRA_EMAIL", "a@b.c")
	t.Setenv("AGENT_MEM_JIRA_TOKEN", "tok")

	today := "2026-10-04T00:00:00Z"
	winNode(t, pool, "jira:PAY-100", "jira", "", today)
	winNode(t, pool, "jira:PAY-101", "jira", "2026-08-03T00:00:00Z", today)
	winNode(t, pool, "jira:FLT-1", "jira", "", today)
	winMap(t, pool, "PAY-100", "PAY-100")
	winMap(t, pool, "PAY-101", "PAY-100")
	winRebuild(t, pool, "PAY-100")
	if _, err := pool.Exec(ctx, `INSERT INTO settings(key,value) VALUES($1,'FLT') ON CONFLICT(key) DO UPDATE SET value='FLT'`, businessRootProjectKey); err != nil {
		t.Fatal(err)
	}

	snapshot := func() string {
		var m, e string
		if err := pool.QueryRow(ctx, `SELECT COALESCE(string_agg(to_jsonb(t)::text, E'\n' ORDER BY to_jsonb(t)::text), '') FROM graph.epic_membership t`).Scan(&m); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(ctx, `SELECT COALESCE(string_agg(to_jsonb(t)::text, E'\n' ORDER BY to_jsonb(t)::text), '') FROM graph.edges t WHERE kind='PART_OF'`).Scan(&e); err != nil {
			t.Fatal(err)
		}
		return m + "\n--\n" + e
	}
	before := snapshot()
	if !strings.Contains(before, "PAY-101") {
		t.Fatalf("fixture produced no PAY rows: %q", before)
	}

	deps := Deps{DB: pool, Logger: zerolog.Nop(), MachineID: "test"}
	if err := refreshJiraBoardHandler(deps)(ctx, nil); err != nil {
		t.Fatalf("job: %v", err)
	}
	if n := reqs.Load(); n != 0 {
		t.Fatalf("stub Jira saw %d requests", n)
	}
	if after := snapshot(); after != before {
		t.Fatalf("hierarchy changed\nbefore:\n%s\nafter:\n%s", before, after)
	}
}
