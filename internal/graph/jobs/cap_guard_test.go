package jobs_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-mem/agent-mem/internal/graph/fetchers"
	"github.com/agent-mem/agent-mem/internal/graph/handlers"
	"github.com/agent-mem/agent-mem/internal/graph/jobs"
	"github.com/agent-mem/agent-mem/internal/llmgateway"
	"github.com/rs/zerolog"
	"golang.org/x/sync/semaphore"
)

func TestTypeDispatcher_CapSkipsLLMButRunsNonLLM(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool := testDB(t)

	var llmRan atomic.Int32
	reg := jobs.NewRegistry()
	reg.Register("llm_job", jobs.Entry{
		PoolSize: 1,
		Lease:    time.Second,
		UsesLLM:  true,
		Handler: func(context.Context, []byte) error {
			llmRan.Add(1)
			return nil
		},
	})
	reg.Register("plain_job", jobs.Entry{
		PoolSize: 1,
		Lease:    time.Second,
		Handler:  func(context.Context, []byte) error { return nil },
	})

	llmID := mustEnqueue(t, pool, "llm_job", []byte(`{}`), 0)
	plainID := mustEnqueue(t, pool, "plain_job", []byte(`{}`), 0)
	newDispatcher := func(jobType string) *jobs.TypeDispatcher {
		return jobs.NewTypeDispatcher(jobs.DispatcherConfig{
			Type:         jobType,
			Registry:     reg,
			DB:           pool,
			WorkerID:     "w-cap-test",
			Runner:       "vps",
			IdleInterval: 20 * time.Millisecond,
			Semaphores:   map[string]*semaphore.Weighted{},
			Logger:       zerolog.Nop(),
			CapReached:   func() bool { return true },
		})
	}
	go newDispatcher("llm_job").Run(ctx)
	go newDispatcher("plain_job").Run(ctx)

	waitForDone(t, pool, plainID, 3*time.Second)

	var status string
	var attempts int16
	if err := pool.QueryRow(ctx,
		`SELECT status, attempts FROM graph.jobs WHERE id=$1`, llmID,
	).Scan(&status, &attempts); err != nil {
		t.Fatalf("query capped job: %v", err)
	}
	if status != "queued" {
		t.Fatalf("capped LLM job status = %q, want queued", status)
	}
	if llmRan.Load() != 0 {
		t.Fatalf("capped LLM handler ran %d times, want 0", llmRan.Load())
	}
	if attempts != 0 {
		t.Fatalf("capped LLM job attempts = %d, want 0", attempts)
	}
}

func TestTypeDispatcher_CappedErrorRefundsMaxAttempt(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool := testDB(t)

	var calls atomic.Int32
	reg := jobs.NewRegistry()
	reg.Register("llm_job", jobs.Entry{
		PoolSize: 1,
		Lease:    time.Second,
		UsesLLM:  true,
		Handler: func(context.Context, []byte) error {
			calls.Add(1)
			return fmt.Errorf("handler context: %w", llmgateway.ErrCapped)
		},
	})

	id := mustEnqueueWithMaxAttempts(t, pool, "llm_job", []byte(`{}`), 0, 1)
	d := jobs.NewTypeDispatcher(jobs.DispatcherConfig{
		Type:          "llm_job",
		Registry:      reg,
		DB:            pool,
		WorkerID:      "w-refund-test",
		Runner:        "vps",
		IdleInterval:  20 * time.Millisecond,
		BackoffBase:   time.Hour,
		BackoffCap:    time.Hour,
		Semaphores:    map[string]*semaphore.Weighted{},
		Logger:        zerolog.Nop(),
		RefundAttempt: func(err error) bool { return errors.Is(err, llmgateway.ErrCapped) },
	})
	go d.Run(ctx)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var status string
		var attempts int16
		err := pool.QueryRow(ctx,
			`SELECT status, attempts FROM graph.jobs WHERE id=$1`, id,
		).Scan(&status, &attempts)
		if err == nil && calls.Load() == 1 && status == "queued" && attempts == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}

	var status string
	var attempts int16
	if err := pool.QueryRow(ctx,
		`SELECT status, attempts FROM graph.jobs WHERE id=$1`, id,
	).Scan(&status, &attempts); err != nil {
		t.Fatalf("query refunded job: %v", err)
	}
	t.Fatalf("capped max-attempt job: calls=%d status=%q attempts=%d; want calls=1 status=queued attempts=0",
		calls.Load(), status, attempts)
}

func TestFetchBody_NotAuthedFailsOnFirstAttempt(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool := testDB(t)

	var requests atomic.Int32
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"ok":false,"error":"not_authed"}`)),
		}, nil
	})}
	deps := handlers.Deps{
		DB:        pool,
		Logger:    zerolog.Nop(),
		MachineID: "test",
		Fetchers: fetchers.NewRegistry(fetchers.Config{
			SlackBotToken: "",
			HTTPClient:    client,
		}, zerolog.Nop()),
	}
	reg := jobs.NewRegistry()
	reg.Register("fetch_body", handlers.NewFetchBodyHandler(deps))
	id := mustEnqueueWithMaxAttempts(t, pool, "fetch_body",
		[]byte(`{"node_id":"slack:C08S954G2LX:1779710863.216389"}`), 0, 5)

	d := jobs.NewTypeDispatcher(jobs.DispatcherConfig{
		Type:         "fetch_body",
		Registry:     reg,
		DB:           pool,
		WorkerID:     "w-not-authed-test",
		Runner:       "vps",
		IdleInterval: 20 * time.Millisecond,
		BackoffBase:  20 * time.Millisecond,
		BackoffCap:   20 * time.Millisecond,
		Semaphores:   map[string]*semaphore.Weighted{},
		Logger:       zerolog.Nop(),
	})
	go d.Run(ctx)

	waitForStatus(t, pool, id, "failed", 3*time.Second)
	var attempts int16
	if err := pool.QueryRow(ctx,
		`SELECT attempts FROM graph.jobs WHERE id=$1`, id,
	).Scan(&attempts); err != nil {
		t.Fatalf("query failed not_authed job: %v", err)
	}
	if attempts != 1 {
		t.Fatalf("not_authed attempts = %d, want 1", attempts)
	}
	if requests.Load() != 2 {
		t.Fatalf("Slack requests = %d, want 2 (replies then history fallback once)", requests.Load())
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
