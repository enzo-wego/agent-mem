package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestSubscriptionGuards exercises the create/update HTTP handlers against the
// scratch DB: the lone-message channel-scope guard on both create and update
// (criterion 6), a sources-only update behaving exactly as before (criterion 5),
// and the newly editable min_participants / scope_definition / active fields.
func TestSubscriptionGuards(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping integration test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	for _, tbl := range []string{"graph.topic_notifications", "graph.topic_subscriptions"} {
		if _, err := pool.Exec(ctx, "DELETE FROM "+tbl); err != nil {
			t.Fatalf("clean %s: %v", tbl, err)
		}
	}

	subs := NewSubscriptions(Deps{DB: pool, SlackDMUserID: "UTEST"})
	r := chi.NewRouter()
	r.Post("/api/graph/subscriptions", subs.create)
	r.Patch("/api/graph/subscriptions/{id}", subs.update)
	srv := httptest.NewServer(r)
	defer srv.Close()

	post := func(t *testing.T, body string) (int, string) {
		t.Helper()
		resp, err := http.Post(srv.URL+"/api/graph/subscriptions", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, strings.TrimSpace(string(b))
	}
	patch := func(t *testing.T, id int64, body string) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPatch,
			srv.URL+"/api/graph/subscriptions/"+strconv.FormatInt(id, 10), strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("PATCH: %v", err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, strings.TrimSpace(string(b))
	}
	idOf := func(t *testing.T, jsonBody string) int64 {
		t.Helper()
		var s struct {
			ID int64 `json:"id"`
		}
		if err := json.Unmarshal([]byte(jsonBody), &s); err != nil {
			t.Fatalf("decode created sub: %v (body=%q)", err, jsonBody)
		}
		return s.ID
	}

	// Criterion 6: min_participants=1 with an empty channel_filter → 400.
	if code, body := post(t, `{"topic":"payments","subscriber_slack_id":"UX","min_participants":1}`); code != http.StatusBadRequest {
		t.Errorf("unscoped min_participants=1 create: status=%d body=%q, want 400", code, body)
	} else if !strings.Contains(body, "channel_filter") {
		t.Errorf("400 body should mention channel_filter, got %q", body)
	}

	// Criterion 6: min_participants=1 WITH a channel_filter → 200, persisted.
	code, body := post(t, `{"topic":"payments","subscriber_slack_id":"UX","min_participants":1,"channel_filter":["C12345"]}`)
	if code != http.StatusOK {
		t.Fatalf("scoped min_participants=1 create: status=%d body=%q, want 200", code, body)
	}
	scopedID := idOf(t, body)
	var mp int
	if err := pool.QueryRow(ctx, `SELECT min_participants FROM graph.topic_subscriptions WHERE id=$1`, scopedID).Scan(&mp); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if mp != 1 {
		t.Errorf("persisted min_participants=%d, want 1", mp)
	}

	// A default (unscoped) sub for the update-guard case.
	code, body = post(t, `{"topic":"deploys","subscriber_slack_id":"UX"}`)
	if code != http.StatusOK {
		t.Fatalf("default create: status=%d body=%q", code, body)
	}
	unscopedID := idOf(t, body)

	// Criterion 5: a sources-only update behaves as before → 204.
	if code, body := patch(t, scopedID, `{"sources":[{"type":"confluence","url":"https://x/wiki/1"}]}`); code != http.StatusNoContent {
		t.Errorf("sources-only update: status=%d body=%q, want 204", code, body)
	}

	// Criterion 6 (update): lowering to min_participants=1 on an unscoped sub → 400.
	if code, body := patch(t, unscopedID, `{"min_participants":1}`); code != http.StatusBadRequest {
		t.Errorf("unscoped update min_participants=1: status=%d body=%q, want 400", code, body)
	} else if !strings.Contains(body, "channel_filter") {
		t.Errorf("update 400 body should mention channel_filter, got %q", body)
	}

	// Update on the scoped sub to min_participants=1 → 204.
	if code, body := patch(t, scopedID, `{"min_participants":1}`); code != http.StatusNoContent {
		t.Errorf("scoped update min_participants=1: status=%d body=%q, want 204", code, body)
	}

	// The new editable fields persist.
	if code, body := patch(t, unscopedID, `{"min_participants":3,"scope_definition":"deployment incidents only","active":false}`); code != http.StatusNoContent {
		t.Fatalf("edit-fields update: status=%d body=%q, want 204", code, body)
	}
	var gotMP int
	var scopeDef string
	var active bool
	if err := pool.QueryRow(ctx,
		`SELECT min_participants, COALESCE(scope_definition,''), active FROM graph.topic_subscriptions WHERE id=$1`,
		unscopedID).Scan(&gotMP, &scopeDef, &active); err != nil {
		t.Fatalf("read back edit: %v", err)
	}
	if gotMP != 3 || scopeDef != "deployment incidents only" || active {
		t.Errorf("edited fields = (mp=%d, scope=%q, active=%v), want (3, %q, false)",
			gotMP, scopeDef, active, "deployment incidents only")
	}

	// Emit real curl responses for the 400 (unscoped) and 200 (scoped) create
	// cases — the deliverable artifact — against the live httptest server.
	t.Log("\n" + curlDump(t, srv.URL, `{"topic":"curl-unscoped","subscriber_slack_id":"UX","min_participants":1}`))
	t.Log("\n" + curlDump(t, srv.URL, `{"topic":"curl-scoped","subscriber_slack_id":"UX","min_participants":1,"channel_filter":["C12345"]}`))
}

// curlDump runs the curl binary against the server and returns the raw response
// body plus the HTTP status line.
func curlDump(t *testing.T, base, body string) string {
	t.Helper()
	cmd := exec.Command("curl", "-s", "-w", "\nHTTP %{http_code}\n",
		"-X", "POST", "-H", "Content-Type: application/json",
		"-d", body, base+"/api/graph/subscriptions")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Logf("curl error: %v", err)
	}
	return "$ curl -X POST -d '" + body + "' .../api/graph/subscriptions\n" + out.String()
}
