package llmgateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

type acceptHarness struct {
	t     *testing.T
	clock *fakeLimiterClock
	c     *Client
	hits  *int32
}

func newAcceptHarness(t *testing.T, start time.Time, cap int) *acceptHarness {
	t.Helper()
	clock := newFakeLimiterClock(start)
	resetSharedLimiter(t, clock)
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = w.Write([]byte(`{"backend":"test","text":"ok"}`))
	}))
	t.Cleanup(srv.Close)
	c := New(srv.URL, "k", 1)
	c.SetCap(cap)
	return &acceptHarness{t: t, clock: clock, c: c, hits: &hits}
}

func (h *acceptHarness) ok(label string) {
	h.t.Helper()
	if _, err := h.c.Generate(context.Background(), "s", "u"); err != nil {
		h.t.Fatalf("%s: want success, got %v", label, err)
	}
}

// refused asserts a token refusal that never reached HTTP.
func (h *acceptHarness) refused(label string) {
	h.t.Helper()
	before := atomic.LoadInt32(h.hits)
	_, err := h.c.Generate(context.Background(), "s", "u")
	if !errors.Is(err, ErrCapped) || !errors.Is(err, ErrUnreachable) {
		h.t.Fatalf("%s: err = %v, want ErrCapped+ErrUnreachable", label, err)
	}
	if atomic.LoadInt32(h.hits) != before {
		h.t.Fatalf("%s: refused call reached HTTP", label)
	}
}

func (h *acceptHarness) okN(n int, label string) {
	h.t.Helper()
	for i := range n {
		h.ok(fmt.Sprintf("%s #%d", label, i))
	}
}

var acceptStart = time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC)

func TestAC1BucketCap300(t *testing.T) {
	h := newAcceptHarness(t, acceptStart, 300)
	h.okN(25, "burst")
	if got := atomic.LoadInt32(h.hits); got != 25 {
		t.Fatalf("hits = %d, want 25", got)
	}
	h.refused("26th")
	h.clock.Advance(12 * time.Second)
	h.ok("after 12s")
	h.refused("after 12s next")
}

func TestAC2FractionalCap30(t *testing.T) {
	h := newAcceptHarness(t, acceptStart, 30)
	h.okN(2, "initial")
	h.refused("third")
	h.refused("third again charges nothing")
	h.clock.Advance(59 * time.Second)
	h.refused("59s")
	h.clock.Advance(time.Second)
	h.ok("60s")
	h.refused("60s next")
	h.clock.Advance(60 * time.Second)
	h.refused("120s")
	h.clock.Advance(60 * time.Second)
	h.ok("180s")
}

func TestAC3HourBoundaryDoesNotBurst(t *testing.T) {
	h := newAcceptHarness(t, time.Date(2026, 11, 5, 10, 59, 59, 0, time.UTC), 300)
	h.okN(25, "drain")
	h.refused("drained")
	h.clock.Advance(2 * time.Second) // now 11:00:01
	h.refused("past hh:00")
}

func TestAC5CapTransitions(t *testing.T) {
	t.Run("same value no refill", func(t *testing.T) {
		h := newAcceptHarness(t, acceptStart, 300)
		h.okN(25, "drain")
		h.c.SetCap(300)
		h.refused("after SetCap(300)")
	})
	t.Run("old-rate settlement", func(t *testing.T) {
		h := newAcceptHarness(t, acceptStart, 300)
		h.okN(25, "drain")
		h.clock.Advance(6 * time.Second)
		h.c.SetCap(600)
		h.refused("0.5 token")
		h.clock.Advance(3 * time.Second)
		h.ok("1.0 token")
		h.refused("after")
	})
	t.Run("clamp 600 to 120", func(t *testing.T) {
		h := newAcceptHarness(t, acceptStart, 600)
		h.ok("prime bucket") // seeds full bucket (50 tokens)
		h.c.SetCap(120)
		// 49 tokens clamp to capacity 10
		h.okN(10, "clamped")
		h.refused("11th")
	})
	t.Run("300 to 0 to 300", func(t *testing.T) {
		h := newAcceptHarness(t, acceptStart, 300)
		h.okN(25, "drain")
		h.c.SetCap(0)
		h.okN(100, "unlimited")
		h.c.SetCap(300)
		h.okN(25, "full again")
		h.refused("26th")
	})
}

func TestAC7ConcurrencyAcrossReload(t *testing.T) {
	clock := newFakeLimiterClock(acceptStart)
	resetSharedLimiter(t, clock)
	limiterPermitWait = 5 * time.Second

	entered := make(chan struct{}, 16)
	release := make(chan struct{})
	var active, maximum int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		cur := atomic.AddInt32(&active, 1)
		for {
			old := atomic.LoadInt32(&maximum)
			if cur <= old || atomic.CompareAndSwapInt32(&maximum, old, cur) {
				break
			}
		}
		entered <- struct{}{}
		<-release
		atomic.AddInt32(&active, -1)
		_, _ = w.Write([]byte(`{"backend":"test","text":"ok"}`))
	}))
	t.Cleanup(srv.Close)

	// graph-width and flat-width, as settings_handlers builds them.
	oldGraph, oldFlat := New(srv.URL, "k", 3072), New(srv.URL, "k", 768)
	oldGraph.SetCap(0)
	oldFlat.SetCap(0)
	errs := make(chan error, 8)
	for _, c := range []*Client{oldGraph, oldFlat} {
		go func() { _, err := c.Generate(context.Background(), "s", "u"); errs <- err }()
	}
	for range 2 {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("old clients did not both enter the gateway")
		}
	}

	newGraph, newFlat := New(srv.URL, "k", 3072), New(srv.URL, "k", 768)
	newGraph.SetCap(0)
	newFlat.SetCap(0)
	for _, c := range []*Client{newGraph, newFlat} {
		for range 3 {
			go func() { _, err := c.Generate(context.Background(), "s", "u"); errs <- err }()
		}
	}
	select {
	case <-entered:
		t.Fatal("a replacement call entered while both permits were held")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	for range 8 {
		if err := <-errs; err != nil {
			t.Fatalf("generate: %v", err)
		}
	}
	if got := atomic.LoadInt32(&maximum); got != 2 {
		t.Fatalf("max in-flight = %d, want 2", got)
	}
}
