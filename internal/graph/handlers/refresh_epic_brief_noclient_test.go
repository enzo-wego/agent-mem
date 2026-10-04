package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/rs/zerolog"

	"github.com/agent-mem/agent-mem/internal/graph/jobs"
)

func TestRefreshEpicBrief_NoClientFails(t *testing.T) {
	h := NewRefreshEpicBriefHandler(Deps{Logger: zerolog.Nop()}).Handler
	for _, dry := range []bool{false, true} {
		payload, _ := json.Marshal(refreshEpicBriefPayload{EpicKey: "PAY-100", DryRun: dry})
		err := h(context.Background(), payload)
		if err == nil || !errors.Is(err, jobs.ErrFatal) {
			t.Fatalf("dry=%v: err = %v, want ErrFatal", dry, err)
		}
	}
}

func TestEnqueueEpicBriefs_NoClientEnqueuesNothing(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%v\n%s", err, sql)
		}
	}
	exec(`DELETE FROM settings WHERE key LIKE 'graph.epic_briefs.%'`)
	t.Cleanup(func() {
		truncateGraphHandlerTables(t, pool)
		_, _ = pool.Exec(ctx, `DELETE FROM settings WHERE key LIKE 'graph.epic_briefs.%'`)
	})
	exec(`INSERT INTO settings(key,value) VALUES('graph.epic_briefs.enabled','true')`)
	seedEpicFixture(t, pool)
	if err := rebuildEpicHierarchy(ctx, pool, "test", "PAY", map[string]int{"PAY-100": 0}); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if n := enqueueEpicBriefs(ctx, Deps{DB: pool, Logger: zerolog.Nop(), MachineID: "test"}, "PAY"); n != 0 {
		t.Fatalf("enqueued %d with no client", n)
	}
	var jobsN int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM graph.jobs WHERE type='refresh_epic_brief'`).Scan(&jobsN)
	if jobsN != 0 {
		t.Fatalf("jobs rows = %d", jobsN)
	}
}
