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

// NewRefreshSlackChannelsHandler returns the job entry for "refresh_slack_channels":
// it pulls every channel the bot can see (conversations.list) into
// graph.slack_channels so the map can label channels by name, not raw id.
func NewRefreshSlackChannelsHandler(deps Deps) jobs.Entry {
	return jobs.Entry{
		Handler:  refreshSlackChannelsHandler(deps),
		Systems:  []string{"slack"},
		PoolSize: 1,
		Lease:    300 * time.Second,
	}
}

func refreshSlackChannelsHandler(deps Deps) jobs.Handler {
	return func(ctx context.Context, _ []byte) error {
		token := os.Getenv("SLACK_BOT_TOKEN")
		if token == "" {
			return fmt.Errorf("%w: refresh_slack_channels: SLACK_BOT_TOKEN not set", jobs.ErrFatal)
		}

		channels, err := fetchSlackChannels(ctx, token)
		if err != nil {
			return err
		}

		for _, c := range channels {
			if c.Name == "" {
				continue
			}
			if _, e := deps.DB.Exec(ctx, `
				INSERT INTO graph.slack_channels (slack_channel_id, name, refreshed_at, machine_id)
				VALUES ($1, $2, NOW(), $3)
				ON CONFLICT (slack_channel_id) DO UPDATE SET
					name         = EXCLUDED.name,
					refreshed_at = NOW()`,
				c.ID, c.Name, deps.MachineID,
			); e != nil {
				deps.Logger.Warn().Err(e).Str("cid", c.ID).Msg("refresh_slack_channels: upsert failed")
			}
		}

		deps.Logger.Info().Int("count", len(channels)).Msg("refresh_slack_channels: done")
		return nil
	}
}

// slackChannel is the relevant slice of a Slack conversations.list channel object.
type slackChannel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// fetchSlackChannels calls conversations.list (public + private) with cursor
// pagination and returns all channels.
func fetchSlackChannels(ctx context.Context, token string) ([]slackChannel, error) {
	client := &http.Client{Timeout: 60 * time.Second}
	var out []slackChannel
	cursor := ""
	for page := 0; page < 50; page++ { // hard cap: 50 pages × 1000 = 50k channels
		u := "https://slack.com/api/conversations.list?limit=1000&exclude_archived=false&types=public_channel,private_channel"
		if cursor != "" {
			u += "&cursor=" + url.QueryEscape(cursor)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, fmt.Errorf("%w: refresh_slack_channels: build request: %v", jobs.ErrFatal, err)
		}
		req.Header.Set("Authorization", "Bearer "+token)

		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("%w: refresh_slack_channels: http: %v", jobs.ErrTransient, err)
		}
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("%w: refresh_slack_channels: read: %v", jobs.ErrTransient, readErr)
		}
		if resp.StatusCode == 429 || resp.StatusCode >= 500 {
			return nil, fmt.Errorf("%w: refresh_slack_channels: HTTP %d", jobs.ErrTransient, resp.StatusCode)
		}
		if resp.StatusCode >= 400 {
			return nil, fmt.Errorf("%w: refresh_slack_channels: HTTP %d", jobs.ErrFatal, resp.StatusCode)
		}

		var apiResp struct {
			OK       bool           `json:"ok"`
			Channels []slackChannel `json:"channels"`
			Error    string         `json:"error"`
			Meta     struct {
				NextCursor string `json:"next_cursor"`
			} `json:"response_metadata"`
		}
		if err := json.Unmarshal(body, &apiResp); err != nil {
			return nil, fmt.Errorf("%w: refresh_slack_channels: parse: %v", jobs.ErrTransient, err)
		}
		if !apiResp.OK {
			return nil, fmt.Errorf("%w: refresh_slack_channels: slack API error: %s", jobs.ErrTransient, apiResp.Error)
		}
		out = append(out, apiResp.Channels...)
		cursor = apiResp.Meta.NextCursor
		if cursor == "" {
			break
		}
	}
	return out, nil
}
