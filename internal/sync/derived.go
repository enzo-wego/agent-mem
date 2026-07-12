package sync

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/agent-mem/agent-mem/internal/database"
)

const (
	// derivedBatchLimit is deliberately larger than any derived table's bulk
	// refresh (nightly slack_users refresh touches ~2k rows at one timestamp).
	// ponytail: if a derived table ever updates >5000 rows in the same
	// instant, switch to keyset pagination on (timestamp, pk).
	derivedBatchLimit = 5000

	// derivedOverlap is re-pulled every cycle so rows sharing the cursor
	// timestamp can never be skipped. Imports are idempotent upserts.
	derivedOverlap = time.Hour
)

// DerivedPullResponse is the payload of GET /api/sync/pull_derived.
type DerivedPullResponse struct {
	ThreadSummaries []database.SyncableThreadSummary `json:"thread_summaries,omitempty"`
	SlackUsers      []database.SyncableSlackUser     `json:"slack_users,omitempty"`
	SlackChannels   []database.SyncableSlackChannel  `json:"slack_channels,omitempty"`
}

// pullDerived fetches derived-table rows updated since the stored cursors.
// One request per tick; a backlog larger than derivedBatchLimit converges
// over successive ticks.
func (e *Engine) pullDerived(ctx context.Context) error {
	tsCursor := e.getPullCursorTime(ctx, "derived.thread_summaries")
	suCursor := e.getPullCursorTime(ctx, "derived.slack_users")
	scCursor := e.getPullCursorTime(ctx, "derived.slack_channels")

	q := url.Values{}
	q.Set("machine_id", e.config.MachineID)
	q.Set("limit", fmt.Sprint(derivedBatchLimit))
	q.Set("ts_after", tsCursor.Add(-derivedOverlap).Format(time.RFC3339Nano))
	q.Set("su_after", suCursor.Add(-derivedOverlap).Format(time.RFC3339Nano))
	q.Set("sc_after", scCursor.Add(-derivedOverlap).Format(time.RFC3339Nano))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		e.config.SyncURL+"/api/sync/pull_derived?"+q.Encode(), nil)
	if err != nil {
		return fmt.Errorf("create pull_derived request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+e.config.APIKey)

	resp, err := e.client.Do(req)
	if err != nil {
		return fmt.Errorf("pull_derived: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		// Cloud not redeployed yet; skip quietly until it is.
		return nil
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("pull_derived returned %d", resp.StatusCode)
	}

	var pr DerivedPullResponse
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		return fmt.Errorf("decode pull_derived response: %w", err)
	}

	imported := 0
	for i := range pr.ThreadSummaries {
		if err := e.db.UpsertThreadSummaryFromSync(ctx, &pr.ThreadSummaries[i]); err == nil {
			imported++
		}
		if t := pr.ThreadSummaries[i].UpdatedAt; t.After(tsCursor) {
			tsCursor = t
		}
	}
	for i := range pr.SlackUsers {
		if err := e.db.UpsertSlackUserFromSync(ctx, &pr.SlackUsers[i]); err == nil {
			imported++
		}
		if t := pr.SlackUsers[i].RefreshedAt; t.After(suCursor) {
			suCursor = t
		}
	}
	for i := range pr.SlackChannels {
		if err := e.db.UpsertSlackChannelFromSync(ctx, &pr.SlackChannels[i]); err == nil {
			imported++
		}
		if t := pr.SlackChannels[i].RefreshedAt; t.After(scCursor) {
			scCursor = t
		}
	}

	e.setPullCursorTime(ctx, "derived.thread_summaries", tsCursor)
	e.setPullCursorTime(ctx, "derived.slack_users", suCursor)
	e.setPullCursorTime(ctx, "derived.slack_channels", scCursor)

	if imported > 0 {
		log.Info().Int("imported", imported).Msg("Derived sync pull complete")
	}
	return nil
}

// getPullCursorTime reads a timestamp cursor from settings (zero time if unset).
func (e *Engine) getPullCursorTime(ctx context.Context, key string) time.Time {
	var v string
	err := e.db.Pool.QueryRow(ctx, `SELECT value FROM settings WHERE key = $1`, "pull_cursor:"+key).Scan(&v)
	if err != nil {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return time.Time{}
	}
	return t
}

// setPullCursorTime stores a timestamp cursor in settings.
func (e *Engine) setPullCursorTime(ctx context.Context, key string, t time.Time) {
	if t.IsZero() {
		return
	}
	e.db.Pool.Exec(ctx, `
		INSERT INTO settings (key, value) VALUES ($1, $2)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value
	`, "pull_cursor:"+key, t.Format(time.RFC3339Nano))
}
