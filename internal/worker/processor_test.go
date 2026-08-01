package worker

import (
	"context"
	"errors"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/agent-mem/agent-mem/internal/database"
	"github.com/agent-mem/agent-mem/internal/llmgateway"
)

// databaseName extracts the database name from a postgres DSN. A DSN that does
// not parse returns "", which fails the test-database guard closed.
func databaseName(dsn string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(u.Path, "/")
}

type failingFlatLLM struct {
	err error
}

func (f failingFlatLLM) GenerateCheap(context.Context, string, string) (string, error) {
	return "", f.err
}

func (f failingFlatLLM) Embed(context.Context, string) ([]float32, error) {
	return nil, errors.New("unexpected embed call")
}

func openProcessorTestDB(t *testing.T) *database.DB {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping integration test")
	}
	// This helper DELETEs every row in pending_messages. On 2026-07-14 an
	// integration test run against the live dev database hard-deleted the graph
	// and synced the damage to prod. Refuse anything whose database name does
	// not say "test" — use agentmem_test, not agentmem. See agent-mem-z14.
	if !strings.Contains(databaseName(dsn), "test") {
		t.Fatalf("refusing to run: DATABASE_URL database name %q does not contain \"test\"; "+
			"this test deletes all rows in pending_messages", databaseName(dsn))
	}
	workingDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	if err := os.Chdir("../.."); err != nil {
		t.Fatalf("change to repository root: %v", err)
	}
	err = database.RunMigrations(dsn)
	if restoreErr := os.Chdir(workingDir); restoreErr != nil {
		t.Fatalf("restore working directory: %v", restoreErr)
	}
	if err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	pool, err := database.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	t.Cleanup(pool.Close)

	if _, err := pool.Exec(context.Background(), "DELETE FROM pending_messages"); err != nil {
		t.Fatalf("clear pending messages: %v", err)
	}
	if _, err := pool.Exec(context.Background(), "DELETE FROM sdk_sessions"); err != nil {
		t.Fatalf("clear sdk sessions: %v", err)
	}

	return database.NewDB(pool)
}

func pendingMessageState(t *testing.T, db *database.DB, id int64) (string, int) {
	t.Helper()
	var status string
	var attempts int
	err := db.Pool.QueryRow(context.Background(), `
		SELECT status, COALESCE((to_jsonb(pending_messages)->>'attempts')::int, -1)
		FROM pending_messages
		WHERE id = $1
	`, id).Scan(&status, &attempts)
	if err != nil {
		t.Fatalf("query pending message state: %v", err)
	}
	return status, attempts
}

func pendingMessageAvailable(t *testing.T, db *database.DB, id int64) bool {
	t.Helper()
	var available bool
	err := db.Pool.QueryRow(context.Background(), `
		SELECT available_at > NOW()
		FROM pending_messages
		WHERE id = $1
	`, id).Scan(&available)
	if err != nil {
		t.Fatalf("query pending message availability: %v", err)
	}
	return available
}

func TestProcessPendingMessages_FirstNonRetryableFailureRequeues(t *testing.T) {
	db := openProcessorTestDB(t)
	id, err := db.QueuePendingMessage(context.Background(), "missing-session", "observation", []byte(`{}`))
	if err != nil {
		t.Fatalf("queue pending message: %v", err)
	}

	s := &Server{db: db, flatLLM: failingFlatLLM{}}
	s.processPendingMessages(context.Background())

	status, attempts := pendingMessageState(t, db, id)
	if status != "pending" {
		t.Errorf("status = %q, want pending", status)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1", attempts)
	}
	if !pendingMessageAvailable(t, db, id) {
		t.Error("available_at is not in the future")
	}
}

func TestProcessPendingMessages_FutureMessageIsNotClaimed(t *testing.T) {
	db := openProcessorTestDB(t)
	ctx := context.Background()
	id, err := db.QueuePendingMessage(ctx, "missing-session", "observation", []byte(`{}`))
	if err != nil {
		t.Fatalf("queue pending message: %v", err)
	}
	if _, err := db.Pool.Exec(ctx, `
		UPDATE pending_messages
		SET available_at = NOW() + INTERVAL '1 minute'
		WHERE id = $1
	`, id); err != nil {
		t.Fatalf("delay pending message: %v", err)
	}

	s := &Server{db: db, flatLLM: failingFlatLLM{}}
	s.processPendingMessages(ctx)

	status, attempts := pendingMessageState(t, db, id)
	if status != "pending" {
		t.Errorf("status = %q, want pending", status)
	}
	if attempts != 0 {
		t.Errorf("attempts = %d, want 0", attempts)
	}
}

func TestProcessPendingMessages_ThirdNonRetryableFailureIsTerminal(t *testing.T) {
	db := openProcessorTestDB(t)
	id, err := db.QueuePendingMessage(context.Background(), "missing-session", "observation", []byte(`{}`))
	if err != nil {
		t.Fatalf("queue pending message: %v", err)
	}

	s := &Server{db: db, flatLLM: failingFlatLLM{}}
	for range 3 {
		// Requeued failures are deliberately delayed; make each retry due so this
		// test can exhaust the budget without waiting for the backoff.
		if _, err := db.Pool.Exec(context.Background(), `
			UPDATE pending_messages SET available_at = NOW() WHERE id = $1
		`, id); err != nil {
			t.Fatalf("make pending message available: %v", err)
		}
		s.processPendingMessages(context.Background())
	}

	status, attempts := pendingMessageState(t, db, id)
	if status != "failed" {
		t.Errorf("status = %q, want failed", status)
	}
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3", attempts)
	}
}

func TestProcessPendingMessages_RetryableFailureDoesNotSpendBudget(t *testing.T) {
	db := openProcessorTestDB(t)
	ctx := context.Background()
	session, err := db.UpsertSession(ctx, "retryable-session", "test-project")
	if err != nil {
		t.Fatalf("upsert session: %v", err)
	}
	if session.MemorySessionID == nil {
		t.Fatal("upserted session has no memory session ID")
	}
	id, err := db.QueuePendingMessage(ctx, "retryable-session", "observation", []byte(`{"tool_name":"Read","tool_input":{},"tool_response":{},"cwd":"/tmp","project":"test-project"}`))
	if err != nil {
		t.Fatalf("queue pending message: %v", err)
	}

	s := &Server{
		db:      db,
		flatLLM: failingFlatLLM{err: llmgateway.ErrUnreachable},
	}
	processCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	s.processPendingMessages(processCtx)

	status, attempts := pendingMessageState(t, db, id)
	if status != "pending" {
		t.Errorf("status = %q, want pending", status)
	}
	if attempts != 0 {
		t.Errorf("attempts = %d, want 0", attempts)
	}
}
