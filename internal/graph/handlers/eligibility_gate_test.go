package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	"github.com/agent-mem/agent-mem/internal/gemini"
)

type eligibilityGemini struct {
	mu          sync.Mutex
	embed       func(string) ([]float32, error)
	embedCalls  map[string]int
	cheapResult string
	cheapErr    error
	cheapCalls  int
}

func (g *eligibilityGemini) Embed(_ context.Context, text string) ([]float32, error) {
	g.mu.Lock()
	if g.embedCalls == nil {
		g.embedCalls = make(map[string]int)
	}
	g.embedCalls[text]++
	g.mu.Unlock()
	if g.embed != nil {
		return g.embed(text)
	}
	return []float32{1, 0}, nil
}

func (g *eligibilityGemini) EmbedWithOptions(ctx context.Context, text string, _ gemini.EmbedOptions) ([]float32, error) {
	return g.Embed(ctx, text)
}

func (*eligibilityGemini) Describe(context.Context, string, []byte, string) (string, string, []string, error) {
	return "", "", nil, nil
}

func (*eligibilityGemini) Generate(context.Context, string, string) (string, error) {
	return "", nil
}

func (g *eligibilityGemini) GenerateCheap(context.Context, string, string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.cheapCalls++
	return g.cheapResult, g.cheapErr
}

func (g *eligibilityGemini) embedCallCount(text string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.embedCalls[text]
}

func (g *eligibilityGemini) cheapCallCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.cheapCalls
}

func seedEligibilityScope(t *testing.T, pool *pgxpool.Pool, definition string) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(t.Context(), `
		INSERT INTO graph.topic_subscriptions
		  (subscriber_slack_id, topic, channel_filter, min_participants, max_author_depth,
		   sources, scope_definition, scope_status, scope_refreshed_at)
		VALUES ($1, $2, '{}'::text[], 2, 99, '[]'::jsonb, $3, 'ready', NOW())
		RETURNING id`, "UELIGTEST", "eligibility-test", definition).Scan(&id)
	if err != nil {
		t.Fatalf("seed eligibility scope: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM graph.topic_subscriptions WHERE id=$1`, id)
	})
	return id
}

func setEligibilityConfig(t *testing.T, pool *pgxpool.Pool, raw string) {
	t.Helper()
	var previous string
	err := pool.QueryRow(t.Context(), `SELECT value FROM settings WHERE key=$1`, eligibilityGateKey).Scan(&previous)
	hadPrevious := err == nil
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO settings(key,value) VALUES($1,$2)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, eligibilityGateKey, raw); err != nil {
		t.Fatalf("set eligibility config: %v", err)
	}
	invalidateEligibilityGate()
	t.Cleanup(func() {
		if hadPrevious {
			_, _ = pool.Exec(context.Background(), `UPDATE settings SET value=$2 WHERE key=$1`, eligibilityGateKey, previous)
		} else {
			_, _ = pool.Exec(context.Background(), `DELETE FROM settings WHERE key=$1`, eligibilityGateKey)
		}
		invalidateEligibilityGate()
	})
}

func eligibilityConfigJSON(t *testing.T, cfg eligibilityGateConfig) string {
	t.Helper()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal eligibility config: %v", err)
	}
	return string(raw)
}

func eligibilityRequest(channelID, ts, body string) map[string]any {
	return map[string]any{
		"source":        "slack",
		"canonical_url": fmt.Sprintf("https://wego.slack.com/archives/%s/p%s", channelID, ts),
		"body":          body,
		"metadata": map[string]any{
			"channel_id": channelID,
			"ts":         ts,
			"body_ts":    "2026-08-24T00:00:00Z",
			"author":     map[string]any{"display_name": "Eligibility Tester"},
			"mentions":   []any{},
			"files":      []any{},
		},
	}
}

func eligibilityThreadRequest(channelID, ts, threadTS, body string) map[string]any {
	req := eligibilityRequest(channelID, ts, body)
	req["metadata"].(map[string]any)["thread_ts"] = threadTS
	return req
}

func eligibilityScopeVersion(t *testing.T, pool *pgxpool.Pool, scopeID int64) time.Time {
	t.Helper()
	var version time.Time
	if err := pool.QueryRow(t.Context(),
		`SELECT scope_refreshed_at FROM graph.topic_subscriptions WHERE id=$1`, scopeID).Scan(&version); err != nil {
		t.Fatalf("read eligibility scope version: %v", err)
	}
	return version
}

func seedEligibilityDecision(
	t *testing.T,
	pool *pgxpool.Pool,
	channelID, messageTS string,
	score any,
	decision, source, mode string,
	scopeVersion, decidedAt time.Time,
) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO graph.eligibility_decisions
		  (channel_id, message_ts, score, decision, decision_source, mode, scope_version, decided_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		channelID, messageTS, score, decision, source, mode, scopeVersion, decidedAt); err != nil {
		t.Fatalf("seed eligibility decision: %v", err)
	}
}

func eligibilityLookupErrorPool(t *testing.T, allowedAcquisitions int) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if databaseName(dsn) != "agentmem_test" {
		t.Fatalf("lookup error pool requires agentmem_test")
	}
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse lookup error pool config: %v", err)
	}
	config.MaxConns = 1
	var mu sync.Mutex
	acquisitions := 0
	config.BeforeAcquire = func(context.Context, *pgx.Conn) bool {
		mu.Lock()
		defer mu.Unlock()
		acquisitions++
		return acquisitions <= allowedAcquisitions
	}
	pool, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatalf("create lookup error pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func eligibilityDeps(pool *pgxpool.Pool, client GeminiClient) Deps {
	return Deps{
		DB:        pool,
		Logger:    zerolog.Nop(),
		MachineID: "eligibility-test",
		Gemini:    client,
	}
}

func decodeIngestResponse(t *testing.T, wBody []byte) ingestResponse {
	t.Helper()
	var resp ingestResponse
	if err := json.Unmarshal(wBody, &resp); err != nil {
		t.Fatalf("decode ingest response: %v", err)
	}
	return resp
}

func TestEligibilityGateDryRunAuditsLowBoundaryAndCachesScope(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)
	const (
		channelID = "CELIGDRY"
		scopeText = "payments checkout scope"
	)
	scopeID := seedEligibilityScope(t, pool, scopeText)
	setEligibilityConfig(t, pool, eligibilityConfigJSON(t, eligibilityGateConfig{
		Enabled:             true,
		Mode:                eligibilityModeDryRun,
		ScopeSubscriptionID: scopeID,
		HighThreshold:       1,
		LowThreshold:        0,
		GatedChannels:       []string{channelID},
	}))

	client := &eligibilityGemini{embed: func(text string) ([]float32, error) {
		if text == scopeText {
			return []float32{1, 0}, nil
		}
		return []float32{0, 1}, nil
	}}
	handler := NewIngestContentHandler(eligibilityDeps(pool, client))

	for i, ts := range []string{"1800000000.000001", "1800000000.000002"} {
		w := postJSON(t, handler, eligibilityRequest(channelID, ts, fmt.Sprintf("unrelated message %d", i)))
		if w.Code != http.StatusOK {
			t.Fatalf("POST %d: status = %d, body = %s", i, w.Code, w.Body.String())
		}
		if outcome := decodeIngestResponse(t, w.Body.Bytes()).Outcome; outcome != "created" {
			t.Fatalf("POST %d outcome = %q, want created in dry_run", i, outcome)
		}
	}

	if got := client.embedCallCount(scopeText); got != 1 {
		t.Fatalf("scope embed calls = %d, want 1", got)
	}
	var decisions, nodes int
	var minScore, maxScore float64
	if err := pool.QueryRow(t.Context(), `
		SELECT COUNT(*), MIN(score), MAX(score)
		FROM graph.eligibility_decisions
		WHERE channel_id=$1 AND decision='ineligible' AND mode='dry_run' AND scope_version IS NOT NULL`, channelID).
		Scan(&decisions, &minScore, &maxScore); err != nil {
		t.Fatalf("read eligibility decisions: %v", err)
	}
	if decisions != 2 || minScore != 0 || maxScore != 0 {
		t.Fatalf("dry-run decisions = %d scores [%v,%v], want 2 scores [0,0]", decisions, minScore, maxScore)
	}
	if err := pool.QueryRow(t.Context(), `SELECT COUNT(*) FROM graph.nodes WHERE scope=$1`, "slack:"+channelID).Scan(&nodes); err != nil {
		t.Fatalf("count dry-run nodes: %v", err)
	}
	if nodes != 2 {
		t.Fatalf("dry-run nodes = %d, want 2", nodes)
	}
}

func TestEligibilityGateEnforceThresholdBoundaries(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)
	const (
		channelID = "CELIGBOUND"
		scopeText = "payments scope boundary"
		highBody  = "exactly high"
		lowBody   = "exactly low"
	)
	scopeID := seedEligibilityScope(t, pool, scopeText)
	setEligibilityConfig(t, pool, eligibilityConfigJSON(t, eligibilityGateConfig{
		Enabled:             true,
		Mode:                eligibilityModeEnforce,
		ScopeSubscriptionID: scopeID,
		HighThreshold:       1,
		LowThreshold:        0,
		GatedChannels:       []string{channelID},
	}))

	client := &eligibilityGemini{embed: func(text string) ([]float32, error) {
		switch text {
		case scopeText, highBody:
			return []float32{1, 0}, nil
		case lowBody:
			return []float32{0, 1}, nil
		default:
			return nil, fmt.Errorf("unexpected embed text %q", text)
		}
	}}
	handler := NewIngestContentHandler(eligibilityDeps(pool, client))

	high := postJSON(t, handler, eligibilityRequest(channelID, "1800000010.000001", highBody))
	if outcome := decodeIngestResponse(t, high.Body.Bytes()).Outcome; outcome != "created" {
		t.Fatalf("score == high outcome = %q, want created", outcome)
	}
	low := postJSON(t, handler, eligibilityRequest(channelID, "1800000010.000002", lowBody))
	if outcome := decodeIngestResponse(t, low.Body.Bytes()).Outcome; outcome != eligibilitySkippedOutcome {
		t.Fatalf("score == low outcome = %q, want %q", outcome, eligibilitySkippedOutcome)
	}

	var lowNodes int
	if err := pool.QueryRow(t.Context(), `SELECT COUNT(*) FROM graph.nodes WHERE id=$1`, "slack:"+channelID+":1800000010.000002").Scan(&lowNodes); err != nil {
		t.Fatalf("count low-boundary node: %v", err)
	}
	if lowNodes != 0 {
		t.Fatalf("low-boundary nodes = %d, want 0", lowNodes)
	}
}

func TestEligibilityGateExemptChannelIsNeverScored(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)
	const channelID = "CELIGEXEMPT"
	scopeID := seedEligibilityScope(t, pool, "payments exempt scope")
	setEligibilityConfig(t, pool, eligibilityConfigJSON(t, eligibilityGateConfig{
		Enabled:             true,
		Mode:                eligibilityModeEnforce,
		ScopeSubscriptionID: scopeID,
		HighThreshold:       0.8,
		LowThreshold:        0.2,
		ExemptChannels:      []string{channelID},
	}))
	client := &eligibilityGemini{embed: func(text string) ([]float32, error) {
		return nil, fmt.Errorf("exempt channel unexpectedly embedded %q", text)
	}}
	handler := NewIngestContentHandler(eligibilityDeps(pool, client))

	w := postJSON(t, handler, eligibilityRequest(channelID, "1800000020.000001", "off topic but exempt"))
	if outcome := decodeIngestResponse(t, w.Body.Bytes()).Outcome; outcome != "created" {
		t.Fatalf("exempt outcome = %q, want created", outcome)
	}
	if got := client.embedCallCount("payments exempt scope"); got != 0 {
		t.Fatalf("exempt scope embed calls = %d, want 0", got)
	}
	var decisions int
	if err := pool.QueryRow(t.Context(), `SELECT COUNT(*) FROM graph.eligibility_decisions WHERE channel_id=$1`, channelID).Scan(&decisions); err != nil {
		t.Fatalf("count exempt decisions: %v", err)
	}
	if decisions != 0 {
		t.Fatalf("exempt decisions = %d, want 0", decisions)
	}
}

func TestEligibilityGateFailsOpen(t *testing.T) {
	tests := []struct {
		name      string
		scopeRow  bool
		embedFunc func(scopeText, messageText string) func(string) ([]float32, error)
	}{
		{
			name:     "missing scope row",
			scopeRow: false,
		},
		{
			name:     "scope embedding unavailable",
			scopeRow: true,
			embedFunc: func(scopeText, _ string) func(string) ([]float32, error) {
				return func(text string) ([]float32, error) {
					if text == scopeText {
						return nil, errors.New("gateway unavailable")
					}
					return []float32{0, 1}, nil
				}
			},
		},
		{
			name:     "message embedding unavailable",
			scopeRow: true,
			embedFunc: func(scopeText, messageText string) func(string) ([]float32, error) {
				return func(text string) ([]float32, error) {
					if text == messageText {
						return nil, errors.New("gateway unavailable")
					}
					if text == scopeText {
						return []float32{1, 0}, nil
					}
					return nil, fmt.Errorf("unexpected embed text %q", text)
				}
			},
		},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool := openTestDB(t)
			truncateGraphHandlerTables(t, pool)
			channelID := fmt.Sprintf("CELIGOPEN%d", i)
			scopeText := "payments fail-open scope " + tt.name
			messageText := "definitely off topic " + tt.name
			var scopeID int64 = math.MaxInt64
			if tt.scopeRow {
				scopeID = seedEligibilityScope(t, pool, scopeText)
			}
			setEligibilityConfig(t, pool, eligibilityConfigJSON(t, eligibilityGateConfig{
				Enabled:             true,
				Mode:                eligibilityModeEnforce,
				ScopeSubscriptionID: scopeID,
				HighThreshold:       0.8,
				LowThreshold:        0.2,
				GatedChannels:       []string{channelID},
			}))
			client := &eligibilityGemini{}
			if tt.embedFunc != nil {
				client.embed = tt.embedFunc(scopeText, messageText)
			}
			handler := NewIngestContentHandler(eligibilityDeps(pool, client))

			w := postJSON(t, handler, eligibilityRequest(channelID, fmt.Sprintf("1800000030.%06d", i+1), messageText))
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
			}
			if outcome := decodeIngestResponse(t, w.Body.Bytes()).Outcome; outcome != "created" {
				t.Fatalf("fail-open outcome = %q, want created", outcome)
			}
		})
	}
}

func TestEligibilityGateUncertainAdjudicationUsesCheapTierAndFailsOpen(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)
	const (
		channelID = "CELIGLLM"
		scopeText = "payments uncertain scope"
		message   = "ambiguous checkout-adjacent chatter"
	)
	scopeID := seedEligibilityScope(t, pool, scopeText)
	setEligibilityConfig(t, pool, eligibilityConfigJSON(t, eligibilityGateConfig{
		Enabled:             true,
		Mode:                eligibilityModeEnforce,
		ScopeSubscriptionID: scopeID,
		HighThreshold:       0.75,
		LowThreshold:        0.25,
		LLMAdjudicate:       true,
		GatedChannels:       []string{channelID},
	}))

	vector := []float32{0.5, float32(math.Sqrt(0.75))}
	client := &eligibilityGemini{
		embed: func(text string) ([]float32, error) {
			if text == scopeText {
				return []float32{1, 0}, nil
			}
			return vector, nil
		},
		cheapResult: "no",
	}
	handler := NewIngestContentHandler(eligibilityDeps(pool, client))
	w := postJSON(t, handler, eligibilityRequest(channelID, "1800000040.000001", message))
	if outcome := decodeIngestResponse(t, w.Body.Bytes()).Outcome; outcome != eligibilitySkippedOutcome {
		t.Fatalf("cheap-tier no outcome = %q, want %q", outcome, eligibilitySkippedOutcome)
	}
	if got := client.cheapCallCount(); got != 1 {
		t.Fatalf("cheap calls = %d, want 1", got)
	}

	client.cheapErr = errors.New("cheap tier unavailable")
	client.cheapResult = ""
	w = postJSON(t, handler, eligibilityRequest(channelID, "1800000040.000002", message))
	if outcome := decodeIngestResponse(t, w.Body.Bytes()).Outcome; outcome != "created" {
		t.Fatalf("cheap-tier error outcome = %q, want created", outcome)
	}
	var eligibleAudits int
	if err := pool.QueryRow(t.Context(), `
		SELECT COUNT(*) FROM graph.eligibility_decisions
		WHERE channel_id=$1 AND message_ts=$2 AND decision='eligible'`,
		channelID, "1800000040.000002").Scan(&eligibleAudits); err != nil {
		t.Fatalf("count fail-open adjudication audit: %v", err)
	}
	if eligibleAudits != 1 {
		t.Fatalf("fail-open adjudication audits = %d, want 1", eligibleAudits)
	}
}

func TestEligibilityGateLegacyScopeWithoutRefreshTimestampScores(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)
	const (
		channelID = "CELIGLEGACY"
		scopeText = "legacy payments scope"
	)
	var scopeID int64
	if err := pool.QueryRow(t.Context(), `
		INSERT INTO graph.topic_subscriptions
		  (subscriber_slack_id, topic, channel_filter, min_participants, max_author_depth,
		   sources, scope_definition, scope_status, scope_refreshed_at)
		VALUES ('UELIGLEGACY', 'eligibility-legacy', '{}'::text[], 2, 99,
		        '[]'::jsonb, $1, 'ready', NULL)
		RETURNING id`, scopeText).Scan(&scopeID); err != nil {
		t.Fatalf("seed legacy eligibility scope: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM graph.topic_subscriptions WHERE id=$1`, scopeID)
	})
	setEligibilityConfig(t, pool, eligibilityConfigJSON(t, eligibilityGateConfig{
		Enabled:             true,
		Mode:                eligibilityModeDryRun,
		ScopeSubscriptionID: scopeID,
		HighThreshold:       0.8,
		LowThreshold:        0.2,
		GatedChannels:       []string{channelID},
	}))
	client := &eligibilityGemini{}
	handler := NewIngestContentHandler(eligibilityDeps(pool, client))

	w := postJSON(t, handler, eligibilityRequest(channelID, "1800000045.000001", scopeText))
	if outcome := decodeIngestResponse(t, w.Body.Bytes()).Outcome; outcome != "created" {
		t.Fatalf("legacy-scope outcome = %q, want created", outcome)
	}
	var decisions int
	if err := pool.QueryRow(t.Context(), `
		SELECT COUNT(*) FROM graph.eligibility_decisions
		WHERE channel_id=$1 AND message_ts=$2 AND scope_version IS NOT NULL`,
		channelID, "1800000045.000001").Scan(&decisions); err != nil {
		t.Fatalf("count legacy-scope decisions: %v", err)
	}
	if decisions != 1 {
		t.Fatalf("legacy-scope decisions = %d, want 1", decisions)
	}
}

func TestEligibilityGateMalformedConfigIsOff(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)
	const channelID = "CELIGBADCFG"
	setEligibilityConfig(t, pool, `{"enabled":true,"mode":"enforce","high_threshold":0.2,"low_threshold":0.8}`)
	client := &eligibilityGemini{embed: func(text string) ([]float32, error) {
		return nil, fmt.Errorf("malformed config unexpectedly embedded %q", text)
	}}
	handler := NewIngestContentHandler(eligibilityDeps(pool, client))

	w := postJSON(t, handler, eligibilityRequest(channelID, "1800000050.000001", "off topic"))
	if outcome := decodeIngestResponse(t, w.Body.Bytes()).Outcome; outcome != "created" {
		t.Fatalf("malformed-config outcome = %q, want created", outcome)
	}
}

func TestEligibilityGateRejectsEqualThresholds(t *testing.T) {
	raw := eligibilityConfigJSON(t, eligibilityGateConfig{
		Enabled:             true,
		Mode:                eligibilityModeDryRun,
		ScopeSubscriptionID: 1,
		HighThreshold:       0.5,
		LowThreshold:        0.5,
		GatedChannels:       []string{},
		ExemptChannels:      []string{},
	})
	if _, err := decodeEligibilityGateConfig([]byte(raw)); err == nil {
		t.Fatal("equal thresholds must be rejected")
	}
}

func TestEligibilityGateValidatesGatedChannelsForMode(t *testing.T) {
	tests := []struct {
		name          string
		mode          string
		gatedChannels []string
		wantErr       string
	}{
		{
			name:    "enforce requires explicit channels",
			mode:    eligibilityModeEnforce,
			wantErr: "enforce requires a non-empty gated_channels list; an empty list means every channel",
		},
		{
			name:          "enforce accepts explicit channels",
			mode:          eligibilityModeEnforce,
			gatedChannels: []string{"CELIGIBILITY"},
		},
		{
			name: "dry run accepts every channel",
			mode: eligibilityModeDryRun,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateEligibilityGateConfig(eligibilityGateConfig{
				Mode:                tt.mode,
				ScopeSubscriptionID: 1,
				HighThreshold:       0.9,
				LowThreshold:        0.1,
				GatedChannels:       tt.gatedChannels,
			})
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validate eligibility gate config: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validation error = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadEligibilityGateTreatsEnforceWithoutGatedChannelsAsOff(t *testing.T) {
	pool := openTestDB(t)
	setEligibilityConfig(t, pool, eligibilityConfigJSON(t, eligibilityGateConfig{
		Enabled:             true,
		Mode:                eligibilityModeEnforce,
		ScopeSubscriptionID: 1,
		HighThreshold:       0.9,
		LowThreshold:        0.1,
		GatedChannels:       []string{},
		ExemptChannels:      []string{},
	}))

	cfg, err := loadEligibilityGate(t.Context(), pool)
	if err != nil {
		t.Fatalf("load eligibility gate: %v", err)
	}
	if cfg != nil {
		t.Fatalf("loaded eligibility gate = %#v, want nil", cfg)
	}
}

func TestEligibilityGateSequentialDuplicateReusesDecision(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)
	const (
		channelID = "CELIGDUP"
		scopeText = "payments duplicate scope"
		messageTS = "1800000060.000001"
		body      = "payments duplicate message"
	)
	scopeID := seedEligibilityScope(t, pool, scopeText)
	setEligibilityConfig(t, pool, eligibilityConfigJSON(t, eligibilityGateConfig{
		Enabled:             true,
		Mode:                eligibilityModeEnforce,
		ScopeSubscriptionID: scopeID,
		HighThreshold:       0.9,
		LowThreshold:        0.1,
		GatedChannels:       []string{channelID},
	}))
	client := &eligibilityGemini{embed: func(text string) ([]float32, error) {
		switch text {
		case scopeText, body:
			return []float32{1, 0}, nil
		default:
			return nil, fmt.Errorf("unexpected embed text %q", text)
		}
	}}
	handler := NewIngestContentHandler(eligibilityDeps(pool, client))

	first := postJSON(t, handler, eligibilityRequest(channelID, messageTS, body))
	if outcome := decodeIngestResponse(t, first.Body.Bytes()).Outcome; outcome != "created" {
		t.Fatalf("first duplicate outcome = %q, want created", outcome)
	}
	second := postJSON(t, handler, eligibilityRequest(channelID, messageTS, body))
	if outcome := decodeIngestResponse(t, second.Body.Bytes()).Outcome; outcome != "unchanged" {
		t.Fatalf("second duplicate outcome = %q, want unchanged", outcome)
	}

	if got := client.embedCallCount(body); got != 1 {
		t.Fatalf("message embed calls = %d, want 1", got)
	}
	var decisions int
	if err := pool.QueryRow(t.Context(), `
		SELECT COUNT(*) FROM graph.eligibility_decisions
		WHERE channel_id=$1 AND message_ts=$2 AND decision_source='scored'`,
		channelID, messageTS).Scan(&decisions); err != nil {
		t.Fatalf("count duplicate decisions: %v", err)
	}
	if decisions != 1 {
		t.Fatalf("duplicate audit rows = %d, want 1", decisions)
	}
}

func TestEligibilityGateCurrentDecisionUsesLatestRow(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)
	const (
		channelID = "CELIGLATEST"
		messageTS = "1800000061.000001"
	)
	scopeID := seedEligibilityScope(t, pool, "payments latest scope")
	scopeVersion := eligibilityScopeVersion(t, pool, scopeID)
	setEligibilityConfig(t, pool, eligibilityConfigJSON(t, eligibilityGateConfig{
		Enabled:             true,
		Mode:                eligibilityModeEnforce,
		ScopeSubscriptionID: scopeID,
		HighThreshold:       0.9,
		LowThreshold:        0.1,
		GatedChannels:       []string{channelID},
	}))
	now := time.Now()
	seedEligibilityDecision(t, pool, channelID, messageTS, 1.0, "eligible", "scored",
		eligibilityModeDryRun, scopeVersion, now.Add(-time.Second))
	seedEligibilityDecision(t, pool, channelID, messageTS, 0.0, "ineligible", "scored",
		eligibilityModeDryRun, scopeVersion, now)

	skip, err := eligibilityGateSkip(t.Context(), eligibilityDeps(pool, nil),
		channelID, messageTS, "", "must not embed")
	if err != nil {
		t.Fatalf("reuse latest current decision: %v", err)
	}
	if !skip {
		t.Fatal("latest current decision is ineligible under current enforce mode; want skip")
	}
	var decisions int
	if err := pool.QueryRow(t.Context(), `
		SELECT COUNT(*) FROM graph.eligibility_decisions WHERE channel_id=$1 AND message_ts=$2`,
		channelID, messageTS).Scan(&decisions); err != nil {
		t.Fatalf("count latest current decisions: %v", err)
	}
	if decisions != 2 {
		t.Fatalf("current decision reuse wrote an audit row: got %d rows, want 2", decisions)
	}
}

func TestEligibilityGateEligibleRootReplyInheritsWithoutScoring(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)
	const (
		channelID = "CELIGINHERIT"
		rootTS    = "1800000062.000001"
		replyTS   = "1800000062.000002"
		scopeText = "payments inherited root scope"
		body      = "reply that must inherit"
	)
	scopeID := seedEligibilityScope(t, pool, scopeText)
	scopeVersion := eligibilityScopeVersion(t, pool, scopeID)
	setEligibilityConfig(t, pool, eligibilityConfigJSON(t, eligibilityGateConfig{
		Enabled:             true,
		Mode:                eligibilityModeEnforce,
		ScopeSubscriptionID: scopeID,
		HighThreshold:       0.9,
		LowThreshold:        0.1,
		GatedChannels:       []string{channelID},
	}))
	seedEligibilityDecision(t, pool, channelID, rootTS, 1.0, "eligible", "scored",
		eligibilityModeDryRun, scopeVersion, time.Now())
	client := &eligibilityGemini{embed: func(text string) ([]float32, error) {
		return nil, fmt.Errorf("inherited reply unexpectedly embedded %q", text)
	}}
	handler := NewIngestContentHandler(eligibilityDeps(pool, client))

	w := postJSON(t, handler, eligibilityThreadRequest(channelID, replyTS, rootTS, body))
	if outcome := decodeIngestResponse(t, w.Body.Bytes()).Outcome; outcome != "created" {
		t.Fatalf("inherited reply outcome = %q, want created", outcome)
	}
	if got := client.embedCallCount(scopeText); got != 0 {
		t.Fatalf("inherited reply scope embed calls = %d, want 0", got)
	}
	if got := client.embedCallCount(body); got != 0 {
		t.Fatalf("inherited reply message embed calls = %d, want 0", got)
	}
	var decision, source, mode string
	var scoreIsNull bool
	var inheritedScopeVersion time.Time
	if err := pool.QueryRow(t.Context(), `
		SELECT decision, decision_source, score IS NULL, mode, scope_version
		FROM graph.eligibility_decisions
		WHERE channel_id=$1 AND message_ts=$2
		ORDER BY decided_at DESC, id DESC LIMIT 1`,
		channelID, replyTS).Scan(&decision, &source, &scoreIsNull, &mode, &inheritedScopeVersion); err != nil {
		t.Fatalf("read inherited decision: %v", err)
	}
	if decision != "eligible" || source != "inherited_root" || !scoreIsNull ||
		mode != eligibilityModeEnforce || !inheritedScopeVersion.Equal(scopeVersion) {
		t.Fatalf("inherited audit = decision %q source %q score_null %v mode %q scope %v, want eligible inherited_root true enforce %v",
			decision, source, scoreIsNull, mode, inheritedScopeVersion, scopeVersion)
	}
}

func TestEligibilityGateMissingRootFallsThroughToScoring(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)
	const (
		channelID = "CELIGMISSINGROOT"
		rootTS    = "1800000063.000001"
		replyTS   = "1800000063.000002"
		scopeText = "payments missing root scope"
		body      = "payments reply with missing root"
	)
	scopeID := seedEligibilityScope(t, pool, scopeText)
	setEligibilityConfig(t, pool, eligibilityConfigJSON(t, eligibilityGateConfig{
		Enabled:             true,
		Mode:                eligibilityModeEnforce,
		ScopeSubscriptionID: scopeID,
		HighThreshold:       0.9,
		LowThreshold:        0.1,
		GatedChannels:       []string{channelID},
	}))
	client := &eligibilityGemini{embed: func(text string) ([]float32, error) {
		switch text {
		case scopeText, body:
			return []float32{1, 0}, nil
		default:
			return nil, fmt.Errorf("unexpected embed text %q", text)
		}
	}}
	handler := NewIngestContentHandler(eligibilityDeps(pool, client))

	w := postJSON(t, handler, eligibilityThreadRequest(channelID, replyTS, rootTS, body))
	if outcome := decodeIngestResponse(t, w.Body.Bytes()).Outcome; outcome != "created" {
		t.Fatalf("missing-root reply outcome = %q, want created", outcome)
	}
	if got := client.embedCallCount(scopeText); got != 1 {
		t.Fatalf("missing-root scope embed calls = %d, want 1", got)
	}
	if got := client.embedCallCount(body); got != 1 {
		t.Fatalf("missing-root message embed calls = %d, want 1", got)
	}
	var source string
	var scoreIsNull bool
	if err := pool.QueryRow(t.Context(), `
		SELECT decision_source, score IS NULL FROM graph.eligibility_decisions
		WHERE channel_id=$1 AND message_ts=$2`,
		channelID, replyTS).Scan(&source, &scoreIsNull); err != nil {
		t.Fatalf("read missing-root decision: %v", err)
	}
	if source != "scored" || scoreIsNull {
		t.Fatalf("missing-root decision source = %q score_null = %v, want scored false", source, scoreIsNull)
	}
}

func TestEligibilityGateLatestIneligibleRootFallsThroughAndCanScoreEligible(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)
	const (
		channelID = "CELIGINELIGIBLEROOT"
		rootTS    = "1800000064.000001"
		replyTS   = "1800000064.000002"
		scopeText = "payments ineligible root scope"
		body      = "payments reply that is independently eligible"
	)
	scopeID := seedEligibilityScope(t, pool, scopeText)
	scopeVersion := eligibilityScopeVersion(t, pool, scopeID)
	setEligibilityConfig(t, pool, eligibilityConfigJSON(t, eligibilityGateConfig{
		Enabled:             true,
		Mode:                eligibilityModeEnforce,
		ScopeSubscriptionID: scopeID,
		HighThreshold:       0.9,
		LowThreshold:        0.1,
		GatedChannels:       []string{channelID},
	}))
	now := time.Now()
	seedEligibilityDecision(t, pool, channelID, rootTS, 1.0, "eligible", "scored",
		eligibilityModeDryRun, scopeVersion, now.Add(-time.Second))
	seedEligibilityDecision(t, pool, channelID, rootTS, 0.0, "ineligible", "scored",
		eligibilityModeDryRun, scopeVersion, now)
	client := &eligibilityGemini{embed: func(text string) ([]float32, error) {
		switch text {
		case scopeText, body:
			return []float32{1, 0}, nil
		default:
			return nil, fmt.Errorf("unexpected embed text %q", text)
		}
	}}
	handler := NewIngestContentHandler(eligibilityDeps(pool, client))

	w := postJSON(t, handler, eligibilityThreadRequest(channelID, replyTS, rootTS, body))
	if outcome := decodeIngestResponse(t, w.Body.Bytes()).Outcome; outcome != "created" {
		t.Fatalf("ineligible-root reply outcome = %q, want created", outcome)
	}
	var decision, source string
	if err := pool.QueryRow(t.Context(), `
		SELECT decision, decision_source FROM graph.eligibility_decisions
		WHERE channel_id=$1 AND message_ts=$2`,
		channelID, replyTS).Scan(&decision, &source); err != nil {
		t.Fatalf("read independently scored reply: %v", err)
	}
	if decision != "eligible" || source != "scored" {
		t.Fatalf("independently scored reply = %q source %q, want eligible scored", decision, source)
	}
}

func TestEligibilityGateNewLookupErrorsFailOpen(t *testing.T) {
	tests := []struct {
		name                string
		allowedAcquisitions int
		threadTS            string
		wantContext         string
	}{
		{
			name:                "current message decision lookup",
			allowedAcquisitions: 0,
			wantContext:         "lookup current decision",
		},
		{
			name:                "eligible root decision lookup",
			allowedAcquisitions: 1,
			threadTS:            "1800000065.000001",
			wantContext:         "lookup eligible root decision",
		},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool := openTestDB(t)
			truncateGraphHandlerTables(t, pool)
			channelID := fmt.Sprintf("CELIGLOOKUPERR%d", i)
			scopeID := seedEligibilityScope(t, pool, "lookup error scope")
			setEligibilityConfig(t, pool, eligibilityConfigJSON(t, eligibilityGateConfig{
				Enabled:             true,
				Mode:                eligibilityModeEnforce,
				ScopeSubscriptionID: scopeID,
				HighThreshold:       0.9,
				LowThreshold:        0.1,
				GatedChannels:       []string{channelID},
			}))
			if _, err := loadEligibilityGate(t.Context(), pool); err != nil {
				t.Fatalf("prime eligibility config cache: %v", err)
			}
			errorPool := eligibilityLookupErrorPool(t, tt.allowedAcquisitions)
			ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
			defer cancel()

			skip, err := eligibilityGateSkip(ctx, eligibilityDeps(errorPool, nil),
				channelID, fmt.Sprintf("1800000065.%06d", i+2), tt.threadTS, "must fail open")
			if skip {
				t.Fatal("lookup error skip = true, want false")
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantContext) {
				t.Fatalf("lookup error = %v, want context %q", err, tt.wantContext)
			}
		})
	}
}

// clearAlertFingerprints removes any leftover fingerprint rows for channelID
// so alert-bot tests are idempotent across repeated runs (truncateGraphHandlerTables
// does not cover these tables).
func clearAlertFingerprints(t *testing.T, pool *pgxpool.Pool, channelID string) {
	t.Helper()
	for _, tbl := range []string{"graph.alert_fingerprint_events", "graph.alert_fingerprints"} {
		if _, err := pool.Exec(t.Context(), `DELETE FROM `+tbl+` WHERE channel_id=$1`, channelID); err != nil {
			t.Fatalf("clear %s: %v", tbl, err)
		}
	}
	t.Cleanup(func() {
		for _, tbl := range []string{"graph.alert_fingerprint_events", "graph.alert_fingerprints"} {
			_, _ = pool.Exec(context.Background(), `DELETE FROM `+tbl+` WHERE channel_id=$1`, channelID)
		}
	})
}

// seedAlertChannel registers a slack channel row with the given (alert-shaped
// or not) name so decideAlertBot's channelIsAlert lookup resolves it.
func seedAlertChannel(t *testing.T, pool *pgxpool.Pool, channelID, name string) {
	t.Helper()
	if _, err := pool.Exec(t.Context(), `
		INSERT INTO graph.slack_channels (slack_channel_id, name, machine_id)
		VALUES ($1, $2, 'eligibility-test')
		ON CONFLICT (slack_channel_id) DO UPDATE SET name = EXCLUDED.name`,
		channelID, name); err != nil {
		t.Fatalf("seed alert channel: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM graph.slack_channels WHERE slack_channel_id=$1`, channelID)
	})
}

// botEligibilityRequest returns an eligibility request whose author is a bot.
func botEligibilityRequest(channelID, ts, body string) map[string]any {
	req := eligibilityRequest(channelID, ts, body)
	req["metadata"].(map[string]any)["author"] = map[string]any{
		"display_name": "AlertBot",
		"is_bot":       true,
	}
	return req
}

// TestEligibilityGateEmptyBodyIsProcessedWithoutEmbeddingOrAudit covers the
// empty-body guard (agent-mem-8nx0): a Slack message with only blocks or
// attachments has no text, so the gate must fall through as eligible without
// embedding anything and without writing an audit row. The message itself is
// still ingested (fail-open direction).
func TestEligibilityGateEmptyBodyIsProcessedWithoutEmbeddingOrAudit(t *testing.T) {
	for name, body := range map[string]string{
		"empty body":      "",
		"whitespace only": " \n\t  ",
	} {
		t.Run(name, func(t *testing.T) {
			pool := openTestDB(t)
			truncateGraphHandlerTables(t, pool)
			const (
				channelID = "CELIGEMPTY"
				scopeText = "payments checkout scope"
			)
			scopeID := seedEligibilityScope(t, pool, scopeText)
			setEligibilityConfig(t, pool, eligibilityConfigJSON(t, eligibilityGateConfig{
				Enabled:             true,
				Mode:                eligibilityModeDryRun,
				ScopeSubscriptionID: scopeID,
				HighThreshold:       1,
				LowThreshold:        0,
				GatedChannels:       []string{channelID},
			}))

			client := &eligibilityGemini{}
			handler := NewIngestContentHandler(eligibilityDeps(pool, client))

			w := postJSON(t, handler, eligibilityRequest(channelID, "1800000000.000001", body))
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
			}
			if outcome := decodeIngestResponse(t, w.Body.Bytes()).Outcome; outcome != "created" {
				t.Fatalf("outcome = %q, want created (empty body is not a failure)", outcome)
			}

			if got := client.embedCallCount(body); got != 0 {
				t.Fatalf("body embed calls = %d, want 0 (empty body must never reach /embed)", got)
			}
			if got := client.embedCallCount(scopeText); got != 0 {
				t.Fatalf("scope embed calls = %d, want 0 (guard runs before scope load)", got)
			}
			var decisions int
			if err := pool.QueryRow(t.Context(),
				`SELECT COUNT(*) FROM graph.eligibility_decisions WHERE channel_id=$1`, channelID).
				Scan(&decisions); err != nil {
				t.Fatalf("count decisions: %v", err)
			}
			if decisions != 0 {
				t.Fatalf("eligibility decisions = %d, want 0 (no audit row for empty body)", decisions)
			}
			var nodes int
			if err := pool.QueryRow(t.Context(),
				`SELECT COUNT(*) FROM graph.nodes WHERE scope=$1`, "slack:"+channelID).Scan(&nodes); err != nil {
				t.Fatalf("count nodes: %v", err)
			}
			if nodes != 1 {
				t.Fatalf("nodes = %d, want 1 (message must still be processed)", nodes)
			}
		})
	}
}

// TestIngestAlertBotRepeatSkipsGateWithoutScoring covers the reorder
// (agent-mem-hzu8): a repeated alert template in an alert-named channel must
// be fingerprinted away BEFORE the eligibility gate pays for an embed, so the
// second message produces no eligibility_decisions row and no node. A novel
// template still reaches the gate and is scored exactly once.
func TestIngestAlertBotRepeatSkipsGateWithoutScoring(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)
	const (
		channelID = "CALERTREORDER"
		scopeText = "payments scope"
	)
	clearAlertFingerprints(t, pool, channelID)
	seedAlertChannel(t, pool, channelID, "reorder-alerts")
	scopeID := seedEligibilityScope(t, pool, scopeText)
	setEligibilityConfig(t, pool, eligibilityConfigJSON(t, eligibilityGateConfig{
		Enabled:             true,
		Mode:                eligibilityModeDryRun,
		ScopeSubscriptionID: scopeID,
		HighThreshold:       1,
		LowThreshold:        0,
		GatedChannels:       []string{channelID},
	}))

	client := &eligibilityGemini{}
	handler := NewIngestContentHandler(eligibilityDeps(pool, client))

	// First sighting: novel fingerprint escalates, so the message legitimately
	// reaches the gate and is scored once.
	novel := "PaymentFailed order 123456 amount 50.00 at 2026-08-24T10:11:12Z"
	w := postJSON(t, handler, botEligibilityRequest(channelID, "1800000000.000001", novel))
	if w.Code != http.StatusOK {
		t.Fatalf("novel: status = %d, body = %s", w.Code, w.Body.String())
	}
	if outcome := decodeIngestResponse(t, w.Body.Bytes()).Outcome; outcome != "created" {
		t.Fatalf("novel outcome = %q, want created", outcome)
	}

	// Second sighting: same template, different volatile values — the
	// fingerprint is known, so it must be discarded before the gate runs.
	repeat := "PaymentFailed order 999999 amount 70.00 at 2026-08-24T11:12:13Z"
	w = postJSON(t, handler, botEligibilityRequest(channelID, "1800000000.000002", repeat))
	if w.Code != http.StatusOK {
		t.Fatalf("repeat: status = %d, body = %s", w.Code, w.Body.String())
	}
	if outcome := decodeIngestResponse(t, w.Body.Bytes()).Outcome; outcome != "alert_fingerprinted" {
		t.Fatalf("repeat outcome = %q, want alert_fingerprinted", outcome)
	}

	if got := client.embedCallCount(repeat); got != 0 {
		t.Fatalf("repeat body embed calls = %d, want 0 (fingerprinting must precede the gate)", got)
	}
	var decisions int
	if err := pool.QueryRow(t.Context(),
		`SELECT COUNT(*) FROM graph.eligibility_decisions WHERE channel_id=$1`, channelID).
		Scan(&decisions); err != nil {
		t.Fatalf("count decisions: %v", err)
	}
	if decisions != 1 {
		t.Fatalf("eligibility decisions = %d, want 1 (only the novel template is scored)", decisions)
	}
	var nodes int
	if err := pool.QueryRow(t.Context(),
		`SELECT COUNT(*) FROM graph.nodes WHERE scope=$1`, "slack:"+channelID).Scan(&nodes); err != nil {
		t.Fatalf("count nodes: %v", err)
	}
	if nodes != 1 {
		t.Fatalf("nodes = %d, want 1 (repeat must not produce a node)", nodes)
	}
}

// TestIngestHumanMessageInAlertChannelStillScored pins the no-behaviour-change
// half of the reorder: a non-automated message in an alert-named channel is
// unaffected — decideAlertBot returns Skip=false after one indexed read, the
// gate still scores it, and no alert fingerprint is recorded.
func TestIngestHumanMessageInAlertChannelStillScored(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)
	const (
		channelID = "CALERTHUMAN"
		scopeText = "payments human scope"
	)
	clearAlertFingerprints(t, pool, channelID)
	seedAlertChannel(t, pool, channelID, "payments-alerts")
	scopeID := seedEligibilityScope(t, pool, scopeText)
	setEligibilityConfig(t, pool, eligibilityConfigJSON(t, eligibilityGateConfig{
		Enabled:             true,
		Mode:                eligibilityModeDryRun,
		ScopeSubscriptionID: scopeID,
		HighThreshold:       1,
		LowThreshold:        0,
		GatedChannels:       []string{channelID},
	}))

	client := &eligibilityGemini{}
	handler := NewIngestContentHandler(eligibilityDeps(pool, client))

	body := "hey team, the checkout deploy is green — human message in an alert channel"
	w := postJSON(t, handler, eligibilityRequest(channelID, "1800000000.000001", body))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if outcome := decodeIngestResponse(t, w.Body.Bytes()).Outcome; outcome != "created" {
		t.Fatalf("outcome = %q, want created", outcome)
	}

	if got := client.embedCallCount(body); got != 1 {
		t.Fatalf("body embed calls = %d, want 1 (gate still scores human messages)", got)
	}
	var scored int
	if err := pool.QueryRow(t.Context(), `
		SELECT COUNT(*) FROM graph.eligibility_decisions
		WHERE channel_id=$1 AND decision_source='scored'`, channelID).Scan(&scored); err != nil {
		t.Fatalf("count scored decisions: %v", err)
	}
	if scored != 1 {
		t.Fatalf("scored decisions = %d, want 1", scored)
	}
	var fingerprints int
	if err := pool.QueryRow(t.Context(),
		`SELECT COUNT(*) FROM graph.alert_fingerprints WHERE channel_id=$1`, channelID).
		Scan(&fingerprints); err != nil {
		t.Fatalf("count fingerprints: %v", err)
	}
	if fingerprints != 0 {
		t.Fatalf("alert fingerprints = %d, want 0 (non-automated messages are never fingerprinted)", fingerprints)
	}
}
