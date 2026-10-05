package handlers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// Periodic scheduling has a five-second operation limit, but unrelated admin
// retries must continue using the caller's longer request deadline.
func TestAdminRetry_NonPeriodicRequestDeadline(t *testing.T) {
	pool := periodicHandlerDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var id int64
	if err := pool.QueryRow(ctx, `INSERT INTO graph.jobs(type,payload,status,machine_id,attempts)
		VALUES('fetch_body','{}','failed','retry-deadline-test',3) RETURNING id`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	var blockerPID int
	var lockedID int64
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid(),id FROM graph.jobs WHERE id=$1 FOR UPDATE`, id).Scan(&blockerPID, &lockedID); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	w := httptest.NewRecorder()
	req := chiRequest(http.MethodPost, "/retry", "", "id", fmt.Sprint(id))
	requestCtx, requestCancel := context.WithTimeout(req.Context(), 15*time.Second)
	defer requestCancel()
	req = req.WithContext(requestCtx)
	go func() {
		defer close(done)
		NewJobsRetryHandler(Deps{DB: pool, Logger: zerolog.Nop()}).ServeHTTP(w, req)
	}()
	t.Cleanup(func() {
		cancel()
		requestCancel()
		_ = tx.Rollback(context.Background())
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("retry request did not stop")
		}
	})
	waitDeadline := time.Now().Add(2 * time.Second)
	for {
		var blocked bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, blockerPID).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		if time.Now().After(waitDeadline) {
			t.Fatal("retry never reached locked UPDATE")
		}
		time.Sleep(5 * time.Millisecond)
	}
	timer := time.NewTimer(5500 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-done:
		t.Fatalf("non-periodic retry adopted periodic deadline: status=%d body=%s", w.Code, w.Body)
	case <-timer.C:
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("retry did not recover after row lock release")
	}
	if w.Code != http.StatusOK {
		t.Fatalf("retry status=%d body=%s", w.Code, w.Body)
	}
	var status string
	var attempts int
	if err := pool.QueryRow(ctx, `SELECT status,attempts FROM graph.jobs WHERE id=$1`, id).Scan(&status, &attempts); err != nil {
		t.Fatal(err)
	}
	if status != "queued" || attempts != 0 {
		t.Fatalf("retry left status=%s attempts=%d", status, attempts)
	}
}
