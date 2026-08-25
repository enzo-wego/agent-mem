package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
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
		token := deps.SlackBotToken
		if token == "" {
			return fmt.Errorf("%w: refresh_slack_channels: AGENT_MEM_SLACK_BOT_TOKEN not set", jobs.ErrFatal)
		}

		// The full-list pass. Its error is captured, not returned early: the
		// targeted backfill below must still run when the list pass is
		// rate-limited, so a stale list stops producing nameless channels.
		// The list error (and its retry semantics) is preserved as the job's
		// result — the backfill is a patch, not a replacement.
		listErr := refreshChannelsFromList(ctx, deps, token)
		if err := backfillUnknownChannelNames(ctx, deps, token); err != nil {
			if listErr == nil {
				return err
			}
			deps.Logger.Warn().Err(err).Msg("refresh_slack_channels: backfill error (list pass also failed)")
		}
		return listErr
	}
}

// slackChannel is the relevant slice of a Slack conversations.list channel object.
type slackChannel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// refreshChannelsFromList runs the conversations.list pass and upserts every
// named channel. Per-row upsert failures are logged, not fatal.
func refreshChannelsFromList(ctx context.Context, deps Deps, token string) error {
	channels, err := fetchSlackChannels(ctx, token)
	if err != nil {
		return err
	}

	for _, c := range channels {
		if c.Name == "" {
			continue
		}
		upsertSlackChannel(ctx, deps, c.ID, c.Name)
	}

	deps.Logger.Info().Int("count", len(channels)).Msg("refresh_slack_channels: done")
	return nil
}

// upsertSlackChannel writes one channel row. Failures are logged and skipped:
// one bad row must not sink the rest of the refresh.
func upsertSlackChannel(ctx context.Context, deps Deps, id, name string) {
	if _, err := deps.DB.Exec(ctx, `
		INSERT INTO graph.slack_channels (slack_channel_id, name, refreshed_at, machine_id)
		VALUES ($1, $2, NOW(), $3)
		ON CONFLICT (slack_channel_id) DO UPDATE SET
			name         = EXCLUDED.name,
			refreshed_at = NOW()`,
		id, name, deps.MachineID,
	); err != nil {
		deps.Logger.Warn().Err(err).Str("cid", id).Msg("refresh_slack_channels: upsert failed")
	}
}

// channelBackfillBatchCap bounds how many unknown channel ids one run will
// resolve with conversations.info. Small on purpose: the backfill exists to
// work around rate limiting, so it must never become a bulk crawler itself.
const channelBackfillBatchCap = 20

// errSlackRateLimited marks a conversations.info call that got HTTP 429 (or
// Slack's "ratelimited" error). The backfill loop stops at the first one.
var errSlackRateLimited = errors.New("slack rate limited")

// backfillUnknownChannelNames finds channel ids that have nodes but no
// graph.slack_channels row — channels first seen after the last successful
// full conversations.list (e.g. because every list run since has been
// rate-limited) — and resolves each with a single targeted conversations.info
// call. This is deliberately not a fix for the rate limiting itself
// (agent-mem-q8tm); it keeps the UI free of bare ids while that is unsolved.
func backfillUnknownChannelNames(ctx context.Context, deps Deps, token string) error {
	rows, err := deps.DB.Query(ctx, `
		SELECT DISTINCT replace(n.scope,'slack:','')
		FROM graph.nodes n
		LEFT JOIN graph.slack_channels sc
		  ON sc.slack_channel_id = replace(n.scope,'slack:','')
		WHERE n.scope LIKE 'slack:%' AND n.scope NOT LIKE 'slack:D%'
		  AND n.deleted_at IS NULL AND sc.slack_channel_id IS NULL
		ORDER BY 1`)
	if err != nil {
		return fmt.Errorf("refresh_slack_channels: backfill query: %w", err)
	}
	var unknown []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("refresh_slack_channels: backfill scan: %w", err)
		}
		unknown = append(unknown, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return fmt.Errorf("refresh_slack_channels: backfill rows: %w", err)
	}
	if len(unknown) == 0 {
		return nil
	}

	skipped := 0
	if len(unknown) > channelBackfillBatchCap {
		skipped = len(unknown) - channelBackfillBatchCap
		unknown = unknown[:channelBackfillBatchCap]
	}
	resolved, failed, rateLimited := 0, 0, false
	for _, id := range unknown {
		name, err := fetchSlackChannelInfo(ctx, token, id)
		if errors.Is(err, errSlackRateLimited) {
			rateLimited = true
			deps.Logger.Info().Str("cid", id).Msg("refresh_slack_channels: backfill rate-limited, stopping batch")
			break
		}
		if err != nil {
			// channel_not_found / missing_scope (the bot is not in that
			// private channel) are expected for invisible channels: log the
			// literal Slack error and move on — one channel cannot block
			// the rest of the batch.
			failed++
			deps.Logger.Info().Str("cid", id).Err(err).Msg("refresh_slack_channels: backfill skipped channel")
			continue
		}
		if name == "" {
			failed++
			continue
		}
		upsertSlackChannel(ctx, deps, id, name)
		resolved++
	}

	// skipped must be visible in the log: a silent cap reads as "all done"
	// when the backlog is bigger than one batch.
	deps.Logger.Info().
		Int("resolved", resolved).
		Int("failed", failed).
		Int("skipped_by_cap", skipped).
		Bool("rate_limited", rateLimited).
		Msg("refresh_slack_channels: name backfill done")
	return nil
}

// fetchSlackChannelInfo resolves one channel's name via conversations.info —
// a single targeted call, unlike the full conversations.list crawl. It returns
// errSlackRateLimited on HTTP 429 or Slack's "ratelimited" error; other !ok
// responses (channel_not_found, missing_scope, …) come back as an error whose
// message carries the literal Slack error string.
func fetchSlackChannelInfo(ctx context.Context, token, channelID string) (string, error) {
	u := slackAPIBaseURL + "/api/conversations.info?channel=" + url.QueryEscape(channelID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := slackHTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("http: %w", err)
	}
	body, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if readErr != nil {
		return "", fmt.Errorf("read: %w", readErr)
	}
	if resp.StatusCode == 429 {
		return "", errSlackRateLimited
	}
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	var apiResp struct {
		OK      bool         `json:"ok"`
		Channel slackChannel `json:"channel"`
		Error   string       `json:"error"`
	}
	if err := json.Unmarshal(body, &apiResp); err != nil {
		return "", fmt.Errorf("parse: %w", err)
	}
	if !apiResp.OK {
		if apiResp.Error == "ratelimited" {
			return "", errSlackRateLimited
		}
		return "", fmt.Errorf("slack API error: %s", apiResp.Error)
	}
	return apiResp.Channel.Name, nil
}

// fetchSlackChannels calls conversations.list (public + private) with cursor
// pagination and returns all channels.
func fetchSlackChannels(ctx context.Context, token string) ([]slackChannel, error) {
	client := &http.Client{Timeout: 60 * time.Second}
	var out []slackChannel
	cursor := ""
	for range 50 { // hard cap: 50 pages × 1000 = 50k channels
		u := slackAPIBaseURL + "/api/conversations.list?limit=1000&exclude_archived=false&types=public_channel,private_channel"
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
