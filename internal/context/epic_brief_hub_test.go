package context

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/agent-mem/agent-mem/internal/config"
)

// emptyLocalPool opens the throwaway test DB (read-only use) and checks that
// the two local tables hold nothing for PAY-1234, i.e. the laptop situation.
func emptyLocalPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	u, err := url.Parse(dsn)
	if dsn == "" || err != nil || strings.TrimPrefix(u.Path, "/") != "agentmem_test" {
		t.Fatalf("DATABASE_URL must point at the agentmem_test database, got %q", dsn)
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var n int
	if err := pool.QueryRow(context.Background(), `
SELECT (SELECT count(*) FROM graph.jira_epic_map WHERE issue_key='PAY-1234')
     + (SELECT count(*) FROM graph.epic_briefs)`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("local jira_epic_map/epic_briefs not empty (%d rows)", n)
	}
	return pool
}

func branchRepo(t *testing.T, branch string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(dir+"/.git", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir+"/.git/HEAD", []byte("ref: refs/heads/"+branch+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestEpicBriefForCwd_HubFallback(t *testing.T) {
	pool := emptyLocalPool(t)
	repo := branchRepo(t, "feat/PAY-1234-x")

	run := func(t *testing.T, h http.HandlerFunc, syncURL bool) (string, *int32, *http.Request, time.Duration) {
		t.Helper()
		var hits int32
		var seen http.Request
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&hits, 1)
			seen = *r.Clone(r.Context())
			h(w, r)
		}))
		t.Cleanup(srv.Close)
		cfg := &config.Config{APIKey: "k-secret"}
		if syncURL {
			cfg.SyncURL = srv.URL
		}
		start := time.Now()
		out := epicBriefForCwd(context.Background(), pool, cfg, repo)
		return out, &hits, &seen, time.Since(start)
	}

	t.Run("served", func(t *testing.T) {
		out, hits, seen, _ := run(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"epic_key":"PAY-1000","title":"Epic thousand","brief":"Hub brief text.","resolved_from":"PAY-1234"}`))
		}, true)
		if !strings.Contains(out, "Hub brief text.") || !strings.Contains(out, "PAY-1000") || !strings.Contains(out, "Epic thousand") {
			t.Fatalf("context = %q", out)
		}
		if *hits != 1 || seen.URL.Path != "/api/graph/epic/PAY-1234" ||
			seen.Header.Get("Authorization") != "Bearer k-secret" || seen.Header.Get("X-Asker-User") != "" {
			t.Fatalf("hits=%d path=%q headers=%v", *hits, seen.URL.Path, seen.Header)
		}
	})
	t.Run("slow", func(t *testing.T) {
		out, _, _, took := run(t, func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(3 * time.Second)
			_, _ = w.Write([]byte(`{"epic_key":"PAY-1000","brief":"late"}`))
		}, true)
		if out != "" || took > 1700*time.Millisecond {
			t.Fatalf("out=%q took=%v", out, took)
		}
	})
	t.Run("no_sync_url", func(t *testing.T) {
		out, hits, _, _ := run(t, func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"epic_key":"PAY-1000","brief":"x"}`))
		}, false)
		if out != "" || *hits != 0 {
			t.Fatalf("out=%q hits=%d", out, *hits)
		}
	})
	t.Run("non_200", func(t *testing.T) {
		out, hits, _, _ := run(t, func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		}, true)
		if out != "" || *hits != 1 {
			t.Fatalf("out=%q hits=%d", out, *hits)
		}
	})
}
