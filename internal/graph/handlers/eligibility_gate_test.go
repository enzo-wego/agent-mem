package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sync"
	"testing"

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
