package trace

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"
)

type Record struct {
	ID             string    `json:"id"`
	CreatedAt      time.Time `json:"created_at"`
	Route          string    `json:"route"`
	BuildRev       string    `json:"build_rev"`
	Caller         string    `json:"caller"`
	Status         int       `json:"status"`
	Error          *string   `json:"error"`
	AskerInput     any       `json:"asker_input"`
	ACLMode        string    `json:"acl_mode"`
	Request        any       `json:"request"`
	Items          any       `json:"items"`
	ItemsTruncated bool      `json:"items_truncated"`
	ResponseSHA256 string    `json:"response_sha256"`
	LatencyMS      int64     `json:"latency_ms"`
}

type Stats struct {
	Attempted uint64 `json:"attempted"`
	Written   uint64 `json:"written"`
	Dropped   uint64 `json:"dropped"`
	Failed    uint64 `json:"failed"`
}
type Store interface {
	Write(context.Context, Record) error
	Prune(context.Context, time.Time) (int64, error)
}
type Options struct {
	MaxEntries, MaxBytes int
	WriteTimeout         time.Duration
}
type queued struct {
	record Record
	size   int
}

type Recorder struct {
	store       Store
	options     Options
	queue       chan queued
	mu          sync.Mutex
	queuedBytes int
	closed      bool
	wg          sync.WaitGroup
	attempted   atomic.Uint64
	written     atomic.Uint64
	dropped     atomic.Uint64
	failed      atomic.Uint64
}

func NewRecorder(store Store, options Options) *Recorder {
	if options.MaxEntries <= 0 {
		options.MaxEntries = 1000
	}
	if options.MaxBytes <= 0 {
		options.MaxBytes = 8 << 20
	}
	if options.WriteTimeout <= 0 {
		options.WriteTimeout = 2 * time.Second
	}
	r := &Recorder{store: store, options: options, queue: make(chan queued, options.MaxEntries)}
	r.wg.Add(1)
	go r.run()
	return r
}

func (r *Recorder) Enqueue(_ context.Context, record Record) bool {
	r.attempted.Add(1)
	encoded, err := json.Marshal(record)
	if err != nil {
		r.dropped.Add(1)
		return false
	}
	q := queued{record: record, size: len(encoded)}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || q.size > r.options.MaxBytes || r.queuedBytes+q.size > r.options.MaxBytes {
		r.dropped.Add(1)
		return false
	}
	select {
	case r.queue <- q:
		r.queuedBytes += q.size
		return true
	default:
		r.dropped.Add(1)
		return false
	}
}

func (r *Recorder) run() {
	defer r.wg.Done()
	for q := range r.queue {
		r.mu.Lock()
		r.queuedBytes -= q.size
		r.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), r.options.WriteTimeout)
		err := r.store.Write(ctx, q.record)
		cancel()
		if err != nil {
			r.failed.Add(1)
		} else {
			r.written.Add(1)
		}
	}
}

func (r *Recorder) Stats() Stats {
	return Stats{r.attempted.Load(), r.written.Load(), r.dropped.Load(), r.failed.Load()}
}

func (r *Recorder) Close(timeout time.Duration) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	close(r.queue)
	r.mu.Unlock()
	done := make(chan struct{})
	go func() { r.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(timeout):
		r.mu.Lock()
		remaining := len(r.queue)
		r.mu.Unlock()
		r.dropped.Add(uint64(remaining))
	}
}

func Prune(ctx context.Context, store Store, now time.Time) (int64, error) {
	return store.Prune(ctx, now.Add(-30*24*time.Hour))
}
