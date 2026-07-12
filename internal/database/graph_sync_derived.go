package database

// Pull-only sync for derived graph tables: thread_summaries, slack_users,
// slack_channels. The cloud VPS is the only writer; local instances pull with
// a timestamp cursor and an overlap window, and import via timestamp-guarded
// upserts so re-pulls are idempotent.
// ponytail: pull-only by design - if a second graph writer ever appears,
// promote these tables into the sync_id push/pull rotation like graph.nodes.

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// SyncableThreadSummary is a graph.thread_summaries row for sync transport.
type SyncableThreadSummary struct {
	ChannelID  string          `json:"channel_id"`
	ThreadTS   string          `json:"thread_ts"`
	Signature  string          `json:"signature"`
	Summary    string          `json:"summary"`
	Overview   *string         `json:"overview,omitempty"`
	Highlights json.RawMessage `json:"highlights,omitempty"`
	UpdatedAt  time.Time       `json:"updated_at"`
}

// SyncableSlackUser is a graph.slack_users row for sync transport.
type SyncableSlackUser struct {
	SlackUserID string    `json:"slack_user_id"`
	DisplayName string    `json:"display_name"`
	RealName    *string   `json:"real_name,omitempty"`
	IsBot       bool      `json:"is_bot"`
	RefreshedAt time.Time `json:"refreshed_at"`
	MachineID   string    `json:"machine_id"`
}

// SyncableSlackChannel is a graph.slack_channels row for sync transport.
type SyncableSlackChannel struct {
	SlackChannelID string    `json:"slack_channel_id"`
	Name           string    `json:"name"`
	RefreshedAt    time.Time `json:"refreshed_at"`
	MachineID      string    `json:"machine_id"`
}

// GetThreadSummariesSince returns rows updated after the cursor.
func (db *DB) GetThreadSummariesSince(ctx context.Context, after time.Time, limit int) ([]SyncableThreadSummary, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT channel_id, thread_ts, signature, summary, overview, highlights, updated_at
		FROM graph.thread_summaries
		WHERE updated_at > $1
		ORDER BY updated_at ASC, channel_id, thread_ts
		LIMIT $2`, after, limit)
	if err != nil {
		return nil, fmt.Errorf("get thread_summaries since: %w", err)
	}
	defer rows.Close()
	var out []SyncableThreadSummary
	for rows.Next() {
		var ts SyncableThreadSummary
		if err := rows.Scan(&ts.ChannelID, &ts.ThreadTS, &ts.Signature, &ts.Summary,
			&ts.Overview, &ts.Highlights, &ts.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan thread_summary: %w", err)
		}
		out = append(out, ts)
	}
	return out, rows.Err()
}

// UpsertThreadSummaryFromSync imports a pulled row; the newer updated_at wins.
func (db *DB) UpsertThreadSummaryFromSync(ctx context.Context, ts *SyncableThreadSummary) error {
	_, err := db.Pool.Exec(ctx, `
		INSERT INTO graph.thread_summaries
			(channel_id, thread_ts, signature, summary, overview, highlights, updated_at)
		VALUES ($1,$2,$3,$4,COALESCE($5, ''),COALESCE($6, '[]'::jsonb),$7)
		ON CONFLICT (channel_id, thread_ts) DO UPDATE SET
			signature  = EXCLUDED.signature,
			summary    = EXCLUDED.summary,
			overview   = EXCLUDED.overview,
			highlights = EXCLUDED.highlights,
			updated_at = EXCLUDED.updated_at
		WHERE graph.thread_summaries.updated_at < EXCLUDED.updated_at`,
		ts.ChannelID, ts.ThreadTS, ts.Signature, ts.Summary, ts.Overview, ts.Highlights, ts.UpdatedAt,
	)
	return err
}

// GetSlackUsersSince returns rows refreshed after the cursor.
func (db *DB) GetSlackUsersSince(ctx context.Context, after time.Time, limit int) ([]SyncableSlackUser, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT slack_user_id, display_name, real_name, is_bot, refreshed_at, machine_id
		FROM graph.slack_users
		WHERE refreshed_at > $1
		ORDER BY refreshed_at ASC, slack_user_id
		LIMIT $2`, after, limit)
	if err != nil {
		return nil, fmt.Errorf("get slack_users since: %w", err)
	}
	defer rows.Close()
	var out []SyncableSlackUser
	for rows.Next() {
		var u SyncableSlackUser
		if err := rows.Scan(&u.SlackUserID, &u.DisplayName, &u.RealName, &u.IsBot,
			&u.RefreshedAt, &u.MachineID); err != nil {
			return nil, fmt.Errorf("scan slack_user: %w", err)
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// UpsertSlackUserFromSync imports a pulled row; the newer refreshed_at wins.
func (db *DB) UpsertSlackUserFromSync(ctx context.Context, u *SyncableSlackUser) error {
	_, err := db.Pool.Exec(ctx, `
		INSERT INTO graph.slack_users
			(slack_user_id, display_name, real_name, is_bot, refreshed_at, machine_id)
		VALUES ($1,$2,COALESCE($3, ''),$4,$5,$6)
		ON CONFLICT (slack_user_id) DO UPDATE SET
			display_name = EXCLUDED.display_name,
			real_name    = EXCLUDED.real_name,
			is_bot       = EXCLUDED.is_bot,
			refreshed_at = EXCLUDED.refreshed_at,
			machine_id   = EXCLUDED.machine_id
		WHERE graph.slack_users.refreshed_at < EXCLUDED.refreshed_at`,
		u.SlackUserID, u.DisplayName, u.RealName, u.IsBot, u.RefreshedAt, u.MachineID,
	)
	return err
}

// GetSlackChannelsSince returns rows refreshed after the cursor.
func (db *DB) GetSlackChannelsSince(ctx context.Context, after time.Time, limit int) ([]SyncableSlackChannel, error) {
	rows, err := db.Pool.Query(ctx, `
		SELECT slack_channel_id, name, refreshed_at, machine_id
		FROM graph.slack_channels
		WHERE refreshed_at > $1
		ORDER BY refreshed_at ASC, slack_channel_id
		LIMIT $2`, after, limit)
	if err != nil {
		return nil, fmt.Errorf("get slack_channels since: %w", err)
	}
	defer rows.Close()
	var out []SyncableSlackChannel
	for rows.Next() {
		var c SyncableSlackChannel
		if err := rows.Scan(&c.SlackChannelID, &c.Name, &c.RefreshedAt, &c.MachineID); err != nil {
			return nil, fmt.Errorf("scan slack_channel: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// UpsertSlackChannelFromSync imports a pulled row; the newer refreshed_at wins.
func (db *DB) UpsertSlackChannelFromSync(ctx context.Context, c *SyncableSlackChannel) error {
	_, err := db.Pool.Exec(ctx, `
		INSERT INTO graph.slack_channels
			(slack_channel_id, name, refreshed_at, machine_id)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (slack_channel_id) DO UPDATE SET
			name         = EXCLUDED.name,
			refreshed_at = EXCLUDED.refreshed_at,
			machine_id   = EXCLUDED.machine_id
		WHERE graph.slack_channels.refreshed_at < EXCLUDED.refreshed_at`,
		c.SlackChannelID, c.Name, c.RefreshedAt, c.MachineID,
	)
	return err
}
