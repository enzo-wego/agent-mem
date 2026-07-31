package jobs

import (
	"context"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
	"golang.org/x/sync/semaphore"
)

// ManagerConfig wires the whole worker subsystem.
type ManagerConfig struct {
	Registry            *Registry
	DB                  *pgxpool.Pool
	WorkerID            string
	Runner              string                        // "vps" or "local"
	Semaphores          map[string]*semaphore.Weighted
	IdleInterval        time.Duration // dispatcher slow-poll cadence
	BackoffBase         time.Duration
	BackoffCap          time.Duration
	JanitorScanInterval time.Duration
	JanitorBatchSize    int
	Logger              zerolog.Logger

	// Paused suspends job execution across every dispatcher. See
	// DispatcherConfig.Paused. The janitor keeps running while paused — it only
	// reclaims expired leases, which costs nothing and keeps the queue tidy.
	Paused func() bool
}

// Manager owns one TypeDispatcher per registered type plus one Janitor.
type Manager struct {
	cfg         ManagerConfig
	wg          sync.WaitGroup
	dispatchers []*TypeDispatcher
	janitor     *Janitor
}

// NewManager constructs the manager but does not start anything yet.
func NewManager(cfg ManagerConfig) *Manager {
	return &Manager{cfg: cfg}
}

// Run starts one dispatcher per registered type and the janitor.
// Returns when ctx is cancelled. Call Wait() afterward to block until
// all goroutines have exited.
func (m *Manager) Run(ctx context.Context) {
	for _, typ := range m.cfg.Registry.Types() {
		d := NewTypeDispatcher(DispatcherConfig{
			Type:         typ,
			Registry:     m.cfg.Registry,
			DB:           m.cfg.DB,
			WorkerID:     m.cfg.WorkerID,
			Runner:       m.cfg.Runner,
			IdleInterval: m.cfg.IdleInterval,
			Semaphores:   m.cfg.Semaphores,
			BackoffBase:  m.cfg.BackoffBase,
			BackoffCap:   m.cfg.BackoffCap,
			Logger:       m.cfg.Logger,
			Paused:       m.cfg.Paused,
		})
		m.dispatchers = append(m.dispatchers, d)
		m.wg.Add(1)
		go func(d *TypeDispatcher) {
			defer m.wg.Done()
			d.Run(ctx)
		}(d)
	}

	m.janitor = NewJanitor(JanitorConfig{
		DB:           m.cfg.DB,
		ScanInterval: m.cfg.JanitorScanInterval,
		BatchSize:    m.cfg.JanitorBatchSize,
		Logger:       m.cfg.Logger,
	})
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.janitor.Run(ctx)
	}()
}

// Wait blocks until all dispatchers and the janitor have stopped.
func (m *Manager) Wait() {
	m.wg.Wait()
}
