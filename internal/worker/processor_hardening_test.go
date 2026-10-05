package worker

import (
	"context"
	"testing"
	"time"
)

func TestProcessPendingMessages_NoLLMLeavesPending(t *testing.T) {
	db := openProcessorTestDB(t)
	ctx := context.Background()
	id, err := db.QueuePendingMessage(ctx, "no-client", "observation", []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{db: db}
	s.processPendingMessages(ctx)
	status, attempts := pendingMessageState(t, db, id)
	if status != "pending" || attempts != 0 {
		t.Fatalf("status=%s attempts=%d, want pending/0", status, attempts)
	}
}

// Block the claim at PostgreSQL so the client disappears after the pre-claim
// check but before processMessage. No timing-dependent client getter is needed.
func TestProcessPendingMessages_ClientLostRefundsAtCap(t *testing.T) {
	db := openProcessorTestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id, err := db.QueuePendingMessage(ctx, "lost-client", "observation", []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool.Exec(ctx, `UPDATE pending_messages SET attempts=$2 WHERE id=$1`, id, maxMessageAttempts); err != nil {
		t.Fatal(err)
	}
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	if _, err := tx.Exec(ctx, `LOCK TABLE pending_messages IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	s := &Server{db: db, flatLLM: failingFlatLLM{}}
	done := make(chan struct{})
	go func() { defer close(done); s.processPendingMessages(ctx) }()
	for {
		var waiting bool
		err := db.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND wait_event_type='Lock' AND query LIKE '%UPDATE pending_messages%')`).Scan(&waiting)
		if err != nil {
			t.Fatal(err)
		}
		if waiting {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("claim did not reach database lock")
		case <-time.After(10 * time.Millisecond):
		}
	}
	s.mu.Lock()
	s.flatLLM = nil
	s.mu.Unlock()
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("processor did not return")
	}
	status, attempts := pendingMessageState(t, db, id)
	if status != "pending" || attempts != maxMessageAttempts {
		t.Fatalf("status=%s attempts=%d, want pending/%d", status, attempts, maxMessageAttempts)
	}
	if !pendingMessageAvailable(t, db, id) {
		t.Fatal("missing-client message was not delayed")
	}
}

type capturedFlatLLM struct {
	server   *Server
	embedded bool
}

func (f *capturedFlatLLM) GenerateCheap(context.Context, string, string) (string, error) {
	f.server.mu.Lock()
	f.server.flatLLM = nil
	f.server.mu.Unlock()
	return `{"request":"capture-client","investigated":"fixture","learned":"captured","completed":"stored","next_steps":"none"}`, nil
}
func (f *capturedFlatLLM) Embed(context.Context, string) ([]float32, error) {
	f.embedded = true
	return nil, nil
}

func TestProcessPendingMessages_CapturedClientCompletes(t *testing.T) {
	db := openProcessorTestDB(t)
	ctx := context.Background()
	if _, err := db.UpsertSession(ctx, "capture-client", "test-project"); err != nil {
		t.Fatal(err)
	}
	id, err := db.QueuePendingMessage(ctx, "capture-client", "summary", []byte(`{"last_assistant_message":"finished","project":"test-project"}`))
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{db: db}
	client := &capturedFlatLLM{server: s}
	calls := 0
	s.flatLLMGetter = func() flatLLM {
		calls++
		// One pre-claim check and one captured client for processMessage.
		// Any redundant per-type lookup sees a removed client.
		if calls <= 2 {
			return client
		}
		return nil
	}
	s.processPendingMessages(ctx)
	status, attempts := pendingMessageState(t, db, id)
	if status != "completed" || attempts != 1 {
		t.Fatalf("status=%s attempts=%d, want completed/1", status, attempts)
	}
	if !client.embedded {
		t.Fatal("captured client did not embed summary after reload")
	}
	var count int
	if err := db.Pool.QueryRow(ctx, `SELECT count(*) FROM session_summaries WHERE request='capture-client'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("stored summaries=%d, want 1", count)
	}
	if calls != 2 {
		t.Fatalf("client resolutions=%d, want pre-claim + one captured client", calls)
	}
}
