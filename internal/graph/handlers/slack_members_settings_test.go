package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
)

func TestSlackMembersSetting(t *testing.T) {
	pool := openTestDB(t)
	ctx := context.Background()
	for _, key := range []string{slackMembersIntervalKey, slackMembersLastAttemptKey} {
		var previous string
		had := pool.QueryRow(ctx, `SELECT value FROM settings WHERE key=$1`, key).Scan(&previous) == nil
		t.Cleanup(func() {
			if had {
				_, _ = pool.Exec(ctx, `INSERT INTO settings(key,value) VALUES($1,$2) ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value`, key, previous)
			} else {
				_, _ = pool.Exec(ctx, `DELETE FROM settings WHERE key=$1`, key)
			}
		})
		if _, err := pool.Exec(ctx, `DELETE FROM settings WHERE key=$1`, key); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `DELETE FROM graph.jobs WHERE type='refresh_slack_members'`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM graph.jobs WHERE type='refresh_slack_members'`)
	})

	// Exercise the same Channels methods and dedicated routes mounted by Mount.
	newRouter := func() http.Handler {
		h := NewChannels(pool)
		r := chi.NewRouter()
		r.Get("/api/graph/slack-members", h.getSlackMembersConfig)
		r.Put("/api/graph/slack-members", h.putSlackMembersConfig)
		return r
	}
	do := func(router http.Handler, method, body string) (int, slackMembersConfig) {
		t.Helper()
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(method, "/api/graph/slack-members", strings.NewReader(body)))
		var cfg slackMembersConfig
		if w.Code == http.StatusOK {
			if err := json.Unmarshal(w.Body.Bytes(), &cfg); err != nil {
				t.Fatalf("decode: %v", err)
			}
		}
		return w.Code, cfg
	}
	router := newRouter()
	if code, cfg := do(router, http.MethodGet, ""); code != http.StatusOK || cfg.IntervalMinutes != 60 {
		t.Fatalf("defaults: status=%d config=%+v", code, cfg)
	}
	for _, interval := range []string{"15", "480", "120"} {
		if code, _ := do(router, http.MethodPut, `{"interval_minutes":`+interval+`}`); code != http.StatusOK {
			t.Fatalf("save %s: status=%d", interval, code)
		}
	}
	if code, cfg := do(newRouter(), http.MethodGet, ""); code != http.StatusOK || cfg.IntervalMinutes != 120 {
		t.Fatalf("reload: status=%d config=%+v", code, cfg)
	}
	var stored string
	if err := pool.QueryRow(ctx, `SELECT value FROM settings WHERE key=$1`, slackMembersIntervalKey).Scan(&stored); err != nil || stored != "120" {
		t.Fatalf("persisted interval=%q err=%v", stored, err)
	}
	for _, body := range []string{
		`{"interval_minutes":14}`, `{"interval_minutes":481}`,
		`{"interval_minutes":15.5}`, `{"interval_minutes":"60"}`,
		`{"interval_minutes":null}`, `{}`, `null`, `{"interval_minutes":`,
		`{"interval_minutes":60} {"interval_minutes":120}`,
	} {
		if code, _ := do(router, http.MethodPut, body); code != http.StatusBadRequest {
			t.Errorf("invalid %s: status=%d, want 400", body, code)
		}
	}
	if _, cfg := do(newRouter(), http.MethodGet, ""); cfg.IntervalMinutes != 120 {
		t.Fatalf("invalid input changed interval: %+v", cfg)
	}

	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(ctx, `INSERT INTO settings(key,value) VALUES($1,$2) ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value`, slackMembersLastAttemptKey, now.Add(-61*time.Minute).Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	count := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM graph.jobs WHERE type='refresh_slack_members'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if err := slackMembersTick(ctx, pool, "settings-test", "any", now); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 0 {
		t.Fatalf("saved 120-minute interval ignored: jobs=%d", n)
	}
	if code, _ := do(router, http.MethodPut, `{"interval_minutes":30}`); code != http.StatusOK {
		t.Fatalf("save shorter interval: status=%d", code)
	}
	if err := slackMembersTick(ctx, pool, "settings-test", "any", now); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 1 {
		t.Fatalf("ticker did not read saved 30-minute interval: jobs=%d", n)
	}
}
