package worker

import "testing"

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
