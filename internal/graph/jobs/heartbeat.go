package jobs

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
)

// Heartbeat is the handle returned by StartHeartbeat. Call Stop when the
// handler returns to release resources.
type Heartbeat struct {
	stop chan struct{}
	done chan struct{}
}

// Stop signals the heartbeat goroutine to exit and waits for it.
func (h *Heartbeat) Stop() {
	if h == nil {
		return
	}
	close(h.stop)
	<-h.done
}

// StartHeartbeat launches a goroutine that bumps lease_until on a job at
// 1/3 of the lease interval. It exits when ctx is cancelled, Stop is
// called, or a DB error occurs.
func StartHeartbeat(
	ctx context.Context,
	db *pgxpool.Pool,
	jobID int64,
	lease time.Duration,
	logger zerolog.Logger,
) *Heartbeat {
	hb := &Heartbeat{
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	tickEvery := lease / 3
	if tickEvery < 500*time.Millisecond {
		tickEvery = 500 * time.Millisecond
	}
	go func() {
		defer close(hb.done)
		t := time.NewTicker(tickEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-hb.stop:
				return
			case <-t.C:
				_, err := db.Exec(ctx, `
					UPDATE graph.jobs
					SET lease_until = NOW() + ($2 || ' seconds')::interval
					WHERE id = $1 AND status = 'running'
				`, jobID, fmt.Sprintf("%d", int(lease/time.Second)))
				if err != nil {
					logger.Warn().Err(err).Int64("job_id", jobID).Msg("heartbeat update failed")
				}
			}
		}
	}()
	return hb
}
