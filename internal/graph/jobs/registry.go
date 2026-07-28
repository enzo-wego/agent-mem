package jobs

import (
	"context"
	"sync"
	"time"
)

// Handler is the signature every job-type handler implements.
type Handler func(ctx context.Context, payload []byte) error

// Entry is one row in the worker registry.
type Entry struct {
	// PoolSize is the maximum number of concurrent goroutines that may run
	// this job type at once.
	PoolSize int
	// Lease is how long a claim is valid before the janitor considers the
	// row stuck. Should be ~3× the realistic max duration of the handler.
	Lease time.Duration
	// Systems is the list of external system names whose semaphores this
	// handler will acquire. Acquired in sorted order to prevent deadlock.
	// Examples: ["jira"], ["slack","gemini"], ["gemini"].
	Systems []string
	// Heartbeat is true if the handler may run longer than the lease and
	// needs lease extension via a background goroutine. Default false.
	Heartbeat bool
	// Handler runs the actual work.
	Handler Handler
}

// Registry maps job-type name → Entry.
type Registry struct {
	mu      sync.RWMutex
	entries map[string]Entry
}

// NewRegistry returns a registry preloaded with default entries for the
// seven Phase-1 handlers. Handler is nil — the worker boot wires real
// handlers via Register before starting the Manager.
func NewRegistry() *Registry {
	r := &Registry{entries: make(map[string]Entry)}
	r.entries["fetch_body"] = Entry{PoolSize: 8, Lease: 60 * time.Second}
	r.entries["describe_attachment"] = Entry{PoolSize: 4, Lease: 120 * time.Second}
	r.entries["resolve_identity"] = Entry{PoolSize: 4, Lease: 30 * time.Second}
	r.entries["index_artifact"] = Entry{PoolSize: 4, Lease: 60 * time.Second}
	r.entries["refresh_slack_groups"] = Entry{PoolSize: 1, Lease: 600 * time.Second, Heartbeat: true}
	r.entries["derive_person_roles"] = Entry{PoolSize: 1, Lease: 300 * time.Second}
	r.entries["import_bamboohr"] = Entry{PoolSize: 1, Lease: 600 * time.Second, Heartbeat: true}
	r.entries["recompute_person_distance"] = Entry{PoolSize: 1, Lease: 600 * time.Second, Heartbeat: true}
	r.entries["backfill_slack_channel"] = Entry{PoolSize: 2, Lease: 120 * time.Second, Systems: []string{"slack"}}
	r.entries["backfill_slack_thread"] = Entry{PoolSize: 2, Lease: 120 * time.Second, Systems: []string{"slack"}}
	return r
}

// Register adds or overrides an entry. Used at boot to bind Handler and
// override defaults from app_settings.
func (r *Registry) Register(typ string, e Entry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries[typ] = e
}

// Get returns the entry for a type, plus ok=true if found.
func (r *Registry) Get(typ string) (Entry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.entries[typ]
	return e, ok
}

// Types returns the set of registered type names.
func (r *Registry) Types() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.entries))
	for k := range r.entries {
		out = append(out, k)
	}
	return out
}
