package worker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFetchOpenRouterUsage_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer sk-or-v1-test" {
			t.Errorf("unexpected Authorization header: %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":{"label":"sk-or-v1-c77...6ac","limit":50,"limit_reset":"monthly","limit_remaining":42.74,"usage":7.26,"usage_daily":2.72,"usage_weekly":7.26,"usage_monthly":7.26,"is_free_tier":false}}`))
	}))
	defer srv.Close()

	resp := fetchOpenRouterUsage(context.Background(), srv.Client(), srv.URL, "sk-or-v1-test")

	if !resp.Available {
		t.Fatalf("expected Available=true, got error: %s", resp.Error)
	}
	if resp.Label != "sk-or-v1-c77...6ac" {
		t.Errorf("unexpected label: %q", resp.Label)
	}
	if resp.Limit == nil || *resp.Limit != 50 {
		t.Errorf("unexpected limit: %v", resp.Limit)
	}
	if resp.LimitRemaining == nil || *resp.LimitRemaining != 42.74 {
		t.Errorf("unexpected limit_remaining: %v", resp.LimitRemaining)
	}
	if resp.Usage == nil || *resp.Usage != 7.26 {
		t.Errorf("unexpected usage: %v", resp.Usage)
	}
	if resp.UsageDaily == nil || *resp.UsageDaily != 2.72 {
		t.Errorf("unexpected usage_daily: %v", resp.UsageDaily)
	}
	if resp.UsageMonthly == nil || *resp.UsageMonthly != 7.26 {
		t.Errorf("unexpected usage_monthly: %v", resp.UsageMonthly)
	}
	if resp.LimitReset != "monthly" {
		t.Errorf("unexpected limit_reset: %q", resp.LimitReset)
	}
	if resp.IsFreeTier == nil || *resp.IsFreeTier != false {
		t.Errorf("unexpected is_free_tier: %v", resp.IsFreeTier)
	}
}

func TestFetchOpenRouterUsage_NullLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":{"label":"free-key","limit":null,"limit_reset":null,"limit_remaining":null,"usage":1.5,"usage_daily":0.1,"usage_weekly":1.5,"usage_monthly":1.5,"is_free_tier":true}}`))
	}))
	defer srv.Close()

	resp := fetchOpenRouterUsage(context.Background(), srv.Client(), srv.URL, "sk-or-v1-test")

	if !resp.Available {
		t.Fatalf("expected Available=true, got error: %s", resp.Error)
	}
	if resp.Limit != nil {
		t.Errorf("expected nil limit, got %v", *resp.Limit)
	}
}

func TestFetchOpenRouterUsage_NonSkOrKey(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.Write([]byte(`{"data":{}}`))
	}))
	defer srv.Close()

	for _, key := range []string{"", "some-random-key", "sk-live-not-openrouter"} {
		resp := fetchOpenRouterUsage(context.Background(), srv.Client(), srv.URL, key)
		if resp.Available {
			t.Errorf("expected Available=false for key %q", key)
		}
		if resp.Error != "OpenRouter key not configured" {
			t.Errorf("unexpected error message for key %q: %q", key, resp.Error)
		}
	}

	if called {
		t.Errorf("expected upstream to never be called for a non-sk-or key")
	}
}

func TestFetchOpenRouterUsage_UpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"message":"invalid api key"}}`))
	}))
	defer srv.Close()

	resp := fetchOpenRouterUsage(context.Background(), srv.Client(), srv.URL, "sk-or-v1-bad")

	if resp.Available {
		t.Fatalf("expected Available=false on upstream error")
	}
	if !strings.Contains(resp.Error, "401") {
		t.Errorf("expected error to mention status code, got: %q", resp.Error)
	}
}

func TestFetchOpenRouterUsage_MalformedJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`not json`))
	}))
	defer srv.Close()

	resp := fetchOpenRouterUsage(context.Background(), srv.Client(), srv.URL, "sk-or-v1-test")

	if resp.Available {
		t.Fatalf("expected Available=false on malformed JSON")
	}
	if resp.Error == "" {
		t.Errorf("expected a non-empty error message")
	}
}

func TestOpenRouterUsageCache(t *testing.T) {
	var cache openRouterUsageCache

	if _, ok := cache.get(); ok {
		t.Fatalf("expected empty cache miss")
	}

	limit := 50.0
	cache.set(openRouterUsageResponse{Available: true, Label: "cached", Limit: &limit})

	got, ok := cache.get()
	if !ok {
		t.Fatalf("expected cache hit after set")
	}
	if got.Label != "cached" {
		t.Errorf("unexpected cached label: %q", got.Label)
	}

	// Setting an unavailable result must not evict a good cached entry.
	cache.set(openRouterUsageResponse{Available: false, Error: "boom"})
	got, ok = cache.get()
	if !ok || got.Label != "cached" {
		t.Errorf("expected prior cached entry to survive a failed set, got ok=%v label=%q", ok, got.Label)
	}
}
