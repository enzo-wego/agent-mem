package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	"github.com/agent-mem/agent-mem/internal/graph/fetchers"
	"github.com/agent-mem/agent-mem/internal/graph/jobs"
)

// refresh_jira_updates keeps Jira nodes fresh: it asks Jira which issues changed
// since the cursor (minus a 20-minute overlap) and queues fetch_body for every
// graph node whose body_ts is behind Jira's `updated`. A ticker in the worker
// process owns the cadence (jiraUpdatesTick); the handler never reschedules.
//
// The handler always returns nil. Its outcome lives in settings:
// jira_updates_last_ok_at / jira_updates_last_error are the health signal the
// Sync page reads, not the job status.

const (
	jiraUpdatesKeyEnabled  = "jira_updates_enabled"
	jiraUpdatesKeyInterval = "jira_updates_interval_minutes"
	jiraUpdatesKeyCursor   = "jira_updates_cursor"
	jiraUpdatesKeyLastRun  = "jira_updates_last_run_at"
	jiraUpdatesKeyLastOK   = "jira_updates_last_ok_at"
	jiraUpdatesKeyLastErr  = "jira_updates_last_error"
	jiraUpdatesKeyQueued   = "jira_updates_last_queued"

	jiraUpdatesDefaultInterval = 15
	jiraUpdatesMinInterval     = 5
	jiraUpdatesMaxInterval     = 1440
	jiraUpdatesOverlapMinutes  = 20
	jiraUpdatesTickEvery       = 60 * time.Second
	jiraUpdatesHealthTimeout   = 5 * time.Second
)

// jiraUpdatesEnv is what the ticker needs to enqueue a run.
type jiraUpdatesEnv struct {
	MachineID string
	Runner    string // target_runner; "" -> "any"
}

// jiraUpdatesConfig is the user-editable part of the poll's settings.
type jiraUpdatesConfig struct {
	Enabled         bool
	IntervalMinutes int
}

// getSetting reads a settings value. ok is false when the key is absent.
func getSetting(ctx context.Context, db *pgxpool.Pool, key string) (value string, ok bool, err error) {
	err = db.QueryRow(ctx, `SELECT value FROM settings WHERE key=$1`, key).Scan(&value)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read setting %s: %w", key, err)
	}
	return value, true, nil
}

// putSetting upserts a settings value through any pgx executor (pool or tx).
func putSetting(ctx context.Context, db jobs.DB, key, value string) error {
	if _, err := db.Exec(ctx, `INSERT INTO settings(key,value) VALUES($1,$2)
		ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value`, key, value); err != nil {
		return fmt.Errorf("write setting %s: %w", key, err)
	}
	return nil
}

// readJiraUpdatesConfig returns the poll config. Enabled unless stored exactly
// "false"; interval 15 when missing or unparsable, clamped to 5–1440.
func readJiraUpdatesConfig(ctx context.Context, db *pgxpool.Pool) (jiraUpdatesConfig, error) {
	enabled, _, err := getSetting(ctx, db, jiraUpdatesKeyEnabled)
	if err != nil {
		return jiraUpdatesConfig{}, err
	}
	raw, _, err := getSetting(ctx, db, jiraUpdatesKeyInterval)
	if err != nil {
		return jiraUpdatesConfig{}, err
	}
	interval, perr := strconv.Atoi(strings.TrimSpace(raw))
	if perr != nil {
		interval = jiraUpdatesDefaultInterval
	}
	interval = min(max(interval, jiraUpdatesMinInterval), jiraUpdatesMaxInterval)
	return jiraUpdatesConfig{Enabled: enabled != "false", IntervalMinutes: interval}, nil
}

// jiraUpdatesTick does one ticker step: when the poll is enabled and the last
// run is at least one interval old (or unknown), enqueue refresh_jira_updates
// unless one is already queued or running.
func jiraUpdatesTick(ctx context.Context, db *pgxpool.Pool, cfg jiraUpdatesEnv, now time.Time) error {
	conf, err := readJiraUpdatesConfig(ctx, db)
	if err != nil {
		return err
	}
	if !conf.Enabled {
		return nil
	}
	last, ok, err := getSetting(ctx, db, jiraUpdatesKeyLastRun)
	if err != nil {
		return err
	}
	if ok {
		if t, perr := time.Parse(time.RFC3339, last); perr == nil && now.Sub(t) < time.Duration(conf.IntervalMinutes)*time.Minute {
			return nil
		}
	}
	runner := cfg.Runner
	if runner == "" {
		runner = "any"
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO graph.jobs (type, payload, priority, machine_id, target_runner)
		SELECT 'refresh_jira_updates', '{}', 5, $1, $2
		WHERE NOT EXISTS (SELECT 1 FROM graph.jobs
		                  WHERE type='refresh_jira_updates' AND status IN ('queued','running'))`,
		cfg.MachineID, runner); err != nil {
		return fmt.Errorf("enqueue refresh_jira_updates: %w", err)
	}
	return nil
}

// runJiraUpdatesTicker calls jiraUpdatesTick every `every` until ctx is done.
func runJiraUpdatesTicker(ctx context.Context, db *pgxpool.Pool, cfg jiraUpdatesEnv, every time.Duration, log zerolog.Logger) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := jiraUpdatesTick(ctx, db, cfg, time.Now()); err != nil && ctx.Err() == nil {
				log.Error().Err(err).Msg("jira updates ticker: tick failed")
			}
		}
	}
}

// RunJiraUpdatesTicker drives the refresh_jira_updates cadence once a minute
// until ctx is cancelled. The worker starts it next to the job manager.
func RunJiraUpdatesTicker(ctx context.Context, db *pgxpool.Pool, machineID, runner string, log zerolog.Logger) {
	runJiraUpdatesTicker(ctx, db, jiraUpdatesEnv{MachineID: machineID, Runner: runner}, jiraUpdatesTickEvery, log)
}

// NewRefreshJiraUpdatesHandler returns the job entry for "refresh_jira_updates".
func NewRefreshJiraUpdatesHandler(deps Deps) jobs.Entry {
	return jobs.Entry{
		Handler:  refreshJiraUpdatesHandler(deps),
		Systems:  []string{"jira"},
		PoolSize: 1,
		Lease:    300 * time.Second,
	}
}

// jiraUpdatedResp is the shape of a search/jql page requested with fields: updated.
type jiraUpdatedResp struct {
	Issues []struct {
		Key    string `json:"key"`
		Fields struct {
			Updated string `json:"updated"`
		} `json:"fields"`
	} `json:"issues"`
	NextPageToken string `json:"nextPageToken"`
	IsLast        bool   `json:"isLast"`
}

func refreshJiraUpdatesHandler(deps Deps) jobs.Handler {
	return func(ctx context.Context, _ []byte) error {
		start := time.Now().UTC().Truncate(time.Second)
		health := func(write func(context.Context) error) bool {
			hctx, cancel := context.WithTimeout(context.Background(), jiraUpdatesHealthTimeout)
			defer cancel()
			if err := write(hctx); err != nil {
				deps.Logger.Error().Err(err).Msg("refresh_jira_updates: health write failed")
				return false
			}
			return true
		}
		fail := func(msg string) {
			deps.Logger.Warn().Str("error", msg).Msg("refresh_jira_updates: run failed")
			health(func(c context.Context) error { return putSetting(c, deps.DB, jiraUpdatesKeyLastErr, msg) })
		}

		// If last_run_at can't be written the DB is down: stop; the ticker retries.
		if !health(func(c context.Context) error {
			return putSetting(c, deps.DB, jiraUpdatesKeyLastRun, start.Format(time.RFC3339))
		}) {
			return nil
		}

		baseURL := strings.TrimRight(os.Getenv("AGENT_MEM_JIRA_BASE_URL"), "/")
		email := os.Getenv("AGENT_MEM_JIRA_EMAIL")
		token := os.Getenv("AGENT_MEM_JIRA_TOKEN")
		if baseURL == "" || email == "" || token == "" {
			fail("jira credentials not set")
			return nil
		}

		raw, ok, err := getSetting(ctx, deps.DB, jiraUpdatesKeyCursor)
		if err != nil {
			fail(err.Error())
			return nil
		}
		if !ok {
			fail("cursor missing")
			return nil
		}
		cursor, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			fail("cursor unparsable: " + raw)
			return nil
		}
		if cursor.After(start) {
			cursor = start
		}
		minutes := int(math.Ceil(start.Sub(cursor).Minutes())) + jiraUpdatesOverlapMinutes
		jql := fmt.Sprintf("updated >= -%dm ORDER BY updated ASC", minutes)

		queued, err := queueStaleJiraNodes(ctx, deps, baseURL, email, token, jql)
		if err != nil {
			fail(err.Error())
			return nil
		}

		health(func(c context.Context) error {
			tx, err := deps.DB.Begin(c)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback(c) }()
			for _, kv := range [][2]string{
				{jiraUpdatesKeyCursor, start.Format(time.RFC3339)},
				{jiraUpdatesKeyLastOK, start.Format(time.RFC3339)},
				{jiraUpdatesKeyQueued, strconv.Itoa(queued)},
				{jiraUpdatesKeyLastErr, ""},
			} {
				if err := putSetting(c, tx, kv[0], kv[1]); err != nil {
					return err
				}
			}
			return tx.Commit(c)
		})
		deps.Logger.Info().Int("queued", queued).Int("window_minutes", minutes).Msg("refresh_jira_updates: done")
		return nil
	}
}

// queueStaleJiraNodes pages the JQL search and queues fetch_body for every
// matching node whose body_ts is behind Jira. Returns the number queued.
func queueStaleJiraNodes(ctx context.Context, deps Deps, baseURL, email, token, jql string) (int, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	var keys []string
	var updated []time.Time
	pageToken := ""
	for {
		body := map[string]any{"jql": jql, "fields": []string{"updated"}, "maxResults": 100}
		if pageToken != "" {
			body["nextPageToken"] = pageToken
		}
		raw, err := jiraSearchRaw(ctx, client, baseURL, email, token, body)
		if err != nil {
			return 0, err
		}
		var page jiraUpdatedResp
		if err := json.Unmarshal(raw, &page); err != nil {
			return 0, fmt.Errorf("decode jira search: %w", err)
		}
		for _, is := range page.Issues {
			t := fetchers.ParseJiraTime(is.Fields.Updated)
			if t.IsZero() {
				return 0, fmt.Errorf("unparsable updated %q for %s", is.Fields.Updated, is.Key)
			}
			keys = append(keys, is.Key)
			updated = append(updated, t)
		}
		if page.IsLast || page.NextPageToken == "" {
			break
		}
		pageToken = page.NextPageToken
	}
	if len(keys) == 0 {
		return 0, nil
	}

	rows, err := deps.DB.Query(ctx, `
		SELECT n.id FROM unnest($1::text[], $2::timestamptz[]) AS j(key, upd)
		JOIN graph.nodes n ON n.id = 'jira:' || j.key
		WHERE n.type='jira' AND n.deleted_at IS NULL AND coalesce(n.body,'') <> ''
		  AND (n.body_ts IS NULL OR n.body_ts < j.upd)`, keys, updated)
	if err != nil {
		return 0, fmt.Errorf("select candidates: %w", err)
	}
	candidates, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return 0, fmt.Errorf("select candidates: %w", err)
	}

	queued := 0
	for _, id := range candidates {
		tag, err := deps.DB.Exec(ctx, `
			INSERT INTO graph.jobs (type, payload, priority, machine_id)
			SELECT 'fetch_body', jsonb_build_object('node_id', $1::text), 5, $2
			WHERE NOT EXISTS (SELECT 1 FROM graph.jobs
			                  WHERE type='fetch_body' AND status IN ('queued','running')
			                    AND payload->>'node_id' = $1)`, id, deps.MachineID)
		if err != nil {
			return 0, fmt.Errorf("enqueue fetch_body %s: %w", id, err)
		}
		queued += int(tag.RowsAffected())
	}
	return queued, nil
}

// jiraUpdatesStatus is the GET/PUT /api/graph/jira-updates body.
type jiraUpdatesStatus struct {
	Enabled         bool    `json:"enabled"`
	IntervalMinutes int     `json:"interval_minutes"`
	LastOKAt        *string `json:"last_ok_at"`
	LastRunAt       *string `json:"last_run_at"`
	LastError       string  `json:"last_error"`
	LastQueued      *int    `json:"last_queued"`
}

func readJiraUpdatesStatus(ctx context.Context, db *pgxpool.Pool) (jiraUpdatesStatus, error) {
	conf, err := readJiraUpdatesConfig(ctx, db)
	if err != nil {
		return jiraUpdatesStatus{}, err
	}
	st := jiraUpdatesStatus{Enabled: conf.Enabled, IntervalMinutes: conf.IntervalMinutes}
	optional := func(key string) (*string, error) {
		v, ok, err := getSetting(ctx, db, key)
		if err != nil || !ok || v == "" {
			return nil, err
		}
		return &v, nil
	}
	if st.LastOKAt, err = optional(jiraUpdatesKeyLastOK); err != nil {
		return jiraUpdatesStatus{}, err
	}
	if st.LastRunAt, err = optional(jiraUpdatesKeyLastRun); err != nil {
		return jiraUpdatesStatus{}, err
	}
	if st.LastError, _, err = getSetting(ctx, db, jiraUpdatesKeyLastErr); err != nil {
		return jiraUpdatesStatus{}, err
	}
	q, err := optional(jiraUpdatesKeyQueued)
	if err != nil {
		return jiraUpdatesStatus{}, err
	}
	if q != nil {
		if n, perr := strconv.Atoi(*q); perr == nil {
			st.LastQueued = &n
		}
	}
	return st, nil
}

// NewJiraUpdatesHandler serves GET and PUT /api/graph/jira-updates.
func NewJiraUpdatesHandler(deps Deps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
		case http.MethodPut:
			var req struct {
				Enabled         *bool `json:"enabled"`
				IntervalMinutes *int  `json:"interval_minutes"`
			}
			r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				writeError(w, http.StatusBadRequest, "invalid JSON")
				return
			}
			if req.Enabled == nil || req.IntervalMinutes == nil {
				writeError(w, http.StatusBadRequest, "enabled and interval_minutes are required")
				return
			}
			if *req.IntervalMinutes < jiraUpdatesMinInterval || *req.IntervalMinutes > jiraUpdatesMaxInterval {
				writeError(w, http.StatusBadRequest, "interval_minutes must be 5-1440")
				return
			}
			if err := saveJiraUpdatesConfig(r.Context(), deps.DB, *req.Enabled, *req.IntervalMinutes); err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
		default:
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		st, err := readJiraUpdatesStatus(r.Context(), deps.DB)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, st)
	})
}

func saveJiraUpdatesConfig(ctx context.Context, db *pgxpool.Pool, enabled bool, interval int) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := putSetting(ctx, tx, jiraUpdatesKeyEnabled, strconv.FormatBool(enabled)); err != nil {
		return err
	}
	if err := putSetting(ctx, tx, jiraUpdatesKeyInterval, strconv.Itoa(interval)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
