package trace

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeStore struct {
	mu      sync.Mutex
	records []Record
	delay   time.Duration
	err     error
}

func (s *fakeStore) Write(ctx context.Context, r Record) error {
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return ctx.Err()
	}
	if s.err != nil {
		return s.err
	}
	s.mu.Lock()
	s.records = append(s.records, r)
	s.mu.Unlock()
	return nil
}
func (s *fakeStore) Prune(context.Context, time.Time) (int64, error) { return 1, s.err }

type blockingStore struct {
	started chan struct{}
	release chan struct{}
}

func (s *blockingStore) Write(ctx context.Context, _ Record) error {
	select {
	case s.started <- struct{}{}:
	default:
	}
	select {
	case <-s.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (s *blockingStore) Prune(context.Context, time.Time) (int64, error) { return 0, nil }

func TestRecorderWritesOutsideRequestContext(t *testing.T) {
	store := &fakeStore{}
	r := NewRecorder(store, Options{MaxEntries: 2, MaxBytes: 1024, WriteTimeout: time.Second})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !r.Enqueue(ctx, Record{ID: "ctx_one"}) {
		t.Fatal("record was dropped")
	}
	r.Close(time.Second)
	if got := r.Stats(); got.Attempted != 1 || got.Written != 1 || got.Dropped != 0 || got.Failed != 0 {
		t.Fatalf("stats = %+v", got)
	}
}

func TestRecorderDropsFullAndOversizedEntries(t *testing.T) {
	store := &fakeStore{delay: 200 * time.Millisecond}
	r := NewRecorder(store, Options{MaxEntries: 1, MaxBytes: 8, WriteTimeout: time.Second})
	if r.Enqueue(context.Background(), Record{ID: "ctx_entry_too_large"}) {
		t.Fatal("oversized record accepted")
	}
	if got := r.Stats(); got.Attempted != 1 || got.Dropped != 1 {
		t.Fatalf("stats = %+v", got)
	}
	r.Close(time.Second)
}
func TestRecorderDropsWhenQueueIsFull(t *testing.T) {
	store := &blockingStore{started: make(chan struct{}, 1), release: make(chan struct{})}
	r := NewRecorder(store, Options{MaxEntries: 1, MaxBytes: 4096, WriteTimeout: time.Second})
	if !r.Enqueue(context.Background(), Record{ID: "ctx_writing"}) {
		t.Fatal("first record dropped")
	}
	<-store.started
	if !r.Enqueue(context.Background(), Record{ID: "ctx_queued"}) {
		t.Fatal("queued record dropped")
	}
	if r.Enqueue(context.Background(), Record{ID: "ctx_full"}) {
		t.Fatal("full queue accepted record")
	}
	close(store.release)
	r.Close(time.Second)
	if got := r.Stats(); got.Attempted != 3 || got.Written != 2 || got.Dropped != 1 {
		t.Fatalf("stats = %+v", got)
	}
}

func TestRecorderCountsWriteFailureAndBoundedShutdown(t *testing.T) {
	store := &fakeStore{delay: time.Second, err: errors.New("boom")}
	r := NewRecorder(store, Options{MaxEntries: 2, MaxBytes: 1024, WriteTimeout: 20 * time.Millisecond})
	r.Enqueue(context.Background(), Record{ID: "ctx_fail"})
	start := time.Now()
	r.Close(100 * time.Millisecond)
	if time.Since(start) > 200*time.Millisecond {
		t.Fatal("shutdown exceeded bound")
	}
	if got := r.Stats(); got.Failed != 1 {
		t.Fatalf("stats = %+v", got)
	}
}

func TestPruneUsesThirtyDayCutoff(t *testing.T) {
	store := &fakeStore{}
	now := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	if _, err := Prune(context.Background(), store, now); err != nil {
		t.Fatal(err)
	}
}
