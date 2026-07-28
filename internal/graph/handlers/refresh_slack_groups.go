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
func NewRefreshSlackGroupsHandler(deps Deps) jobs.Entry {
	return jobs.Entry{
		Handler:  refreshSlackGroupsHandler(deps),
		Systems:  []string{"slack"},
		PoolSize: 1,
		Lease:    300 * time.Second,
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

		// Step 2: upsert each group. A per-group failure is logged and skipped, so the
		// total is checked afterwards — every upsert failing must not report success.
		upserted := 0
		for _, g := range groups {
			// member_user_ids is text[]: pass the slice so pgx encodes an array. Marshalling
			// it to JSON first made Postgres reject every row ("malformed array literal"),
			// which — combined with the skip-on-error below — let this job report done while
			// writing nothing, from its first run until 2026-07-28.
			members := g.Users
			if members == nil {
				members = []string{}
			}
			execErr := upsertSlackGroup(ctx, deps, g, members)
			if execErr != nil {
				deps.Logger.Warn().Err(execErr).Str("group_id", g.ID).Msg("refresh_slack_groups: upsert group failed")
				continue
			}
			upserted++

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

		// Slack returned groups but none could be stored: that is a failure, not a no-op.
		if len(groups) > 0 && upserted == 0 {
			return fmt.Errorf("%w: refresh_slack_groups: all %d group upserts failed", jobs.ErrTransient, len(groups))
		}
		deps.Logger.Info().Int("groups", len(groups)).Int("upserted", upserted).Msg("refresh_slack_groups: done")

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

// upsertSlackGroup writes one usergroup row. members must be a []string so pgx encodes
// graph.slack_groups.member_user_ids (text[]) as an array.
func upsertSlackGroup(ctx context.Context, deps Deps, g slackUserGroup, members []string) error {
	_, err := deps.DB.Exec(ctx, `
		INSERT INTO graph.slack_groups
			(id, handle, name, description, member_user_ids, user_count, refreshed_at, machine_id)
		VALUES ($1, $2, $3, $4, $5, $6, NOW(), $7)
		ON CONFLICT (id) DO UPDATE SET
			handle          = EXCLUDED.handle,
			name            = EXCLUDED.name,
			description     = EXCLUDED.description,
			member_user_ids = EXCLUDED.member_user_ids,
			user_count      = EXCLUDED.user_count,
			refreshed_at    = NOW(),
			machine_id      = EXCLUDED.machine_id`,
		g.ID, g.Handle, g.Name, g.Description, members, g.UserCount, deps.MachineID,
	)
	return err
}
