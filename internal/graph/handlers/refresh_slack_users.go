package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/agent-mem/agent-mem/internal/graph/jobs"
)

// NewRefreshSlackUsersHandler returns a HandlerInfo for the "refresh_slack_users" job type.
func NewRefreshSlackUsersHandler(deps Deps) jobs.Entry {
	return jobs.Entry{
		Handler:  refreshSlackUsersHandler(deps),
		Systems:  []string{"slack"},
		PoolSize: 1,
		Lease:    300 * time.Second,
	}
}

func refreshSlackUsersHandler(deps Deps) jobs.Handler {
	return func(ctx context.Context, _ []byte) error {
		token := os.Getenv("SLACK_BOT_TOKEN")
		if token == "" {
			return fmt.Errorf("%w: refresh_slack_users: SLACK_BOT_TOKEN not set", jobs.ErrFatal)
		}

		users, err := fetchSlackUsers(ctx, token)
		if err != nil {
			return err
		}

		for _, u := range users {
			name := slackBestName(u)
			if name == "" {
				continue
			}
			if _, e := deps.DB.Exec(ctx, `
				INSERT INTO graph.slack_users (slack_user_id, display_name, is_bot, refreshed_at, machine_id)
				VALUES ($1, $2, $3, NOW(), $4)
				ON CONFLICT (slack_user_id) DO UPDATE SET
					display_name = EXCLUDED.display_name,
					is_bot       = EXCLUDED.is_bot,
					refreshed_at = NOW()`,
				u.ID, name, u.IsBot, deps.MachineID,
			); e != nil {
				deps.Logger.Warn().Err(e).Str("uid", u.ID).Msg("refresh_slack_users: upsert slack_users failed")
				continue
			}
			// Backfill people.display_name (used for author chips / scoring) when blank.
			if _, e := deps.DB.Exec(ctx, `
				UPDATE graph.people SET display_name = $2
				WHERE slack_user_id = $1 AND COALESCE(display_name, '') = ''`,
				u.ID, name,
			); e != nil {
				deps.Logger.Warn().Err(e).Str("uid", u.ID).Msg("refresh_slack_users: update people failed")
			}
			// Fill the person's email (the key that merges them with BambooHR/Jira).
			// Guard against the UNIQUE(email) constraint: only set if free.
			if email := strings.ToLower(strings.TrimSpace(u.Profile.Email)); email != "" {
				if _, e := deps.DB.Exec(ctx, `
					UPDATE graph.people SET email = $2
					WHERE slack_user_id = $1 AND email IS NULL
					  AND NOT EXISTS (SELECT 1 FROM graph.people WHERE email = $2)`,
					u.ID, email,
				); e != nil {
					deps.Logger.Warn().Err(e).Str("uid", u.ID).Msg("refresh_slack_users: update email failed")
				}
			}
		}

		deps.Logger.Info().Int("count", len(users)).Msg("refresh_slack_users: done")
		return nil
	}
}

// slackUser is the relevant slice of a Slack users.list member object.
type slackUser struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	IsBot   bool   `json:"is_bot"`
	Deleted bool   `json:"deleted"`
	Profile struct {
		DisplayName string `json:"display_name"`
		RealName    string `json:"real_name"`
		Email       string `json:"email"` // requires users:read.email scope; "" otherwise
	} `json:"profile"`
}

// slackBestName prefers profile.display_name, then real_name, then handle.
func slackBestName(u slackUser) string {
	if u.Profile.DisplayName != "" {
		return u.Profile.DisplayName
	}
	if u.Profile.RealName != "" {
		return u.Profile.RealName
	}
	return u.Name
}

// fetchSlackUsers calls users.list with cursor pagination and returns all members.
func fetchSlackUsers(ctx context.Context, token string) ([]slackUser, error) {
	client := &http.Client{Timeout: 60 * time.Second}
	var out []slackUser
	cursor := ""
	for page := 0; page < 50; page++ { // hard cap: 50 pages × 200 = 10k users
		u := "https://slack.com/api/users.list?limit=200"
		if cursor != "" {
			u += "&cursor=" + url.QueryEscape(cursor)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, fmt.Errorf("%w: refresh_slack_users: build request: %v", jobs.ErrFatal, err)
		}
		req.Header.Set("Authorization", "Bearer "+token)

		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("%w: refresh_slack_users: http: %v", jobs.ErrTransient, err)
		}
		body, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("%w: refresh_slack_users: read: %v", jobs.ErrTransient, readErr)
		}
		if resp.StatusCode == 429 || resp.StatusCode >= 500 {
			return nil, fmt.Errorf("%w: refresh_slack_users: HTTP %d", jobs.ErrTransient, resp.StatusCode)
		}
		if resp.StatusCode >= 400 {
			return nil, fmt.Errorf("%w: refresh_slack_users: HTTP %d", jobs.ErrFatal, resp.StatusCode)
		}

		var apiResp struct {
			OK      bool        `json:"ok"`
			Members []slackUser `json:"members"`
			Error   string      `json:"error"`
			Meta    struct {
				NextCursor string `json:"next_cursor"`
			} `json:"response_metadata"`
		}
		if err := json.Unmarshal(body, &apiResp); err != nil {
			return nil, fmt.Errorf("%w: refresh_slack_users: parse: %v", jobs.ErrTransient, err)
		}
		if !apiResp.OK {
			return nil, fmt.Errorf("%w: refresh_slack_users: slack API error: %s", jobs.ErrTransient, apiResp.Error)
		}
		out = append(out, apiResp.Members...)
		cursor = apiResp.Meta.NextCursor
		if cursor == "" {
			break
		}
	}
	return out, nil
}
