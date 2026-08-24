package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agent-mem/agent-mem/internal/graph/fetchers"
	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog"
)

func TestScopeRefreshErrorCoversGenScopeFailures(t *testing.T) {
	tests := []struct {
		name   string
		titles []string
		gemini *mockGemini
	}{
		{
			name:   "no readable material",
			gemini: &mockGemini{},
		},
		{
			name:   "LLM error",
			titles: []string{"Payment PRD"},
			gemini: &mockGemini{generateResult: func() (string, error) {
				return "", errors.New("LLM unavailable")
			}},
		},
		{
			name:   "invalid LLM JSON",
			titles: []string{"Payment PRD"},
			gemini: &mockGemini{generateResult: func() (string, error) {
				return "not JSON", nil
			}},
		},
		{
			name:   "empty LLM output",
			titles: []string{"Payment PRD"},
			gemini: &mockGemini{generateResult: func() (string, error) {
				return "", nil
			}},
		},
		{
			name:   "empty scope definition",
			titles: []string{"Payment PRD"},
			gemini: &mockGemini{generateResult: func() (string, error) {
				return `{"scope_definition":"","summary":"missing scope"}`, nil
			}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scopeDef, _ := genScope(context.Background(), Deps{Gemini: tt.gemini}, "payments", tt.titles, nil)
			if scopeDef != "" {
				t.Fatalf("genScope() definition = %q, want empty", scopeDef)
			}
			if got := scopeRefreshError(scopeDef, nil, 5); got != "no readable content in 5 source(s)" {
				t.Errorf("scopeRefreshError() = %q, want readable fallback", got)
			}
		})
	}
}

func TestSourceFailureLineSanitizesHTTPBodies(t *testing.T) {
	tests := []struct {
		name string
		src  topicSource
		err  error
		want string
	}{
		{
			name: "Confluence HTML response",
			src:  topicSource{Type: "confluence", URL: "https://x/wiki/pages/123"},
			err:  errors.New("confluence descendants status 404: <html>dead link</html>"),
			want: "confluence https://x/wiki/pages/123: status 404",
		},
		{
			name: "GitHub HTML response",
			src:  topicSource{Type: "github", URL: "wego/payments"},
			err:  errors.New("repo meta: github status 401: <html>bad credentials</html>"),
			want: "github wego/payments: status 401",
		},
		{
			name: "multiline non-HTTP error",
			src:  topicSource{Type: "github", URL: "wego/payments"},
			err:  errors.New("connection reset\nsecret response body"),
			want: "github wego/payments: connection reset",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sourceFailureLine(tt.src, tt.err)
			if got != tt.want {
				t.Errorf("sourceFailureLine() = %q, want %q", got, tt.want)
			}
			if strings.Contains(got, "<html>") || strings.Contains(got, "\n") {
				t.Errorf("sourceFailureLine() leaked response body: %q", got)
			}
		})
	}
}

func TestScopeRefreshErrorIsBounded(t *testing.T) {
	failures := make([]string, 0, 20)
	for range 20 {
		failures = append(failures, strings.Repeat("x", 100))
	}
	got := scopeRefreshError("", failures, len(failures))
	if len(got) > maxScopeErrorLength {
		t.Fatalf("scopeRefreshError() length = %d, want <= %d", len(got), maxScopeErrorLength)
	}
}

func TestRefreshTopicScopeFailurePreservesScope(t *testing.T) {
	pool := openTestDB(t)
	ctx := context.Background()

	const (
		oldDefinition = "payment partner incidents and money movement"
		oldSummary    = "A previously distilled scope summary."
		rootID        = "9223372036854770000"
	)
	oldRefreshedAt := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	var id int64
	observedStatus := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/repos/") {
			http.Error(w, "<html>bad credentials</html>", http.StatusUnauthorized)
			return
		}
		var status string
		if err := pool.QueryRow(r.Context(),
			`SELECT scope_status FROM graph.topic_subscriptions WHERE id=$1`, id).Scan(&status); err != nil {
			observedStatus <- "query error: " + err.Error()
		} else {
			observedStatus <- status
		}
		http.Error(w, "<html>dead link</html>", http.StatusNotFound)
	}))
	defer server.Close()

	sources, err := json.Marshal([]topicSource{
		{Type: "confluence", URL: server.URL + "/wiki/spaces/PA/pages/" + rootID + "/Garbage"},
		{Type: "github", URL: "https://github.com/wego/payments"},
	})
	if err != nil {
		t.Fatalf("marshal sources: %v", err)
	}

	err = pool.QueryRow(ctx, `
		INSERT INTO graph.topic_subscriptions
		  (subscriber_slack_id, topic, sources, scope_definition, scope_summary,
		   scope_status, scope_refreshed_at, scope_error)
		VALUES ('UTEST', 'scope-refresh-preservation', $1, $2, $3, 'ready', $4, '')
		RETURNING id`, sources, oldDefinition, oldSummary, oldRefreshedAt).Scan(&id)
	if err != nil {
		t.Fatalf("insert subscription: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM graph.topic_subscriptions WHERE id=$1`, id)
		_, _ = pool.Exec(context.Background(), `DELETE FROM graph.jobs WHERE machine_id='scope-refresh-test'`)
		_, _ = pool.Exec(context.Background(), `DELETE FROM graph.nodes WHERE natural_key=$1`, rootID)
	})

	payload, err := json.Marshal(refreshScopePayload{SubscriptionID: id})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	fetcherRegistry := fetchers.NewRegistry(fetchers.Config{
		CFBaseURL:  server.URL,
		GHBaseURL:  server.URL,
		HTTPClient: server.Client(),
	}, zerolog.Nop())
	handler := NewRefreshTopicScope(Deps{
		DB:        pool,
		Logger:    zerolog.Nop(),
		MachineID: "scope-refresh-test",
		Fetchers:  fetcherRegistry,
		Gemini:    &mockGemini{},
	})
	if err := handler(ctx, payload); err != nil {
		t.Fatalf("refresh handler: %v", err)
	}
	select {
	case status := <-observedStatus:
		if status != "refreshing" {
			t.Errorf("scope_status during source fetch = %q, want refreshing", status)
		}
	case <-time.After(time.Second):
		t.Error("did not observe source fetch")
	}

	var gotDefinition, gotSummary, gotStatus, gotError string
	var gotRefreshedAt time.Time
	if err := pool.QueryRow(ctx, `
		SELECT scope_definition, scope_summary, scope_status, scope_error, scope_refreshed_at
		FROM graph.topic_subscriptions WHERE id=$1`, id).
		Scan(&gotDefinition, &gotSummary, &gotStatus, &gotError, &gotRefreshedAt); err != nil {
		t.Fatalf("read subscription: %v", err)
	}
	if gotDefinition != oldDefinition || gotSummary != oldSummary {
		t.Errorf("failed refresh changed preserved scope: definition=%q summary=%q", gotDefinition, gotSummary)
	}
	if gotStatus != "error" {
		t.Errorf("scope_status = %q, want error", gotStatus)
	}
	wantError := "confluence " + server.URL + "/wiki/spaces/PA/pages/" + rootID + "/Garbage: status 404\n" +
		"github wego/payments: status 401"
	if gotError != wantError {
		t.Errorf("scope_error = %q, want %q", gotError, wantError)
	}
	if !gotRefreshedAt.Equal(oldRefreshedAt) {
		t.Errorf("scope_refreshed_at = %s, want preserved last-ok timestamp %s", gotRefreshedAt, oldRefreshedAt)
	}
}

func TestRefreshTopicScopePartialSuccessRecordsSourceFailures(t *testing.T) {
	pool := openTestDB(t)
	ctx := context.Background()

	const (
		rootID  = "9223372036854760000"
		childID = "9223372036854760001"
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/repos/") {
			http.Error(w, "<html>bad credentials</html>", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"id":"` + childID + `","title":"Payment PRD"}],"_links":{"next":""}}`))
	}))
	defer server.Close()

	sources, err := json.Marshal([]topicSource{
		{Type: "confluence", URL: server.URL + "/wiki/spaces/PA/pages/" + rootID + "/Payment-PRDs"},
		{Type: "github", URL: "https://github.com/wego/payments"},
	})
	if err != nil {
		t.Fatalf("marshal sources: %v", err)
	}
	var id int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO graph.topic_subscriptions (subscriber_slack_id, topic, sources)
		VALUES ('UTEST', 'scope-partial-success', $1) RETURNING id`, sources).Scan(&id); err != nil {
		t.Fatalf("insert subscription: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM graph.topic_subscriptions WHERE id=$1`, id)
		_, _ = pool.Exec(context.Background(), `DELETE FROM graph.jobs WHERE machine_id='scope-partial-test'`)
		_, _ = pool.Exec(context.Background(), `DELETE FROM graph.nodes WHERE natural_key = ANY($1)`, []string{rootID, childID})
	})

	payload, err := json.Marshal(refreshScopePayload{SubscriptionID: id})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	fetcherRegistry := fetchers.NewRegistry(fetchers.Config{
		CFBaseURL:  server.URL,
		GHBaseURL:  server.URL,
		HTTPClient: server.Client(),
	}, zerolog.Nop())
	gemini := &mockGemini{generateResult: func() (string, error) {
		return `{"scope_definition":"payment products and partners","summary":"Payment product documentation."}`, nil
	}}
	handler := NewRefreshTopicScope(Deps{
		DB:        pool,
		Logger:    zerolog.Nop(),
		MachineID: "scope-partial-test",
		Fetchers:  fetcherRegistry,
		Gemini:    gemini,
	})
	if err := handler(ctx, payload); err != nil {
		t.Fatalf("refresh handler: %v", err)
	}

	var definition, summary, status, scopeError string
	if err := pool.QueryRow(ctx, `
		SELECT scope_definition, scope_summary, scope_status, scope_error
		FROM graph.topic_subscriptions WHERE id=$1`, id).
		Scan(&definition, &summary, &status, &scopeError); err != nil {
		t.Fatalf("read subscription: %v", err)
	}
	if definition != "payment products and partners" || summary != "Payment product documentation." {
		t.Errorf("distilled scope = (%q, %q), want successful Confluence scope", definition, summary)
	}
	if status != "ready" {
		t.Errorf("scope_status = %q, want ready", status)
	}
	if scopeError != "github wego/payments: status 401" {
		t.Errorf("scope_error = %q, want GitHub status failure", scopeError)
	}
}

func TestSubscriptionScopeRefreshLifecycle(t *testing.T) {
	pool := openTestDB(t)
	ctx := context.Background()
	refreshedAt := time.Date(2026, 8, 24, 1, 2, 3, 0, time.UTC)

	var nullTimestampID, readyID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO graph.topic_subscriptions (subscriber_slack_id, topic)
		VALUES ('UTEST', 'scope-null-timestamp') RETURNING id`).Scan(&nullTimestampID); err != nil {
		t.Fatalf("insert null-timestamp subscription: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO graph.topic_subscriptions
		  (subscriber_slack_id, topic, scope_status, scope_error, scope_refreshed_at)
		VALUES ('UTEST', 'scope-queued-lifecycle', 'ready', 'github wego/payments: status 401', $1)
		RETURNING id`, refreshedAt).Scan(&readyID); err != nil {
		t.Fatalf("insert ready subscription: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM graph.jobs WHERE machine_id='scope-lifecycle-test'`)
		_, _ = pool.Exec(context.Background(), `DELETE FROM graph.topic_subscriptions WHERE id = ANY($1)`, []int64{nullTimestampID, readyID})
	})

	subscriptions, err := listSubscriptions(ctx, pool, false)
	if err != nil {
		t.Fatalf("list subscriptions: %v", err)
	}
	byID := make(map[int64]subscription, len(subscriptions))
	for _, sub := range subscriptions {
		byID[sub.ID] = sub
	}
	if byID[nullTimestampID].ScopeRefreshedAt != nil {
		t.Errorf("null timestamp = %v, want nil", byID[nullTimestampID].ScopeRefreshedAt)
	}
	ready := byID[readyID]
	if ready.ScopeError != "github wego/payments: status 401" {
		t.Errorf("scope_error = %q, want persisted source error", ready.ScopeError)
	}
	if ready.ScopeRefreshedAt == nil || !ready.ScopeRefreshedAt.Equal(refreshedAt) {
		t.Errorf("scope_refreshed_at = %v, want %s", ready.ScopeRefreshedAt, refreshedAt)
	}

	handler := NewSubscriptions(Deps{DB: pool, Runner: "any", MachineID: "scope-lifecycle-test"})
	router := chi.NewRouter()
	router.Post("/api/graph/subscriptions/{id}/refresh", handler.refresh)
	req := httptest.NewRequest(http.MethodPost, "/api/graph/subscriptions/"+strconv.FormatInt(readyID, 10)+"/refresh", nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, req)
	if response.Code != http.StatusAccepted {
		t.Fatalf("refresh status = %d, body=%q, want %d", response.Code, response.Body.String(), http.StatusAccepted)
	}
	if !strings.Contains(response.Body.String(), `"status":"queued"`) {
		t.Errorf("refresh body = %q, want queued status", response.Body.String())
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT scope_status FROM graph.topic_subscriptions WHERE id=$1`, readyID).Scan(&status); err != nil {
		t.Fatalf("read queued status: %v", err)
	}
	if status != "queued" {
		t.Errorf("scope_status = %q, want queued", status)
	}
}
