package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/agent-mem/agent-mem/internal/config"
	graphhandlers "github.com/agent-mem/agent-mem/internal/graph/handlers"
	"github.com/agent-mem/agent-mem/internal/graph/jobs"
	"github.com/agent-mem/agent-mem/internal/llmgateway"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// TestNewGatewayClientHonoursCap verifies that a client produced by
// newGatewayClient carries the cap from the snapshot and refuses generate
// calls once the ceiling is reached — with no HTTP request made for the
// refused call.
func TestNewGatewayClientHonoursCap(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusOK)
		// Return a valid generate response.
		w.Write([]byte(`{"backend":"test","text":"ok"}`))
	}))
	t.Cleanup(srv.Close)

	snap := config.ConfigSnapshot{
		LLMGatewayURL:       srv.URL,
		LLMGatewayAPIKey:    "key",
		LLMHourlyCallCap:    1, // cap at 1
		GeminiEmbeddingDims: 768,
	}

	c := newGatewayClient(snap, 768)
	if c == nil {
		t.Fatal("newGatewayClient returned nil with a non-empty URL")
	}

	ctx := context.Background()

	// First call: should succeed and consume the one slot.
	_, err := c.Generate(ctx, "sys", "user")
	if err != nil {
		t.Fatalf("first Generate (within cap) failed: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected 1 HTTP call after first Generate, got %d", calls)
	}

	// Second call: cap is exhausted, must refuse without an HTTP request.
	_, err = c.Generate(ctx, "sys", "user")
	if err == nil {
		t.Fatal("second Generate (over cap) should have been refused, got nil error")
	}
	if !strings.Contains(err.Error(), "hourly cap") {
		t.Errorf("refusal error does not mention 'hourly cap': %v", err)
	}
	if !llmgateway.IsRetryable(err) {
		t.Errorf("cap refusal must be retryable (transient), got: %v", err)
	}
	if calls != 1 {
		t.Errorf("HTTP call count = %d after cap refusal, want 1 (no new request)", calls)
	}
}

func TestGetSettingsIncludesCapAndProcessingPaused(t *testing.T) {
	s := &Server{config: &config.Config{
		LLMHourlyCallCap: 73,
		ProcessingPaused: true,
	}}
	req := httptest.NewRequest(http.MethodGet, "/api/settings", nil)
	rec := httptest.NewRecorder()

	s.handleGetSettings(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/settings status = %d, want 200", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode settings response: %v", err)
	}
	if got, ok := body["llm_hourly_call_cap"]; !ok || got != float64(73) {
		t.Fatalf("llm_hourly_call_cap = %#v (present=%v), want 73", got, ok)
	}
	if got, ok := body["processing_paused"]; !ok || got != true {
		t.Fatalf("processing_paused = %#v (present=%v), want true", got, ok)
	}
}

func TestSlackUserSettingsSaveMaskPreserveAndClear(t *testing.T) {
	db := openProcessorTestDB(t)
	cfg := &config.Config{}
	s := &Server{config: cfg, db: db}
	var logs bytes.Buffer
	oldLogger := log.Logger
	log.Logger = zerolog.New(&logs)
	t.Cleanup(func() { log.Logger = oldLogger })
	put := func(body string, status int) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		s.handleUpdateSettings(rec, httptest.NewRequest(http.MethodPut, "/api/settings", strings.NewReader(body)))
		if rec.Code != status {
			t.Fatalf("PUT status = %d, want %d", rec.Code, status)
		}
		for _, secret := range []string{"xoxc-FAKE-LEAK-CANARY", "xoxd-FAKE-LEAK-CANARY"} {
			if strings.Contains(rec.Body.String(), secret) || strings.Contains(logs.String(), secret) {
				t.Fatal("settings response or log leaked credential")
			}
		}
		return rec
	}
	put(`{"slack_user_token":"xoxc-FAKE-LEAK-CANARY","slack_user_cookie":"xoxd-FAKE-LEAK-CANARY"}`, http.StatusOK)
	assertSaved := func(token, cookie string) {
		t.Helper()
		persisted, err := db.GetAllSettings(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		reloaded := &config.Config{}
		reloaded.ApplyDBSettings(persisted)
		got := reloaded.RuntimeSettings()
		if got["slack_user_token"] != token || got["slack_user_cookie"] != cookie {
			t.Fatal("credentials did not survive persisted-map reload")
		}
		for _, key := range []string{"slack_user_token", "slack_user_cookie"} {
			if _, ok := persisted[key]; !ok {
				t.Fatalf("persisted settings omitted %s", key)
			}
		}
	}
	assertSaved("xoxc-FAKE-LEAK-CANARY", "xoxd-FAKE-LEAK-CANARY")
	rec := httptest.NewRecorder()
	s.handleGetSettings(rec, httptest.NewRequest(http.MethodGet, "/api/settings", nil))
	var response map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	for key, secret := range map[string]string{"slack_user_token": "xoxc-FAKE-LEAK-CANARY", "slack_user_cookie": "xoxd-FAKE-LEAK-CANARY"} {
		if response[key] != maskKey(secret) || strings.Contains(rec.Body.String(), secret) {
			t.Fatalf("%s GET mask incorrect", key)
		}
	}
	put(`{"slack_user_token":"","slack_user_cookie":"  "}`, http.StatusOK)
	put(`{}`, http.StatusOK)
	assertSaved("xoxc-FAKE-LEAK-CANARY", "xoxd-FAKE-LEAK-CANARY")
	for _, body := range []string{
		`{"clear_settings":["llm_gateway_api_key"],"slack_user_token":"replacement"}`,
		`{"clear_settings":["slack_user_cookie","unknown"]}`,
		`{"clear_settings":"slack_user_cookie"}`,
		`{"clear_settings":[42]}`,
		`{"clear_settings":null}`,
	} {
		put(body, http.StatusBadRequest)
		assertSaved("xoxc-FAKE-LEAK-CANARY", "xoxd-FAKE-LEAK-CANARY")
	}
	put(`{"slack_user_cookie":"replacement","clear_settings":["slack_user_cookie"]}`, http.StatusOK)
	assertSaved("xoxc-FAKE-LEAK-CANARY", "")
	put(`{"slack_user_token":"replacement","clear_settings":["slack_user_token"]}`, http.StatusOK)
	assertSaved("", "")
}

type settingsSlackTransport struct {
	base   http.RoundTripper
	target *url.URL
}

func (s settingsSlackTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	cloned := req.Clone(req.Context())
	u := *req.URL
	u.Scheme, u.Host = s.target.Scheme, s.target.Host
	cloned.URL = &u
	return s.base.RoundTrip(cloned)
}

func TestSlackUserSettingsSameDownloadHandlerHotReload(t *testing.T) {
	db := openProcessorTestDB(t)
	cfg := &config.Config{}
	s := &Server{config: cfg, db: db}
	var logs bytes.Buffer
	oldLogger := log.Logger
	log.Logger = zerolog.New(&logs)
	t.Cleanup(func() { log.Logger = oldLogger })
	var headers []http.Header
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers = append(headers, r.Header.Clone())
		if r.Header.Get("Authorization") == "Bearer xoxc-FAKE-LEAK-CANARY" {
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write([]byte("download succeeded"))
			return
		}
		w.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(server.Close)
	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	oldTransport := http.DefaultTransport
	http.DefaultTransport = settingsSlackTransport{base: server.Client().Transport, target: target}
	t.Cleanup(func() { http.DefaultTransport = oldTransport })
	getterCalls := 0
	handler := graphhandlers.NewDescribeAttachmentHandler(graphhandlers.Deps{
		SlackBotToken: "bot-test",
		SlackUserCreds: func() (string, string) {
			getterCalls++
			snap := cfg.Snapshot()
			return snap.SlackUserToken, snap.SlackUserCookie
		},
		Logger: zerolog.New(&logs),
	}).Handler
	payload := json.RawMessage(`{"node_id":"test","source":"slack","external_url":"https://files.slack.com/files-pri/test.bin","mime":"application/x-settings-test"}`)
	download := func(wantCalls int) {
		t.Helper()
		err := handler(context.Background(), payload)
		if !errors.Is(err, jobs.ErrFatal) {
			t.Fatal("unsupported MIME/download refusal must be fatal")
		}
		if len(headers) != wantCalls {
			t.Fatalf("HTTP requests = %d, want %d", len(headers), wantCalls)
		}
		if wantCalls == 3 || wantCalls == 5 {
			if !strings.Contains(err.Error(), "unsupported mime") {
				t.Fatal("fallback did not finish downloading before MIME rejection")
			}
		}
		for _, secret := range []string{"xoxc-FAKE-LEAK-CANARY", "xoxd-FAKE-LEAK-CANARY"} {
			if strings.Contains(err.Error(), secret) || strings.Contains(logs.String(), secret) {
				t.Fatal("download error or log leaked credential")
			}
		}
	}
	save := func(body string) {
		t.Helper()
		rec := httptest.NewRecorder()
		s.handleUpdateSettings(rec, httptest.NewRequest(http.MethodPut, "/api/settings", strings.NewReader(body)))
		if rec.Code != http.StatusOK {
			t.Fatalf("save status = %d", rec.Code)
		}
	}
	download(1)
	save(`{"slack_user_token":"xoxc-FAKE-LEAK-CANARY","slack_user_cookie":"xoxd-FAKE-LEAK-CANARY"}`)
	download(3)
	if headers[2].Get("Cookie") != "d=xoxd-FAKE-LEAK-CANARY" {
		t.Fatal("saved cookie did not reach same handler")
	}
	save(`{"clear_settings":["slack_user_cookie"]}`)
	download(5)
	if headers[4].Get("Authorization") != "Bearer xoxc-FAKE-LEAK-CANARY" || headers[4].Get("Cookie") != "" {
		t.Fatal("cleared cookie must leave Bearer-only fallback")
	}
	save(`{"clear_settings":["slack_user_token"]}`)
	download(6)
	if getterCalls != 4 {
		t.Fatalf("getter calls = %d, want one per download", getterCalls)
	}
}
