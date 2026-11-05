package llmgateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeLimiterClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeLimiterClock(now time.Time) *fakeLimiterClock {
	return &fakeLimiterClock{now: now}
}

func (c *fakeLimiterClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeLimiterClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func resetSharedLimiter(t *testing.T, clock *fakeLimiterClock) {
	t.Helper()
	oldNow := limiterNow
	oldWait := limiterPermitWait
	oldPermits := limiterPermits
	oldAcquire := acquirePermit

	clearSharedLimiterForTest()
	limiterNow = clock.Now
	limiterPermitWait = PermitWait
	limiterPermits = make(chan struct{}, 2)
	acquirePermit = acquireGeneratePermit

	t.Cleanup(func() {
		clearSharedLimiterForTest()
		limiterNow = oldNow
		limiterPermitWait = oldWait
		limiterPermits = oldPermits
		acquirePermit = oldAcquire
	})
}

func clearSharedLimiterForTest() {
	sharedLimiter.mu.Lock()
	sharedLimiter.cap = 0
	sharedLimiter.tokens = 0
	sharedLimiter.lastRefill = time.Time{}
	sharedLimiter.countHour = time.Time{}
	sharedLimiter.count = 0
	sharedLimiter.loggedCap = false
	sharedLimiter.mu.Unlock()
}

func limiterSnapshot() (cap int, capacity, tokens float64, last time.Time, count int) {
	sharedLimiter.mu.Lock()
	defer sharedLimiter.mu.Unlock()
	return sharedLimiter.cap, sharedLimiter.capacityLocked(), sharedLimiter.tokens,
		sharedLimiter.lastRefill, sharedLimiter.count
}

func TestGenerateLimiterCapacityUsesTwelfthWithFloor(t *testing.T) {
	for _, tc := range []struct {
		cap          int
		wantCapacity float64
	}{
		{cap: 1, wantCapacity: 1},
		{cap: 11, wantCapacity: 1},
		{cap: 12, wantCapacity: 1},
		{cap: 120, wantCapacity: 10},
	} {
		t.Run(fmt.Sprintf("cap_%d", tc.cap), func(t *testing.T) {
			clock := newFakeLimiterClock(time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC))
			resetSharedLimiter(t, clock)
			c := New("http://127.0.0.1:1", "key", 1)
			c.SetCap(tc.cap)

			gotCap, gotCapacity, tokens, _, _ := limiterSnapshot()
			if gotCap != tc.cap || gotCapacity != tc.wantCapacity || tokens != tc.wantCapacity {
				t.Fatalf("SetCap(%d) state = cap %d capacity %v tokens %v, want %d/%v/%v",
					tc.cap, gotCap, gotCapacity, tokens, tc.cap, tc.wantCapacity, tc.wantCapacity)
			}
		})
	}
}

func TestGenerateLimiterContinuouslyRefills(t *testing.T) {
	clock := newFakeLimiterClock(time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC))
	resetSharedLimiter(t, clock)
	srv, _ := serve(t, http.StatusOK, `{"backend":"test","text":"ok"}`)
	c := New(srv.URL, "secret", 1)
	c.SetCap(12)

	if _, err := c.Generate(context.Background(), "s", "u"); err != nil {
		t.Fatalf("initial token: %v", err)
	}
	if !c.Capped() {
		t.Fatal("bucket must be empty after its only token is consumed")
	}
	clock.Advance(299 * time.Second)
	if !c.Capped() {
		t.Fatal("12/hour bucket refilled a token before 300 seconds")
	}
	clock.Advance(time.Second)
	if c.Capped() {
		t.Fatal("12/hour bucket did not refill one token after 300 seconds")
	}
	if _, err := c.Generate(context.Background(), "s", "u"); err != nil {
		t.Fatalf("refilled token: %v", err)
	}
}

func TestSetCapTransitionsPreserveSmoothState(t *testing.T) {
	clock := newFakeLimiterClock(time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC))
	resetSharedLimiter(t, clock)
	srv, _ := serve(t, http.StatusOK, `{"backend":"test","text":"ok"}`)
	c := New(srv.URL, "secret", 1)
	c.SetCap(120)

	for i := range 10 {
		if _, err := c.Generate(context.Background(), "s", "u"); err != nil {
			t.Fatalf("drain call %d: %v", i, err)
		}
	}
	clock.Advance(30 * time.Second)
	c.SetCap(240)
	cap, capacity, tokens, _, _ := limiterSnapshot()
	if cap != 240 || capacity != 20 || tokens != 1 {
		t.Fatalf("positive-to-positive state = cap %d capacity %v tokens %v, want 240/20/1", cap, capacity, tokens)
	}
	if _, err := c.Generate(context.Background(), "s", "u"); err != nil {
		t.Fatalf("settled old-rate token: %v", err)
	}

	clock.Advance(30 * time.Second)
	c.SetCap(12)
	_, capacity, tokens, last, _ := limiterSnapshot()
	if capacity != 1 || tokens != 1 {
		t.Fatalf("lower cap did not clamp bucket: capacity %v tokens %v", capacity, tokens)
	}
	clock.Advance(10 * time.Second)
	c.SetCap(12)
	_, _, sameTokens, sameLast, _ := limiterSnapshot()
	if sameTokens != tokens || !sameLast.Equal(last) {
		t.Fatalf("same cap changed limiter state: tokens %v -> %v, last %v -> %v", tokens, sameTokens, last, sameLast)
	}

	c.SetCap(0)
	if c.Capped() {
		t.Fatal("positive-to-zero must make generation unlimited")
	}
	if _, _, gotCap := c.CallCount(); gotCap != 0 {
		t.Fatalf("CallCount cap = %d after disabling, want 0", gotCap)
	}
	clock.Advance(time.Hour)
	c.SetCap(12)
	_, _, tokens, _, _ = limiterSnapshot()
	if tokens != 1 {
		t.Fatalf("zero-to-positive tokens = %v, want a full bucket", tokens)
	}
}

func TestGenerateLimiterSharedAcrossClientsAndSameCapReload(t *testing.T) {
	clock := newFakeLimiterClock(time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC))
	resetSharedLimiter(t, clock)
	srv, _ := serve(t, http.StatusOK, `{"backend":"test","text":"ok"}`)
	first := New(srv.URL, "secret", 1)
	first.SetCap(12)
	if _, err := first.Generate(context.Background(), "s", "u"); err != nil {
		t.Fatalf("first client: %v", err)
	}

	reloaded := New(srv.URL, "secret", 1)
	reloaded.SetCap(12)
	if !reloaded.Capped() {
		t.Fatal("same-cap client reload reset the shared bucket")
	}
	if _, err := reloaded.Generate(context.Background(), "s", "u"); !errors.Is(err, ErrCapped) {
		t.Fatalf("reloaded client error = %v, want ErrCapped", err)
	}
	clock.Advance(5 * time.Minute)
	if _, err := reloaded.Generate(context.Background(), "s", "u"); err != nil {
		t.Fatalf("reloaded client did not share refill: %v", err)
	}
	firstCount, _, _ := first.CallCount()
	reloadedCount, _, _ := reloaded.CallCount()
	if firstCount != 2 || reloadedCount != 2 {
		t.Fatalf("shared display counts = first %d reloaded %d, want 2/2", firstCount, reloadedCount)
	}
}

func TestCappedIsNonConsumingTokenOnlyAndDisplayCountDoesNotGate(t *testing.T) {
	clock := newFakeLimiterClock(time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC))
	resetSharedLimiter(t, clock)
	srv, _ := serve(t, http.StatusOK, `{"backend":"test","text":"ok"}`)
	c := New(srv.URL, "secret", 1)
	c.SetCap(12)

	limiterPermits <- struct{}{}
	limiterPermits <- struct{}{}
	for i := range 3 {
		if c.Capped() {
			t.Fatalf("Capped call %d consumed a token or treated busy permits as a cap", i)
		}
	}
	limiterPermits = make(chan struct{}, 2)
	sharedLimiter.mu.Lock()
	sharedLimiter.count = 10_000
	sharedLimiter.countHour = clock.Now().Truncate(time.Hour)
	sharedLimiter.mu.Unlock()

	if _, err := c.Generate(context.Background(), "s", "u"); err != nil {
		t.Fatalf("display count gated an available token: %v", err)
	}
	if !c.Capped() {
		t.Fatal("successful generate did not debit the token")
	}
}

func TestDisplayCountUsesSharedClockHour(t *testing.T) {
	clock := newFakeLimiterClock(time.Date(2026, 11, 5, 10, 59, 50, 0, time.UTC))
	resetSharedLimiter(t, clock)
	srv, _ := serve(t, http.StatusOK, `{"backend":"test","text":"ok"}`)
	first := New(srv.URL, "secret", 1)
	second := New(srv.URL, "secret", 1)
	first.SetCap(0)

	if _, err := first.Generate(context.Background(), "s", "u"); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := second.CallCount(); got != 1 {
		t.Fatalf("second client display count = %d, want shared count 1", got)
	}
	clock.Advance(20 * time.Second)
	if got, _, _ := first.CallCount(); got != 0 {
		t.Fatalf("display count after clock-hour boundary = %d, want 0", got)
	}
}

func TestGenerateAdmissionLimitsAggregateConcurrencyToTwo(t *testing.T) {
	clock := newFakeLimiterClock(time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC))
	resetSharedLimiter(t, clock)
	limiterPermitWait = time.Second

	const calls = 6
	entered := make(chan struct{}, calls)
	release := make(chan struct{})
	var active int32
	var maximum int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		current := atomic.AddInt32(&active, 1)
		for {
			old := atomic.LoadInt32(&maximum)
			if current <= old || atomic.CompareAndSwapInt32(&maximum, old, current) {
				break
			}
		}
		entered <- struct{}{}
		<-release
		atomic.AddInt32(&active, -1)
		_, _ = w.Write([]byte(`{"backend":"test","text":"ok"}`))
	}))
	t.Cleanup(srv.Close)

	errCh := make(chan error, calls)
	for range calls {
		go func() {
			_, err := New(srv.URL, "key", 1).Generate(context.Background(), "s", "u")
			errCh <- err
		}()
	}
	for range 2 {
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatal("two generate calls did not enter the gateway")
		}
	}
	select {
	case <-entered:
		t.Fatal("a third generate call entered while both shared permits were occupied")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	for i := range calls {
		if err := <-errCh; err != nil {
			t.Fatalf("generate %d: %v", i, err)
		}
	}
	if got := atomic.LoadInt32(&maximum); got != 2 {
		t.Fatalf("maximum aggregate concurrency = %d, want 2", got)
	}
}

func TestPermitWaitErrorClassification(t *testing.T) {
	t.Run("cancellation is exact", func(t *testing.T) {
		clock := newFakeLimiterClock(time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC))
		resetSharedLimiter(t, clock)
		limiterPermits <- struct{}{}
		limiterPermits <- struct{}{}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := New("http://127.0.0.1:1", "key", 1).Generate(ctx, "s", "u")
		if err != context.Canceled {
			t.Fatalf("canceled permit wait error = %#v, want exactly context.Canceled", err)
		}
	})

	t.Run("deadline wraps cap unreachable and context", func(t *testing.T) {
		clock := newFakeLimiterClock(time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC))
		resetSharedLimiter(t, clock)
		limiterPermitWait = time.Second
		limiterPermits <- struct{}{}
		limiterPermits <- struct{}{}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()
		_, err := New("http://127.0.0.1:1", "key", 1).Generate(ctx, "s", "u")
		if !errors.Is(err, ErrUnreachable) || !errors.Is(err, ErrCapped) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("deadline error = %v, want ErrUnreachable + ErrCapped + DeadlineExceeded", err)
		}
	})

	t.Run("permit timeout wraps cap and unreachable", func(t *testing.T) {
		clock := newFakeLimiterClock(time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC))
		resetSharedLimiter(t, clock)
		limiterPermitWait = 0
		limiterPermits <- struct{}{}
		limiterPermits <- struct{}{}
		_, err := New("http://127.0.0.1:1", "key", 1).Generate(context.Background(), "s", "u")
		if !errors.Is(err, ErrUnreachable) || !errors.Is(err, ErrCapped) {
			t.Fatalf("permit timeout error = %v, want ErrUnreachable + ErrCapped", err)
		}
		if errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("permit timeout unexpectedly wraps context deadline: %v", err)
		}
	})
}

func TestPermitReleasedWhenTokenRecheckRefuses(t *testing.T) {
	clock := newFakeLimiterClock(time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC))
	resetSharedLimiter(t, clock)
	c := New("http://127.0.0.1:1", "key", 1)
	c.SetCap(12)
	var released int
	acquirePermit = func(context.Context) (func(), error) {
		sharedLimiter.mu.Lock()
		sharedLimiter.tokens = 0
		sharedLimiter.mu.Unlock()
		return func() { released++ }, nil
	}

	_, err := c.Generate(context.Background(), "s", "u")
	if !errors.Is(err, ErrCapped) {
		t.Fatalf("token recheck error = %v, want ErrCapped", err)
	}
	if released != 1 {
		t.Fatalf("permit releases = %d, want 1", released)
	}
}

func TestPermitReleasedAfterEveryDispatchedResult(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
		body string
	}{
		{name: "success", code: http.StatusOK, body: `{"backend":"test","text":"ok"}`},
		{name: "status error", code: http.StatusServiceUnavailable, body: `{"detail":"busy"}`},
		{name: "empty response", code: http.StatusOK, body: `{"backend":"test","text":""}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock := newFakeLimiterClock(time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC))
			resetSharedLimiter(t, clock)
			srv, _ := serve(t, tc.code, tc.body)
			var released int
			acquirePermit = func(context.Context) (func(), error) {
				return func() { released++ }, nil
			}

			_, _ = New(srv.URL, "secret", 1).Generate(context.Background(), "s", "u")
			if released != 1 {
				t.Fatalf("permit releases = %d, want 1", released)
			}
		})
	}
}

func TestDispatchedFailureDoesNotRefundToken(t *testing.T) {
	clock := newFakeLimiterClock(time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC))
	resetSharedLimiter(t, clock)
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		http.Error(w, "busy", http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	c := New(srv.URL, "key", 1)
	c.SetCap(12)

	if _, err := c.Generate(context.Background(), "s", "u"); err == nil {
		t.Fatal("dispatched 503 returned nil error")
	}
	if !c.Capped() {
		t.Fatal("failed dispatched call refunded its token")
	}
	if _, err := c.Generate(context.Background(), "s", "u"); !errors.Is(err, ErrCapped) {
		t.Fatalf("second call error = %v, want ErrCapped", err)
	}
	if calls != 1 {
		t.Fatalf("HTTP calls = %d, want 1", calls)
	}
	if count, _, _ := c.CallCount(); count != 1 {
		t.Fatalf("display count = %d, want dispatched count 1", count)
	}
}

func TestGenerateCapAndPermitsDoNotAffectEmbedOrDescribe(t *testing.T) {
	clock := newFakeLimiterClock(time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC))
	resetSharedLimiter(t, clock)
	var generateCalls, embedCalls, describeCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/generate":
			generateCalls++
			_, _ = w.Write([]byte(`{"backend":"test","text":"ok"}`))
		case "/embed":
			embedCalls++
			_, _ = w.Write([]byte(`{"embeddings":[[1]],"model":"test","dims":1}`))
		case "/describe":
			describeCalls++
			_, _ = w.Write([]byte(`{"backend":"test","text":"{\"description\":\"d\",\"ocr\":\"o\",\"entities\":[]}"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	c := New(srv.URL, "key", 1)
	c.SetCap(12)
	if _, err := c.Generate(context.Background(), "s", "u"); err != nil {
		t.Fatal(err)
	}
	limiterPermits <- struct{}{}
	limiterPermits <- struct{}{}
	if _, err := c.Embed(context.Background(), "text"); err != nil {
		t.Fatalf("Embed while generate capped and permits busy: %v", err)
	}
	if _, _, _, err := c.Describe(context.Background(), "image/png", []byte{1}, "prompt"); err != nil {
		t.Fatalf("Describe while generate capped and permits busy: %v", err)
	}
	if generateCalls != 1 || embedCalls != 1 || describeCalls != 1 {
		t.Fatalf("HTTP calls generate/embed/describe = %d/%d/%d, want 1/1/1", generateCalls, embedCalls, describeCalls)
	}
	if gen, embed, cap := c.CallCount(); gen != 1 || embed != 1 || cap != 12 {
		t.Fatalf("CallCount = (%d,%d,%d), want (1,1,12)", gen, embed, cap)
	}
}

func TestSanitizeGenerateCaller(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
	}{
		{in: "", want: "agent-mem/unknown"},
		{in: "---", want: "agent-mem/unknown"},
		{in: "Feature Worker@@Run", want: "agent-mem/feature-worker-run"},
		{in: " A/B\\C ", want: "agent-mem/a-b-c"},
		{in: "...", want: "agent-mem/unknown"},
		{in: "handlers.confirmTopicLink", want: "agent-mem/handlers-confirmtopiclink"},
		{in: "worker.(*Server).processObservation", want: "agent-mem/worker-server-processobservation"},
		{in: "Name___with...dots", want: "agent-mem/name___with-dots"},
	} {
		if got := sanitizeCaller(tc.in); got != tc.want {
			t.Errorf("sanitizeCaller(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	got := sanitizeCaller(strings.Repeat("a", 80) + "---")
	if len(got) != 64 || strings.HasSuffix(got, "-") {
		t.Fatalf("long sanitized caller = %q (len %d), want len 64 without trailing hyphen", got, len(got))
	}
}

func TestGenerateSendsSanitizedCallerHeader(t *testing.T) {
	clock := newFakeLimiterClock(time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC))
	resetSharedLimiter(t, clock)
	var caller string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		caller = r.Header.Get("X-LLM-Caller")
		_, _ = w.Write([]byte(`{"backend":"test","text":"ok"}`))
	}))
	t.Cleanup(srv.Close)

	if _, err := New(srv.URL, "key", 1).Generate(context.Background(), "s", "u"); err != nil {
		t.Fatal(err)
	}
	if caller != "agent-mem/unknown" {
		t.Fatalf("X-LLM-Caller = %q, want agent-mem/unknown", caller)
	}
}
