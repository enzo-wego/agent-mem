package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

func TestBackfillSlackHandler_TargetRunner(t *testing.T) {
	pool := openTestDB(t)
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		runner string
		want   string
	}{
		{name: "configured_local", runner: "local", want: "local"},
		{name: "empty_uses_queue_default", runner: "", want: "any"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			truncateGraphHandlerTables(t, pool)
			h := NewBackfillSlackHandler(Deps{
				DB:        pool,
				Logger:    zerolog.Nop(),
				MachineID: "test-runner-routing",
				Runner:    tc.runner,
			})
			req := httptest.NewRequest(http.MethodPost, "/api/graph/backfill/slack", strings.NewReader(`{"channel_id":"C123","months":1}`))
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != http.StatusAccepted {
				t.Fatalf("status = %d, want %d: %s", w.Code, http.StatusAccepted, w.Body.String())
			}
			var response backfillSlackResponse
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			var runner, jobType, machineID, channelID string
			if err := pool.QueryRow(ctx, `
				SELECT target_runner, type, machine_id, payload->>'channel_id'
				FROM graph.jobs WHERE id = $1`, response.JobID).Scan(&runner, &jobType, &machineID, &channelID); err != nil {
				t.Fatalf("read enqueued job: %v", err)
			}
			if runner != tc.want {
				t.Errorf("target_runner = %q, want %q for deps.Runner = %q", runner, tc.want, tc.runner)
			}
			if jobType != "backfill_slack_channel" || machineID != "test-runner-routing" || channelID != "C123" {
				t.Errorf("enqueued job type=%q machine_id=%q channel_id=%q, want backfill_slack_channel/test-runner-routing/C123", jobType, machineID, channelID)
			}
		})
	}
}
