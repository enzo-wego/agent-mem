package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	"github.com/agent-mem/agent-mem/internal/graph/ids"
	"github.com/agent-mem/agent-mem/internal/graph/normalizer"
)

// Attachment-only messages avoid unrelated extraction and eligibility LLM work.
func describeSlackFixture(pool *pgxpool.Pool) (Deps, string, slackMessage, ingestContentRequest) {
	const channel = "CTESTDESCRIBEDEDUPE"
	msg := slackMessage{
		Ts: "1780000000.000001",
		Files: []slackFile{{
			ID:         "FTESTDESCRIBEDEDUPE",
			MimeType:   "image/png",
			Name:       "dedupe.png",
			Size:       10,
			URLPrivate: "https://files.slack.com/dedupe.png",
		}},
	}
	file := msg.Files[0]
	req := ingestContentRequest{
		Source:       "slack",
		CanonicalURL: "https://slack.com/archives/" + channel + "/p" + slackTsToP(msg.Ts),
		Body:         msg.Text,
		Metadata: ingestContentMetadata{
			ChannelID: channel,
			Ts:        msg.Ts,
			BodyTS:    slackTsToTime(msg.Ts).Format(time.RFC3339),
			Files: []ingestFileRef{{
				ID:         file.ID,
				MimeType:   file.MimeType,
				Filename:   file.Name,
				Size:       file.Size,
				URLPrivate: file.URLPrivate,
			}},
		},
	}
	deps := Deps{
		DB:          pool,
		Logger:      zerolog.Nop(),
		MachineID:   "test-describe-dedupe",
		Normalizers: normalizer.NewRegistry(),
	}
	return deps, channel, msg, req
}

func decodeDescribeIngest(t *testing.T, w *httptest.ResponseRecorder) ingestResponse {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("ingest/content status = %d, want 200: %s", w.Code, w.Body.String())
	}
	var resp ingestResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode ingest response: %v", err)
	}
	return resp
}

func assertDescribeJobCount(t *testing.T, deps Deps, attID string, want int) int64 {
	t.Helper()
	var count int
	var jobID int64
	if err := deps.DB.QueryRow(context.Background(), `
		SELECT count(*), COALESCE(min(id), 0) FROM graph.jobs
		WHERE type='describe_attachment' AND payload->>'node_id'=$1`, attID).
		Scan(&count, &jobID); err != nil {
		t.Fatalf("count describe jobs: %v", err)
	}
	if count != want {
		t.Fatalf("describe jobs for %s = %d, want %d", attID, count, want)
	}
	return jobID
}

func assertDescribeIngestViews(t *testing.T, resp ingestResponse, attID, outcome string, jobID int64) {
	t.Helper()
	if len(resp.AttachmentsRegistered) != 1 {
		t.Fatalf("attachments_registered = %+v, want one entry", resp.AttachmentsRegistered)
	}
	if got := resp.AttachmentsRegistered[0]; got.NodeID != attID || got.Outcome != outcome {
		t.Errorf("attachment view = %+v, want %s/%s", got, attID, outcome)
	}
	var jobs []jobEnqueuedView
	for _, job := range resp.JobsEnqueued {
		if job.Type == "describe_attachment" {
			jobs = append(jobs, job)
		}
	}
	if outcome == "describe_skipped" {
		if len(jobs) != 0 {
			t.Errorf("skipped response contains describe jobs: %+v", jobs)
		}
		return
	}
	if len(jobs) != 1 {
		t.Fatalf("describe jobs_enqueued = %+v, want one entry", jobs)
	}
	if jobs[0].ID <= 0 || jobs[0].ID != jobID {
		t.Errorf("describe job view = %+v, want committed ID %d", jobs[0], jobID)
	}
}

func TestDescribeDedupe_SlackBackfillAndIngest(t *testing.T) {
	t.Run("backfill_twice", func(t *testing.T) {
		pool := resetFetchTest(t)
		deps, channel, msg, _ := describeSlackFixture(pool)
		attID := ids.SlackFile(msg.Files[0].ID)
		for i := range 2 {
			if err := ingestSlackMessage(context.Background(), deps, channel, msg); err != nil {
				t.Fatalf("backfill %d: %v", i+1, err)
			}
			assertDescribeJobCount(t, deps, attID, 1)
		}
	})

	t.Run("described", func(t *testing.T) {
		for _, producer := range []string{"backfill", "ingest"} {
			t.Run(producer, func(t *testing.T) {
				pool := resetFetchTest(t)
				deps, channel, msg, req := describeSlackFixture(pool)
				attID := ids.SlackFile(msg.Files[0].ID)
				if _, err := pool.Exec(context.Background(), `
					INSERT INTO graph.nodes (id, type, natural_key, machine_id)
					VALUES ($1, 'slack_file', $2, $3)`, attID, msg.Files[0].ID, deps.MachineID); err != nil {
					t.Fatal(err)
				}
				if _, err := pool.Exec(context.Background(), `
					INSERT INTO graph.artifact_bodies (node_id, body_full, machine_id)
					VALUES ($1, 'already described', $2)`, attID, deps.MachineID); err != nil {
					t.Fatal(err)
				}
				if producer == "backfill" {
					if err := ingestSlackMessage(context.Background(), deps, channel, msg); err != nil {
						t.Fatalf("backfill: %v", err)
					}
				} else {
					resp := decodeDescribeIngest(t, postJSON(t, NewIngestContentHandler(deps), req))
					assertDescribeIngestViews(t, resp, attID, "describe_skipped", 0)
				}
				assertDescribeJobCount(t, deps, attID, 0)
			})
		}
	})

	t.Run("ingest_twice", func(t *testing.T) {
		pool := resetFetchTest(t)
		deps, _, msg, req := describeSlackFixture(pool)
		attID := ids.SlackFile(msg.Files[0].ID)
		handler := NewIngestContentHandler(deps)
		first := decodeDescribeIngest(t, postJSON(t, handler, req))
		jobID := assertDescribeJobCount(t, deps, attID, 1)
		assertDescribeIngestViews(t, first, attID, "queued_for_describe", jobID)
		second := decodeDescribeIngest(t, postJSON(t, handler, req))
		assertDescribeIngestViews(t, second, attID, "describe_skipped", 0)
		assertDescribeJobCount(t, deps, attID, 1)
	})

	t.Run("ingest_eligibility_error", func(t *testing.T) {
		pool := resetFetchTest(t)
		deps, _, msg, req := describeSlackFixture(pool)
		attID := ids.SlackFile(msg.Files[0].ID)
		orig := describeEligibilityCheck
		describeEligibilityCheck = func(ctx context.Context, tx pgx.Tx, attID string) (bool, error) {
			return false, context.DeadlineExceeded
		}
		t.Cleanup(func() { describeEligibilityCheck = orig })
		resp := decodeDescribeIngest(t, postJSON(t, NewIngestContentHandler(deps), req))
		assertDescribeIngestViews(t, resp, attID, "describe_skipped", 0)
		assertDescribeJobCount(t, deps, attID, 0)
	})
}

func TestDescribeDedupe_MixedProducerConcurrent(t *testing.T) {
	pool := resetFetchTest(t)
	deps, channel, msg, req := describeSlackFixture(pool)
	attID := ids.SlackFile(msg.Files[0].ID)
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	handler := NewIngestContentHandler(deps)
	start := make(chan struct{})
	backfillDone := make(chan error, 1)
	ingestDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		<-start
		backfillDone <- ingestSlackMessage(context.Background(), deps, channel, msg)
	}()
	go func() {
		<-start
		r := httptest.NewRequest(http.MethodPost, "/api/graph/ingest/content", bytes.NewReader(raw))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		ingestDone <- w
	}()
	close(start)
	backfillErr := <-backfillDone
	w := <-ingestDone
	if backfillErr != nil {
		t.Fatalf("concurrent backfill: %v", backfillErr)
	}
	resp := decodeDescribeIngest(t, w)
	jobID := assertDescribeJobCount(t, deps, attID, 1)
	if len(resp.AttachmentsRegistered) != 1 {
		t.Fatalf("concurrent attachment views = %+v, want one", resp.AttachmentsRegistered)
	}
	switch resp.AttachmentsRegistered[0].Outcome {
	case "queued_for_describe":
		assertDescribeIngestViews(t, resp, attID, "queued_for_describe", jobID)
	case "describe_skipped":
		assertDescribeIngestViews(t, resp, attID, "describe_skipped", 0)
	default:
		t.Fatalf("unexpected concurrent attachment outcome: %+v", resp.AttachmentsRegistered)
	}
}
