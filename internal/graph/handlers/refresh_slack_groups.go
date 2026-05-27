package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/agent-mem/agent-mem/internal/graph/jobs"
)

// NewRefreshSlackGroupsHandler returns a HandlerInfo for the "refresh_slack_groups" job type.
func NewRefreshSlackGroupsHandler(deps Deps) jobs.HandlerInfo {
	return jobs.HandlerInfo{
		Handler:  refreshSlackGroupsHandler(deps),
		Systems:  []string{"slack"},
		PoolSize: 1,
		Timeout:  300 * time.Second,
	}
}

func refreshSlackGroupsHandler(deps Deps) jobs.Handler {
	return func(ctx context.Context, payload []byte) error {
		token := os.Getenv("SLACK_BOT_TOKEN")
		if token == "" {
			return fmt.Errorf("%w: refresh_slack_groups: SLACK_BOT_TOKEN not set", jobs.ErrFatal)
		}

		// Step 1: call usergroups.list with include_users=true.
		groups, err := fetchSlackUserGroups(ctx, token)
		if err != nil {
			return err
		}

		// Step 2: upsert each group.
		for _, g := range groups {
			memberJSON, _ := json.Marshal(g.Users)
			_, execErr := deps.DB.Exec(ctx, `
				INSERT INTO graph.slack_groups
					(id, handle, name, description, member_user_ids, user_count, refreshed_at, machine_id)
				VALUES ($1, $2, $3, $4, $5, $6, NOW(), $7)
				ON CONFLICT (id) DO UPDATE SET
					handle         = EXCLUDED.handle,
					name           = EXCLUDED.name,
					description    = EXCLUDED.description,
					member_user_ids = EXCLUDED.member_user_ids,
					user_count     = EXCLUDED.user_count,
					refreshed_at   = NOW(),
					machine_id     = EXCLUDED.machine_id`,
				g.ID, g.Handle, g.Name, g.Description, memberJSON, g.UserCount, deps.MachineID,
			)
			if execErr != nil {
				deps.Logger.Warn().Err(execErr).Str("group_id", g.ID).Msg("refresh_slack_groups: upsert group failed")
				continue
			}

			// Step 3: auto-detect interest groups (*-geeks, *-ops).
			if isInterestGroup(g.Handle) {
				for _, uid := range g.Users {
					_, affErr := deps.DB.Exec(ctx, `
						INSERT INTO graph.user_affinity_config
							(slack_user_id, group_id, group_handle, updated_at)
						VALUES ($1, $2, $3, NOW())
						ON CONFLICT (slack_user_id, group_id) DO UPDATE SET
							group_handle = EXCLUDED.group_handle,
							updated_at   = NOW()`,
						uid, g.ID, g.Handle,
					)
					if affErr != nil {
						deps.Logger.Warn().Err(affErr).Str("uid", uid).Str("group_id", g.ID).
							Msg("refresh_slack_groups: upsert user_affinity_config failed")
					}
				}
			}
		}

		return nil
	}
}

// slackUserGroup is the relevant slice of the Slack API usergroup object.
type slackUserGroup struct {
	ID          string   `json:"id"`
	Handle      string   `json:"handle"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	UserCount   int      `json:"user_count"`
	Users       []string `json:"users"`
}

// fetchSlackUserGroups calls usergroups.list and returns all groups with members.
func fetchSlackUserGroups(ctx context.Context, token string) ([]slackUserGroup, error) {
	url := "https://slack.com/api/usergroups.list?include_users=true"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: refresh_slack_groups: build request: %v", jobs.ErrFatal, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: refresh_slack_groups: http: %v", jobs.ErrTransient, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("%w: refresh_slack_groups: read: %v", jobs.ErrTransient, err)
	}
	if resp.StatusCode == 429 || resp.StatusCode >= 500 {
		return nil, fmt.Errorf("%w: refresh_slack_groups: HTTP %d", jobs.ErrTransient, resp.StatusCode)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("%w: refresh_slack_groups: HTTP %d", jobs.ErrFatal, resp.StatusCode)
	}

	var apiResp struct {
		OK         bool             `json:"ok"`
		Usergroups []slackUserGroup `json:"usergroups"`
		Error      string           `json:"error"`
	}
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return nil, fmt.Errorf("%w: refresh_slack_groups: parse response: %v", jobs.ErrTransient, err)
	}
	if !apiResp.OK {
		return nil, fmt.Errorf("%w: refresh_slack_groups: slack API error: %s", jobs.ErrTransient, apiResp.Error)
	}

	return apiResp.Usergroups, nil
}

// isInterestGroup returns true for handles matching *-geeks or *-ops patterns.
func isInterestGroup(handle string) bool {
	return strings.HasSuffix(handle, "-geeks") || strings.HasSuffix(handle, "-ops")
}
