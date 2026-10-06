package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	"github.com/agent-mem/agent-mem/internal/graph/ids"
	"github.com/agent-mem/agent-mem/internal/graph/jobs"
)

// refresh_gh_key_search links wego/* GitHub PRs to the PAY tickets their title or
// body names. It walks GitHub's search API by the PRs' own updated time, oldest
// first, and moves a per-PR cursor after every PR it finishes, so a run that stops
// anywhere resumes at the last finished PR. Like refresh_jira_updates, the handler
// always returns nil; its outcome lives in the gh_key_search_* settings.

const (
	ghKeyKeyEnabled   = "gh_key_search_enabled"
	ghKeyKeyInterval  = "gh_key_search_interval_minutes"
	ghKeyKeyStartDate = "gh_key_search_start_date"
	ghKeyKeyCursor    = "gh_key_search_cursor"
	ghKeyKeyLastRun   = "gh_key_search_last_run_at"
	ghKeyKeyLastOK    = "gh_key_search_last_ok_at"
	ghKeyKeyLastErr   = "gh_key_search_last_error"
	ghKeyKeyLinked    = "gh_key_search_last_linked"
	ghKeyKeyUnmatched = "gh_key_search_last_unmatched"

	ghKeyDefaultInterval  = 60
	ghKeyMinInterval      = 15
	ghKeyMaxInterval      = 1440
	ghKeyDefaultStartDate = "2025-01-01"
	ghKeyTickEvery        = 60 * time.Second
	ghKeyHealthTimeout    = 5 * time.Second
	ghKeyRunBudget        = 240 * time.Second
	ghKeyRequestTimeout   = 20 * time.Second
	ghKeyEndLag           = 2 * time.Minute
	ghKeyOverlap          = 10 * time.Minute
	ghKeyMaxPerMinute     = 25
	ghKeyPageSize         = 100
	ghKeyMaxResults       = 1000
	ghKeyEdgeSource       = "gh_key_search"
	ghKeyTimeFmt          = "2006-01-02T15:04:05Z"
)

var (
	ghKeyCandidate = regexp.MustCompile(`(?i)pay-[0-9]+`)
	ghKeyRepoURL   = regexp.MustCompile(`/repos/wego/([^/]+)$`)
)

// ghKeyEnv is what the ticker needs to enqueue a run.
type ghKeyEnv struct {
	MachineID string
	Runner    string
}

type ghKeyConfig struct {
	Enabled         bool
	IntervalMinutes int
	StartDate       string
}

func readGHKeyConfig(ctx context.Context, db *pgxpool.Pool) (ghKeyConfig, error) {
	conf := ghKeyConfig{IntervalMinutes: ghKeyDefaultInterval, StartDate: ghKeyDefaultStartDate}
	v, _, err := getSetting(ctx, db, ghKeyKeyEnabled)
	if err != nil {
		return conf, err
	}
	conf.Enabled = v == "true"
	v, ok, err := getSetting(ctx, db, ghKeyKeyInterval)
	if err != nil {
		return conf, err
	}
	if n, perr := strconv.Atoi(strings.TrimSpace(v)); ok && perr == nil && n >= ghKeyMinInterval && n <= ghKeyMaxInterval {
		conf.IntervalMinutes = n
	}
	v, ok, err = getSetting(ctx, db, ghKeyKeyStartDate)
	if err != nil {
		return conf, err
	}
	if _, perr := time.Parse("2006-01-02", v); ok && perr == nil {
		conf.StartDate = v
	}
	return conf, nil
}

// ghKeySearchTick enqueues refresh_gh_key_search when enabled, due, and none is
// queued or running.
func ghKeySearchTick(ctx context.Context, db *pgxpool.Pool, env ghKeyEnv, now time.Time) error {
	conf, err := readGHKeyConfig(ctx, db)
	if err != nil {
		return err
	}
	if !conf.Enabled {
		return nil
	}
	last, ok, err := getSetting(ctx, db, ghKeyKeyLastRun)
	if err != nil {
		return err
	}
	if ok {
		if t, perr := time.Parse(time.RFC3339, last); perr == nil && now.Sub(t) < time.Duration(conf.IntervalMinutes)*time.Minute {
			return nil
		}
	}
	runner := env.Runner
	if runner == "" {
		runner = "any"
	}
	if _, err := db.Exec(ctx, `
		INSERT INTO graph.jobs (type, payload, priority, machine_id, target_runner)
		SELECT 'refresh_gh_key_search', '{}', 5, $1, $2
		WHERE NOT EXISTS (SELECT 1 FROM graph.jobs
		                  WHERE type='refresh_gh_key_search' AND status IN ('queued','running'))`,
		env.MachineID, runner); err != nil {
		return fmt.Errorf("enqueue refresh_gh_key_search: %w", err)
	}
	return nil
}

// RunGHKeySearchTicker drives the refresh_gh_key_search cadence once a minute
// until ctx is cancelled.
func RunGHKeySearchTicker(ctx context.Context, db *pgxpool.Pool, machineID, runner string, log zerolog.Logger) {
	t := time.NewTicker(ghKeyTickEvery)
	defer t.Stop()
	env := ghKeyEnv{MachineID: machineID, Runner: runner}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := ghKeySearchTick(ctx, db, env, time.Now()); err != nil && ctx.Err() == nil {
				log.Error().Err(err).Msg("gh key search ticker: tick failed")
			}
		}
	}
}

// NewRefreshGHKeySearchHandler returns the job entry for "refresh_gh_key_search".
func NewRefreshGHKeySearchHandler(deps Deps) jobs.Entry {
	return jobs.Entry{
		Handler:  newGHKeyRunner(deps).handler(),
		Systems:  []string{"github"},
		PoolSize: 1,
		Lease:    300 * time.Second,
	}
}

// ghKeyRunner holds the run's collaborators; tests replace the injectable ones.
type ghKeyRunner struct {
	deps    Deps
	client  *http.Client
	now     func() time.Time
	sleep   func(ctx context.Context, d time.Duration) error
	stripDB *pgxpool.Pool

	// successWrite stores the success health values.
	successWrite func(ctx context.Context, linked, unmatched int, at time.Time) error
	// beforeFetchJob runs inside the item transaction before the fetch_body insert.
	beforeFetchJob func() error
	// afterItem runs after each item's transaction committed.
	afterItem func()
	// reqTimeout bounds each HTTP request.
	reqTimeout time.Duration

	reqTimes []time.Time
	cursor   time.Time
}

func newGHKeyRunner(deps Deps) *ghKeyRunner {
	r := &ghKeyRunner{
		deps:       deps,
		client:     &http.Client{},
		now:        func() time.Time { return time.Now().UTC() },
		stripDB:    deps.DB,
		reqTimeout: ghKeyRequestTimeout,
		sleep: func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				return nil
			}
		},
	}
	r.successWrite = r.defaultSuccessWrite
	return r
}

func (r *ghKeyRunner) defaultSuccessWrite(ctx context.Context, linked, unmatched int, at time.Time) error {
	tx, err := r.deps.DB.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, kv := range [][2]string{
		{ghKeyKeyLastOK, at.UTC().Format(time.RFC3339)},
		{ghKeyKeyLinked, strconv.Itoa(linked)},
		{ghKeyKeyUnmatched, strconv.Itoa(unmatched)},
		{ghKeyKeyLastErr, ""},
	} {
		if err := putSetting(ctx, tx, kv[0], kv[1]); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// health runs a settings write that survives a cancelled job context.
func (r *ghKeyRunner) health(ctx context.Context, write func(context.Context) error) bool {
	hctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ghKeyHealthTimeout)
	defer cancel()
	if err := write(hctx); err != nil {
		r.deps.Logger.Error().Err(err).Msg("refresh_gh_key_search: health write failed")
		return false
	}
	return true
}

func (r *ghKeyRunner) fail(ctx context.Context, msg string) {
	r.deps.Logger.Warn().Str("error", msg).Msg("refresh_gh_key_search: run failed")
	r.health(ctx, func(c context.Context) error { return putSetting(c, r.deps.DB, ghKeyKeyLastErr, msg) })
}

func (r *ghKeyRunner) handler() jobs.Handler {
	return func(ctx context.Context, _ []byte) error {
		r.run(ctx)
		return nil
	}
}

func (r *ghKeyRunner) run(ctx context.Context) {
	start := r.now().UTC().Truncate(time.Second)
	end := start.Add(-ghKeyEndLag)
	deadline := start.Add(ghKeyRunBudget)
	linked, unmatched := 0, 0
	r.reqTimes = nil

	if !r.health(ctx, func(c context.Context) error {
		return putSetting(c, r.deps.DB, ghKeyKeyLastRun, start.Format(time.RFC3339))
	}) {
		return
	}
	if r.deps.GHToken == "" {
		r.fail(ctx, "github token not set")
		return
	}

	raw, ok, err := getSetting(ctx, r.deps.DB, ghKeyKeyCursor)
	if err != nil {
		r.fail(ctx, err.Error())
		return
	}
	var c time.Time
	if !ok {
		sd, _, err := getSetting(ctx, r.deps.DB, ghKeyKeyStartDate)
		if err != nil {
			r.fail(ctx, err.Error())
			return
		}
		d, perr := time.Parse("2006-01-02", sd)
		if perr != nil {
			d, _ = time.Parse("2006-01-02", ghKeyDefaultStartDate)
		}
		c = d.UTC()
	} else {
		c, err = time.Parse(time.RFC3339, raw)
		if err != nil {
			r.fail(ctx, "cursor unparsable: "+raw)
			return
		}
		c = c.UTC()
	}
	r.cursor = c

	if c.Before(end) {
		q := c.Add(-ghKeyOverlap)
		for {
			before := r.cursor
			total, stopped, l, u, err := r.query(ctx, q, end, deadline)
			linked += l
			unmatched += u
			if err != nil {
				r.fail(ctx, err.Error())
				return
			}
			if stopped || total <= ghKeyMaxResults {
				break
			}
			if !r.cursor.After(before) {
				r.fail(ctx, "stalled at "+r.cursor.Format(ghKeyTimeFmt))
				return
			}
			q = r.cursor
		}
	}

	if err := r.healthErr(ctx, func(c context.Context) error {
		return r.successWrite(c, linked, unmatched, r.now())
	}); err != nil {
		r.deps.Logger.Error().Err(err).Msg("refresh_gh_key_search: success write failed")
		return
	}
	r.deps.Logger.Info().Int("linked", linked).Int("unmatched", unmatched).Msg("refresh_gh_key_search: done")
}

func (r *ghKeyRunner) healthErr(ctx context.Context, write func(context.Context) error) error {
	hctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), ghKeyHealthTimeout)
	defer cancel()
	return write(hctx)
}

type ghSearchItem struct {
	Number        int             `json:"number"`
	Title         string          `json:"title"`
	Body          *string         `json:"body"`
	HTMLURL       string          `json:"html_url"`
	RepositoryURL string          `json:"repository_url"`
	UpdatedAt     string          `json:"updated_at"`
	PullRequest   json.RawMessage `json:"pull_request"`
}

type ghSearchResp struct {
	TotalCount        int            `json:"total_count"`
	IncompleteResults bool           `json:"incomplete_results"`
	Items             []ghSearchItem `json:"items"`
}

// query reads every page of one search window. stopped is true when the run
// budget ran out (a normal stop).
func (r *ghKeyRunner) query(ctx context.Context, q, end, deadline time.Time) (total int, stopped bool, linked, unmatched int, err error) {
	search := fmt.Sprintf("org:wego is:pr PAY in:title,body updated:%s..%s", q.UTC().Format(ghKeyTimeFmt), end.UTC().Format(ghKeyTimeFmt))
	pages := 1
	for p := 1; p <= pages && p <= ghKeyMaxResults/ghKeyPageSize; p++ {
		if !r.now().Before(deadline) {
			return total, true, linked, unmatched, nil
		}
		if err = r.pace(ctx); err != nil {
			return
		}
		if !r.now().Before(deadline) {
			return total, true, linked, unmatched, nil
		}
		var resp ghSearchResp
		if resp, err = r.fetchPage(ctx, search, p); err != nil {
			return
		}
		if resp.IncompleteResults {
			err = errors.New("github search returned incomplete_results")
			return
		}
		total = resp.TotalCount
		pages = (min(total, ghKeyMaxResults) + ghKeyPageSize - 1) / ghKeyPageSize
		if len(resp.Items) == 0 {
			break
		}
		for _, it := range resp.Items {
			if !r.now().Before(deadline) {
				return total, true, linked, unmatched, nil
			}
			var l, u int
			if l, u, err = r.processItem(ctx, it); err != nil {
				return
			}
			linked += l
			unmatched += u
			if r.afterItem != nil {
				r.afterItem()
			}
		}
	}
	return total, false, linked, unmatched, nil
}

// pace keeps requests at or under ghKeyMaxPerMinute in any 60 s span.
func (r *ghKeyRunner) pace(ctx context.Context) error {
	for {
		now := r.now()
		keep := r.reqTimes[:0]
		for _, t := range r.reqTimes {
			if now.Sub(t) < time.Minute {
				keep = append(keep, t)
			}
		}
		r.reqTimes = keep
		if len(r.reqTimes) < ghKeyMaxPerMinute {
			r.reqTimes = append(r.reqTimes, now)
			return nil
		}
		wait := r.reqTimes[0].Add(time.Minute).Sub(now)
		if err := r.sleep(ctx, wait); err != nil {
			return err
		}
	}
}

func (r *ghKeyRunner) fetchPage(ctx context.Context, search string, page int) (ghSearchResp, error) {
	var out ghSearchResp
	vals := url.Values{}
	vals.Set("q", search)
	vals.Set("sort", "updated")
	vals.Set("order", "asc")
	vals.Set("per_page", strconv.Itoa(ghKeyPageSize))
	vals.Set("page", strconv.Itoa(page))
	base := strings.TrimRight(r.deps.GHBaseURL, "/")
	if base == "" {
		base = "https://api.github.com"
	}
	rctx, cancel := context.WithTimeout(ctx, r.reqTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, base+"/search/issues?"+vals.Encode(), nil)
	if err != nil {
		return out, err
	}
	req.Header.Set("Authorization", "Bearer "+r.deps.GHToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := r.client.Do(req)
	if err != nil {
		return out, fmt.Errorf("github search: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return out, fmt.Errorf("github search read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return out, fmt.Errorf("github search: HTTP %d", resp.StatusCode)
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return out, fmt.Errorf("github search: invalid JSON: %w", err)
	}
	return out, nil
}

// extractPAYKeys returns the de-duplicated upper-case PAY-<n> keys in text. A key
// must follow the start of the text or a character that is not a letter, digit or
// '-'; digits are consumed greedily so PAY-24145 never yields PAY-2414.
func extractPAYKeys(text string) []string {
	var keys []string
	seen := map[string]bool{}
	for _, loc := range ghKeyCandidate.FindAllStringIndex(text, -1) {
		if loc[0] > 0 {
			prev, _ := utf8.DecodeLastRuneInString(text[:loc[0]])
			if unicode.IsLetter(prev) || unicode.IsDigit(prev) || prev == '-' {
				continue
			}
		}
		k := strings.ToUpper(text[loc[0]:loc[1]])
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	return keys
}

// processItem handles one search hit in one transaction: links and cursor move
// together.
func (r *ghKeyRunner) processItem(ctx context.Context, it ghSearchItem) (linked, unmatched int, err error) {
	updated, err := time.Parse(time.RFC3339, it.UpdatedAt)
	if err != nil {
		return 0, 0, fmt.Errorf("item %s#%d: bad updated_at %q", it.RepositoryURL, it.Number, it.UpdatedAt)
	}
	updated = updated.UTC()

	var prID, repo string
	var keys []string
	if m := ghKeyRepoURL.FindStringSubmatch(it.RepositoryURL); m != nil && len(it.PullRequest) > 0 && string(it.PullRequest) != "null" {
		repo = m[1]
		if prID, err = ids.GHPR("wego/"+repo, it.Number); err != nil {
			repo = ""
		}
	}
	if repo != "" {
		body := ""
		if it.Body != nil {
			body = *it.Body
		}
		if body, err = stripPRBoilerplateErr(ctx, r.stripDB, prID, body); err != nil {
			return 0, 0, fmt.Errorf("%s: boilerplate lookup: %w", prID, err)
		}
		keys = extractPAYKeys(it.Title + "\n" + body)
	}

	tx, err := r.deps.DB.Begin(ctx)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var kept []string
	for _, k := range keys {
		jid, err := ids.Jira(k)
		if err != nil {
			unmatched++
			continue
		}
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM graph.nodes WHERE id=$1 AND deleted_at IS NULL)`, jid).Scan(&exists); err != nil {
			return 0, 0, err
		}
		if exists {
			kept = append(kept, jid)
		} else {
			unmatched++
		}
	}

	if len(kept) > 0 {
		if _, err := tx.Exec(ctx, `
			INSERT INTO graph.nodes (id, type, natural_key, url, title, updated_at, machine_id)
			VALUES ($1, 'gh_pr', $2, $3, $4, NOW(), $5)
			ON CONFLICT (id) DO NOTHING`,
			prID, fmt.Sprintf("wego/%s#%d", repo, it.Number), it.HTMLURL, it.Title, r.deps.MachineID); err != nil {
			return 0, 0, err
		}
		for _, jid := range kept {
			tag, err := tx.Exec(ctx, `
				INSERT INTO graph.edges (from_node_id, to_node_id, kind, source_msg_id, machine_id)
				VALUES ($1, $2, 'REFERENCES', $3, $4)
				ON CONFLICT (from_node_id, to_node_id, kind) DO NOTHING`,
				prID, jid, ghKeyEdgeSource, r.deps.MachineID)
			if err != nil {
				return 0, 0, err
			}
			linked += int(tag.RowsAffected())
		}
		var empty bool
		if err := tx.QueryRow(ctx, `SELECT coalesce(body,'') = '' FROM graph.nodes WHERE id=$1`, prID).Scan(&empty); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return 0, 0, err
		} else if err == nil && empty {
			if r.beforeFetchJob != nil {
				if err := r.beforeFetchJob(); err != nil {
					return 0, 0, err
				}
			}
			raw, err := json.Marshal(fetchBodyPayload{NodeID: prID, Via: ghKeyEdgeSource})
			if err != nil {
				return 0, 0, err
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO graph.jobs (type, payload, priority, machine_id)
				SELECT 'fetch_body', $2::jsonb, 5, $3
				WHERE NOT EXISTS (SELECT 1 FROM graph.jobs
				                  WHERE type='fetch_body' AND status IN ('queued','running')
				                    AND payload->>'node_id' = $1)`,
				prID, raw, r.deps.MachineID); err != nil {
				return 0, 0, err
			}
		}
	}

	// Cursor never moves backwards; compared as timestamps.
	if _, err := tx.Exec(ctx, `
		INSERT INTO settings(key, value) VALUES($1, $2)
		ON CONFLICT(key) DO UPDATE SET value = EXCLUDED.value
		WHERE settings.value::timestamptz < EXCLUDED.value::timestamptz`,
		ghKeyKeyCursor, updated.Format(ghKeyTimeFmt)); err != nil {
		return 0, 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, 0, err
	}
	if updated.After(r.cursor) {
		r.cursor = updated
	}
	return linked, unmatched, nil
}

// ghKeyStatus is the GET/PUT /api/graph/gh-key-search body.
type ghKeyStatus struct {
	Enabled         bool    `json:"enabled"`
	IntervalMinutes int     `json:"interval_minutes"`
	StartDate       string  `json:"start_date"`
	Cursor          *string `json:"cursor"`
	LastRunAt       *string `json:"last_run_at"`
	LastOKAt        *string `json:"last_ok_at"`
	LastError       string  `json:"last_error"`
	LastLinked      *int    `json:"last_linked"`
	LastUnmatched   *int    `json:"last_unmatched"`
}

func readGHKeyStatus(ctx context.Context, db *pgxpool.Pool) (ghKeyStatus, error) {
	conf, err := readGHKeyConfig(ctx, db)
	if err != nil {
		return ghKeyStatus{}, err
	}
	st := ghKeyStatus{Enabled: conf.Enabled, IntervalMinutes: conf.IntervalMinutes, StartDate: conf.StartDate}
	optional := func(key string) (*string, error) {
		v, ok, err := getSetting(ctx, db, key)
		if err != nil || !ok || v == "" {
			return nil, err
		}
		return &v, nil
	}
	optInt := func(key string) (*int, error) {
		s, err := optional(key)
		if err != nil || s == nil {
			return nil, err
		}
		n, perr := strconv.Atoi(*s)
		if perr != nil {
			return nil, nil
		}
		return &n, nil
	}
	if st.Cursor, err = optional(ghKeyKeyCursor); err != nil {
		return st, err
	}
	if st.LastRunAt, err = optional(ghKeyKeyLastRun); err != nil {
		return st, err
	}
	if st.LastOKAt, err = optional(ghKeyKeyLastOK); err != nil {
		return st, err
	}
	if st.LastError, _, err = getSetting(ctx, db, ghKeyKeyLastErr); err != nil {
		return st, err
	}
	if st.LastLinked, err = optInt(ghKeyKeyLinked); err != nil {
		return st, err
	}
	if st.LastUnmatched, err = optInt(ghKeyKeyUnmatched); err != nil {
		return st, err
	}
	return st, nil
}

// NewGHKeySearchHandler serves GET and PUT /api/graph/gh-key-search.
func NewGHKeySearchHandler(deps Deps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
		case http.MethodPut:
			var req struct {
				Enabled         *bool   `json:"enabled"`
				IntervalMinutes *int    `json:"interval_minutes"`
				StartDate       *string `json:"start_date"`
			}
			r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				writeError(w, http.StatusBadRequest, "invalid JSON")
				return
			}
			if req.Enabled == nil || req.IntervalMinutes == nil || req.StartDate == nil {
				writeError(w, http.StatusBadRequest, "enabled, interval_minutes and start_date are required")
				return
			}
			if *req.IntervalMinutes < ghKeyMinInterval || *req.IntervalMinutes > ghKeyMaxInterval {
				writeError(w, http.StatusBadRequest, "interval_minutes must be 15-1440")
				return
			}
			if _, err := time.Parse("2006-01-02", *req.StartDate); err != nil {
				writeError(w, http.StatusBadRequest, "start_date must be a calendar date YYYY-MM-DD")
				return
			}
			tx, err := deps.DB.Begin(r.Context())
			if err == nil {
				defer func() { _ = tx.Rollback(r.Context()) }()
				for _, kv := range [][2]string{
					{ghKeyKeyEnabled, strconv.FormatBool(*req.Enabled)},
					{ghKeyKeyInterval, strconv.Itoa(*req.IntervalMinutes)},
					{ghKeyKeyStartDate, *req.StartDate},
				} {
					if err = putSetting(r.Context(), tx, kv[0], kv[1]); err != nil {
						break
					}
				}
				if err == nil {
					err = tx.Commit(r.Context())
				}
			}
			if err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
		default:
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		st, err := readGHKeyStatus(r.Context(), deps.DB)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, st)
	})
}
