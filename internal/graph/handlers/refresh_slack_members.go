package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/agent-mem/agent-mem/internal/graph/jobs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	slackMembersIntervalKey    = "graph.slack_members.interval_minutes"
	slackMembersLastAttemptKey = "graph.slack_members.last_attempt_at"
	slackMembersLockSQL        = `SELECT pg_try_advisory_lock(hashtext('refresh_slack_members'))`
	slackMembersUnlockSQL      = `SELECT pg_advisory_unlock(hashtext('refresh_slack_members'))`
)

// A single pinned session owns the pass lock and every write. Readers accept
// channel grants for 24 hours plus their existing five-minute cache grace.
func NewRefreshSlackMembersHandler(deps Deps) jobs.Entry {
	return jobs.Entry{Handler: refreshSlackMembersHandler(deps), Systems: []string{"slack"}, PoolSize: 1, Lease: 600 * time.Second, Heartbeat: true, MaxRuntime: 30 * time.Minute}
}
func refreshSlackMembersHandler(deps Deps) jobs.Handler {
	return refreshSlackMembersWithClient(deps, slackMembersClient{baseURL: "https://slack.com/api", http: &http.Client{Timeout: 60 * time.Second}, wait: waitSlackMembers})
}

type slackMembersClient struct {
	baseURL string
	http    *http.Client
	wait    func(context.Context, time.Duration) error
}
type slackMembersOutcome uint8

const (
	membersComplete slackMembersOutcome = iota
	membersInaccessible
	membersUnknown
)

func readSlackMembersInterval(ctx context.Context, db jobs.DB) (int, error) {
	var raw string
	err := db.QueryRow(ctx, `SELECT value FROM settings WHERE key=$1`, slackMembersIntervalKey).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return 60, nil
	}
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 60, nil
	}
	return min(max(n, 15), 480), nil
}
func slackMembersDue(ctx context.Context, db jobs.DB, now time.Time) (bool, error) {
	interval, err := readSlackMembersInterval(ctx, db)
	if err != nil {
		return false, err
	}
	var raw string
	err = db.QueryRow(ctx, `SELECT value FROM settings WHERE key=$1`, slackMembersLastAttemptKey).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	last, err := time.Parse(time.RFC3339Nano, raw)
	return err != nil || now.Sub(last) >= time.Duration(interval)*time.Minute, nil
}

func releaseSlackMembersConn(conn *pgxpool.Conn) {
	defer conn.Release()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	var unlocked bool
	err := conn.QueryRow(ctx, slackMembersUnlockSQL).Scan(&unlocked)
	cancel()
	if err != nil || !unlocked {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = conn.Conn().Close(ctx)
	}
}

func refreshSlackMembersWithClient(deps Deps, client slackMembersClient) jobs.Handler {
	return func(ctx context.Context, payload []byte) (result error) {
		var p struct {
			Force bool `json:"force"`
		}
		if len(payload) > 0 {
			if err := json.Unmarshal(payload, &p); err != nil {
				return fmt.Errorf("%w: refresh_slack_members payload: %v", jobs.ErrFatal, err)
			}
		}
		conn, err := deps.DB.Acquire(ctx)
		if err != nil {
			return fmt.Errorf("%w: acquire: %v", jobs.ErrTransient, err)
		}
		var locked bool
		if err := conn.QueryRow(ctx, slackMembersLockSQL).Scan(&locked); err != nil {
			// A canceled query may have acquired the session lock before losing its response.
			releaseSlackMembersConn(conn)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("%w: lock: %v", jobs.ErrTransient, err)
		}
		if !locked {
			conn.Release()
			if p.Force {
				return fmt.Errorf("%w: refresh_slack_members: pass already running", jobs.ErrTransient)
			}
			return nil
		}
		defer releaseSlackMembersConn(conn)
		due, err := slackMembersDue(ctx, conn, time.Now())
		if err != nil {
			return fmt.Errorf("%w: due: %v", jobs.ErrTransient, err)
		}
		if !p.Force && !due {
			return nil
		}
		if deps.SlackBotToken == "" {
			return fmt.Errorf("%w: refresh_slack_members: Slack bot token not set", jobs.ErrFatal)
		}
		if err := putSetting(ctx, conn, slackMembersLastAttemptKey, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return fmt.Errorf("%w: last attempt: %v", jobs.ErrTransient, err)
		}
		complete, inaccessible, unknown, mapped, unmapped := 0, 0, 0, 0, 0
		allKnown := false
		defer func() {
			// Expiry is housekeeping, including aborted passes; ACL enforces freshness
			// even if the DB/session is unavailable here. Cancellation cannot skip it.
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var expired int
			err := conn.QueryRow(cleanupCtx, `WITH expired AS (
    DELETE FROM graph.member_scopes WHERE (scope LIKE 'slack:C%' OR scope LIKE 'slack:G%')
    AND refreshed_at < now()-interval '24 hours' RETURNING scope)
    SELECT count(DISTINCT scope) FROM expired`).Scan(&expired)
			if err != nil {
				deps.Logger.Error().Err(err).Msg("refresh_slack_members: expiry failed")
				if result == nil {
					result = fmt.Errorf("%w: expiry: %v", jobs.ErrTransient, err)
				}
			}
			if allKnown && result == nil {
				if _, err := conn.Exec(cleanupCtx, `DELETE FROM graph.member_scopes m
     WHERE (m.scope LIKE 'slack:C%' OR m.scope LIKE 'slack:G%')
     AND NOT EXISTS (SELECT 1 FROM graph.nodes n WHERE n.scope=m.scope AND n.deleted_at IS NULL)`); err != nil {
					result = fmt.Errorf("%w: stale cleanup: %v", jobs.ErrTransient, err)
				}
			}
			log := deps.Logger.Info()
			if result != nil {
				log = deps.Logger.Warn()
			}
			log.Err(result).Int("complete", complete).Int("inaccessible", inaccessible).Int("unknown", unknown).Int("expired", expired).Int("mapped_members", mapped).Int("unmapped_members", unmapped).Msg("refresh_slack_members: pass summary")
			// Scheduled runs recover only on the next due tick. Manual force retains
			// queue backoff; canceled dispatchers let the lease expire normally.
			if !p.Force && errors.Is(result, jobs.ErrTransient) && ctx.Err() == nil {
				result = nil
			}
		}()
		rows, err := conn.Query(ctx, `SELECT n.scope FROM
    (SELECT DISTINCT scope FROM graph.nodes WHERE deleted_at IS NULL
     AND (scope LIKE 'slack:C%' OR scope LIKE 'slack:G%')) n
    LEFT JOIN graph.member_scopes m ON m.scope=n.scope
    GROUP BY n.scope ORDER BY min(m.refreshed_at) NULLS FIRST, n.scope`)
		if err != nil {
			return fmt.Errorf("%w: channels: %v", jobs.ErrTransient, err)
		}
		var scopes []string
		for rows.Next() {
			var scope string
			if err := rows.Scan(&scope); err != nil {
				rows.Close()
				return fmt.Errorf("%w: channel: %v", jobs.ErrTransient, err)
			}
			scopes = append(scopes, scope)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return fmt.Errorf("%w: channels: %v", jobs.ErrTransient, err)
		}
		for _, scope := range scopes {
			ids, outcome, err := client.members(ctx, deps.SlackBotToken, strings.TrimPrefix(scope, "slack:"))
			if err != nil {
				return err
			}
			switch outcome {
			case membersUnknown:
				unknown++
				continue
			case membersInaccessible:
				if _, err := conn.Exec(ctx, `DELETE FROM graph.member_scopes WHERE scope=$1`, scope); err != nil {
					return fmt.Errorf("%w: inaccessible revoke: %v", jobs.ErrTransient, err)
				}
				inaccessible++
			case membersComplete:
				tx, err := conn.Begin(ctx)
				if err != nil {
					return fmt.Errorf("%w: begin: %v", jobs.ErrTransient, err)
				}
				count, err := replaceSlackMembers(ctx, tx, scope, ids)
				if err != nil {
					_ = tx.Rollback(context.Background())
					return fmt.Errorf("%w: replace %s: %v", jobs.ErrTransient, scope, err)
				}
				if err := tx.Commit(ctx); err != nil {
					return fmt.Errorf("%w: commit %s: %v", jobs.ErrTransient, scope, err)
				}
				complete++
				mapped += count
				var missing int
				if err := conn.QueryRow(ctx, `SELECT count(*) FROM unnest($1::text[]) AS ids(id)
    WHERE NOT EXISTS (SELECT 1 FROM graph.people p WHERE p.slack_user_id=ids.id
    AND p.eeid IS NOT NULL AND p.merged_into IS NULL)`, ids).Scan(&missing); err != nil {
					return fmt.Errorf("%w: unmapped members: %v", jobs.ErrTransient, err)
				}
				unmapped += missing
			}
		}
		allKnown = unknown == 0
		return nil
	}
}

func replaceSlackMembers(ctx context.Context, tx pgx.Tx, scope string, ids []string) (int, error) {
	if _, err := tx.Exec(ctx, `DELETE FROM graph.member_scopes WHERE scope=$1`, scope); err != nil {
		return 0, err
	}
	if ids == nil {
		ids = []string{}
	}
	tag, err := tx.Exec(ctx, `INSERT INTO graph.member_scopes(eeid,scope,refreshed_at)
  SELECT DISTINCT eeid,$2,now() FROM graph.people WHERE slack_user_id=ANY($1) AND eeid IS NOT NULL AND merged_into IS NULL
  ON CONFLICT(eeid,scope) DO UPDATE SET refreshed_at=now()`, ids, scope)
	return int(tag.RowsAffected()), err
}

func waitSlackMembers(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
func slackMembersRetryAfter(raw string) time.Duration {
	if seconds, err := strconv.Atoi(raw); err == nil {
		return time.Duration(min(max(seconds, 0), 60)) * time.Second
	}
	if at, err := http.ParseTime(raw); err == nil {
		return min(max(time.Until(at), 0), 60*time.Second)
	}
	return time.Second
}
func (client slackMembersClient) members(ctx context.Context, token, channel string) ([]string, slackMembersOutcome, error) {
	cursor := ""
	seen := map[string]bool{}
	unique := map[string]bool{}
	var ids []string
	for range 20 {
		values := url.Values{"channel": {channel}, "limit": {"1000"}}
		if cursor != "" {
			values.Set("cursor", cursor)
		}
		var page struct {
			OK       bool     `json:"ok"`
			Error    string   `json:"error"`
			Members  []string `json:"members"`
			Metadata struct {
				Cursor string `json:"next_cursor"`
			} `json:"response_metadata"`
		}
		for attempt := range 2 {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, client.baseURL+"/conversations.members?"+values.Encode(), nil)
			if err != nil {
				return nil, membersUnknown, fmt.Errorf("%w: request: %v", jobs.ErrFatal, err)
			}
			req.Header.Set("Authorization", "Bearer "+token)
			resp, err := client.http.Do(req)
			transient := err != nil
			retryAfter := time.Second
			if err == nil {
				transient = resp.StatusCode == 429 || resp.StatusCode >= 500
				retryAfter = slackMembersRetryAfter(resp.Header.Get("Retry-After"))
				if !transient {
					err = json.NewDecoder(resp.Body).Decode(&page)
				}
				_ = resp.Body.Close()
				if !transient && err != nil {
					return nil, membersUnknown, nil
				}
				if !transient && !page.OK && page.Error == "ratelimited" {
					transient = true
				}
			}
			if ctx.Err() != nil {
				return nil, membersUnknown, ctx.Err()
			}
			if transient {
				if attempt == 1 {
					return nil, membersUnknown, fmt.Errorf("%w: conversations.members %s: %v %s", jobs.ErrTransient, channel, err, page.Error)
				}
				if err := client.wait(ctx, retryAfter); err != nil {
					return nil, membersUnknown, err
				}
				continue
			}
			if !page.OK {
				switch page.Error {
				case "not_in_channel", "channel_not_found":
					return nil, membersInaccessible, nil
				case "invalid_auth", "not_authed", "token_revoked", "account_inactive", "missing_scope":
					return nil, membersUnknown, fmt.Errorf("%w: conversations.members: %s", jobs.ErrFatal, page.Error)
				default:
					return nil, membersUnknown, nil
				}
			}
			break
		}
		for _, id := range page.Members {
			if !unique[id] {
				unique[id] = true
				ids = append(ids, id)
			}
		}
		cursor = page.Metadata.Cursor
		if cursor == "" {
			return ids, membersComplete, nil
		}
		if seen[cursor] {
			return nil, membersUnknown, nil
		}
		seen[cursor] = true
	}
	return nil, membersUnknown, nil
}
