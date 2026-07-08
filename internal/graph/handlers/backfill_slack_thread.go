package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"

	"github.com/agent-mem/agent-mem/internal/graph/jobs"
)

// backfillSlackThreadPayload is the JSON payload for the backfill_slack_thread job type.
type backfillSlackThreadPayload struct {
	ChannelID string `json:"channel_id"`
	ThreadTs  string `json:"thread_ts"`
	Cursor    string `json:"cursor"`
}

// NewBackfillSlackThreadHandler returns a HandlerInfo for the "backfill_slack_thread" job type.
func NewBackfillSlackThreadHandler(deps Deps) jobs.Entry {
	return jobs.Entry{
		Handler:  backfillSlackThreadHandler(deps),
		Systems:  []string{"slack"},
		PoolSize: 2,
		Lease:    120 * time.Second,
	}
}

func backfillSlackThreadHandler(deps Deps) jobs.Handler {
	return func(ctx context.Context, payload []byte) error {
		var p backfillSlackThreadPayload
		if err := json.Unmarshal(payload, &p); err != nil {
			return fmt.Errorf("%w: backfill_slack_thread unmarshal: %v", jobs.ErrFatal, err)
		}
		if p.ChannelID == "" || p.ThreadTs == "" {
			return fmt.Errorf("%w: backfill_slack_thread: channel_id and thread_ts required", jobs.ErrFatal)
		}

		token := os.Getenv("SLACK_BOT_TOKEN")
		if token == "" {
			return fmt.Errorf("%w: backfill_slack_thread: SLACK_BOT_TOKEN not set", jobs.ErrFatal)
		}

		// Call conversations.replies.
		repliesResp, err := fetchConversationsReplies(ctx, token, p.ChannelID, p.ThreadTs, p.Cursor)
		if err != nil {
			return err
		}

		// Ingest every message in the thread, INCLUDING the parent/root. Ingestion is
		// an idempotent upsert, so re-processing the root (which Slack returns first on
		// each page) is harmless — and crucial when the root predates the channel
		// backfill window and isn't ingested yet (old thread, fresh reply).
		for _, msg := range repliesResp.Messages {
			// Skip bot self and subtype messages.
			automated := msg.BotID != "" || msg.Subtype == "bot_message"
			alertDecision := decideAlertBot(ctx, deps, p.ChannelID, msg.Text, automated)
			if alertDecision.Skip {
				continue
			}
			if msg.Subtype != "" && !alertDecision.Escalate {
				continue
			}
			if msg.BotID != "" && msg.User == "" && !alertDecision.Escalate {
				continue
			}

			if err := ingestSlackMessage(ctx, deps, p.ChannelID, msg); err != nil {
				deps.Logger.Warn().Err(err).
					Str("channel_id", p.ChannelID).
					Str("thread_ts", p.ThreadTs).
					Str("ts", msg.Ts).
					Msg("backfill_slack_thread: ingest reply failed; skipping")
			}
		}

		// If there are more pages, re-enqueue self with the next cursor; otherwise
		// the thread is complete — (re)generate its topic summary in the background.
		if repliesResp.ResponseMetadata.NextCursor != "" {
			nextPayload := backfillSlackThreadPayload{
				ChannelID: p.ChannelID,
				ThreadTs:  p.ThreadTs,
				Cursor:    repliesResp.ResponseMetadata.NextCursor,
			}
			if _, jErr := jobs.Enqueue(ctx, deps.DB, "backfill_slack_thread", nextPayload, jobs.EnqueueOptions{
				Priority:     5,
				TargetRunner: "vps",
				MachineID:    deps.MachineID,
			}); jErr != nil {
				return fmt.Errorf("backfill_slack_thread: re-enqueue next page: %w", jErr)
			}
		} else {
			enqueueSummarizeThread(ctx, deps.DB, p.ChannelID, p.ThreadTs, false)
		}

		return nil
	}
}

type slackRepliesResponse struct {
	OK               bool           `json:"ok"`
	Messages         []slackMessage `json:"messages"`
	Error            string         `json:"error"`
	ResponseMetadata struct {
		NextCursor string `json:"next_cursor"`
	} `json:"response_metadata"`
}

// fetchConversationsReplies calls Slack conversations.replies with pagination support.
func fetchConversationsReplies(ctx context.Context, token, channelID, threadTs, cursor string) (*slackRepliesResponse, error) {
	params := url.Values{}
	params.Set("channel", channelID)
	params.Set("ts", threadTs)
	params.Set("limit", "200")
	if cursor != "" {
		params.Set("cursor", cursor)
	}

	apiURL := "https://slack.com/api/conversations.replies?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: backfill_slack_thread: build request: %v", jobs.ErrFatal, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: backfill_slack_thread: http: %v", jobs.ErrTransient, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%w: backfill_slack_thread: read body: %v", jobs.ErrTransient, err)
	}

	if resp.StatusCode == 429 || resp.StatusCode >= 500 {
		return nil, fmt.Errorf("%w: backfill_slack_thread: HTTP %d", jobs.ErrTransient, resp.StatusCode)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("%w: backfill_slack_thread: HTTP %d", jobs.ErrFatal, resp.StatusCode)
	}

	var apiResp slackRepliesResponse
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return nil, fmt.Errorf("%w: backfill_slack_thread: parse response: %v", jobs.ErrTransient, err)
	}
	if !apiResp.OK {
		if apiResp.Error == "ratelimited" {
			return nil, fmt.Errorf("%w: backfill_slack_thread: ratelimited", jobs.ErrTransient)
		}
		return nil, fmt.Errorf("%w: backfill_slack_thread: slack error: %s", jobs.ErrTransient, apiResp.Error)
	}

	return &apiResp, nil
}
