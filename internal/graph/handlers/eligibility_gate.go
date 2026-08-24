package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	eligibilityGateKey               = "graph.eligibility_gate"
	eligibilityModeDryRun            = "dry_run"
	eligibilityModeEnforce           = "enforce"
	eligibilitySkippedOutcome        = "skipped_off_topic"
	eligibilityDecisionScored        = "scored"
	eligibilityDecisionInheritedRoot = "inherited_root"
	eligibilityGateTTL               = 60 * time.Second
)

type eligibilityGateConfig struct {
	Enabled             bool     `json:"enabled"`
	Mode                string   `json:"mode"`
	ScopeSubscriptionID int64    `json:"scope_subscription_id"`
	HighThreshold       float64  `json:"high_threshold"`
	LowThreshold        float64  `json:"low_threshold"`
	LLMAdjudicate       bool     `json:"llm_adjudicate"`
	GatedChannels       []string `json:"gated_channels"`
	ExemptChannels      []string `json:"exempt_channels"`
}

func (cfg eligibilityGateConfig) MarshalJSON() ([]byte, error) {
	type configJSON eligibilityGateConfig
	if cfg.GatedChannels == nil {
		cfg.GatedChannels = []string{}
	}
	if cfg.ExemptChannels == nil {
		cfg.ExemptChannels = []string{}
	}
	return json.Marshal(configJSON(cfg))
}

type eligibilityScopeKey struct {
	subscriptionID int64
	refreshedAt    time.Time
}

type eligibilityScope struct {
	definition string
	embedding  []float32
}

var (
	eligibilityConfigMu       sync.Mutex
	eligibilityConfigCache    *eligibilityGateConfig
	eligibilityConfigLoaded   bool
	eligibilityConfigLoadedAt time.Time

	eligibilityScopeMu    sync.Mutex
	eligibilityScopeCache = make(map[eligibilityScopeKey]eligibilityScope)
)

func invalidateEligibilityGate() {
	eligibilityConfigMu.Lock()
	eligibilityConfigCache = nil
	eligibilityConfigLoaded = false
	eligibilityConfigMu.Unlock()
}

func loadEligibilityGate(ctx context.Context, db *pgxpool.Pool) (*eligibilityGateConfig, error) {
	eligibilityConfigMu.Lock()
	defer eligibilityConfigMu.Unlock()

	if eligibilityConfigLoaded && time.Since(eligibilityConfigLoadedAt) < eligibilityGateTTL {
		return eligibilityConfigCache, nil
	}
	if db == nil {
		eligibilityConfigCache = nil
		eligibilityConfigLoaded = true
		eligibilityConfigLoadedAt = time.Now()
		return nil, nil
	}

	var raw string
	err := db.QueryRow(ctx, `SELECT value FROM settings WHERE key=$1`, eligibilityGateKey).Scan(&raw)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("load eligibility gate config: %w", err)
	}
	eligibilityConfigCache = nil
	eligibilityConfigLoaded = true
	eligibilityConfigLoadedAt = time.Now()
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	cfg, err := decodeEligibilityGateConfig([]byte(raw))
	if err != nil {
		return nil, nil
	}
	eligibilityConfigCache = &cfg
	return eligibilityConfigCache, nil
}

func decodeEligibilityGateConfig(raw []byte) (eligibilityGateConfig, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return eligibilityGateConfig{}, fmt.Errorf("decode eligibility gate config: %w", err)
	}
	required := [...]string{
		"enabled",
		"mode",
		"scope_subscription_id",
		"high_threshold",
		"low_threshold",
		"llm_adjudicate",
		"gated_channels",
		"exempt_channels",
	}
	if len(fields) != len(required) {
		return eligibilityGateConfig{}, errors.New("eligibility gate config must contain exactly the supported fields")
	}
	for _, name := range required {
		if _, ok := fields[name]; !ok {
			return eligibilityGateConfig{}, fmt.Errorf("eligibility gate config missing %q", name)
		}
	}

	var cfg eligibilityGateConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return eligibilityGateConfig{}, fmt.Errorf("decode eligibility gate config: %w", err)
	}
	if cfg.GatedChannels == nil || cfg.ExemptChannels == nil {
		return eligibilityGateConfig{}, errors.New("gated_channels and exempt_channels must be arrays")
	}
	if err := validateEligibilityGateConfig(cfg); err != nil {
		return eligibilityGateConfig{}, err
	}
	return cfg, nil
}

func validateEligibilityGateConfig(cfg eligibilityGateConfig) error {
	if cfg.Mode != eligibilityModeDryRun && cfg.Mode != eligibilityModeEnforce {
		return fmt.Errorf("mode must be %q or %q", eligibilityModeDryRun, eligibilityModeEnforce)
	}
	if cfg.ScopeSubscriptionID <= 0 {
		return errors.New("scope_subscription_id must be positive")
	}
	if cfg.LowThreshold < 0 || cfg.LowThreshold > 1 {
		return errors.New("low_threshold must be between 0 and 1")
	}
	if cfg.HighThreshold < 0 || cfg.HighThreshold > 1 {
		return errors.New("high_threshold must be between 0 and 1")
	}
	if cfg.LowThreshold >= cfg.HighThreshold {
		return errors.New("low_threshold must be lower than high_threshold")
	}

	for _, id := range cfg.GatedChannels {
		if strings.TrimSpace(id) == "" {
			return errors.New("gated_channels must not contain empty channel IDs")
		}
	}
	for _, id := range cfg.ExemptChannels {
		if strings.TrimSpace(id) == "" {
			return errors.New("exempt_channels must not contain empty channel IDs")
		}
	}
	return nil
}

func eligibilityGateApplies(cfg *eligibilityGateConfig, channelID string) bool {
	if cfg == nil || !cfg.Enabled || channelID == "" {
		return false
	}
	for _, id := range cfg.ExemptChannels {
		if id == channelID {
			return false
		}
	}
	if len(cfg.GatedChannels) == 0 {
		return true
	}
	for _, id := range cfg.GatedChannels {
		if id == channelID {
			return true
		}
	}
	return false
}

type eligibilityDecision struct {
	decision     string
	scopeVersion time.Time
}

func loadLatestEligibilityDecision(
	ctx context.Context,
	db *pgxpool.Pool,
	channelID, messageTS string,
) (eligibilityDecision, bool, error) {
	var decision eligibilityDecision
	err := db.QueryRow(ctx, `
		SELECT decision, scope_version
		FROM graph.eligibility_decisions
		WHERE channel_id=$1 AND message_ts=$2
		ORDER BY decided_at DESC, id DESC
		LIMIT 1`,
		channelID, messageTS).Scan(&decision.decision, &decision.scopeVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return eligibilityDecision{}, false, nil
	}
	if err != nil {
		return eligibilityDecision{}, false, err
	}
	return decision, true, nil
}

func eligibilityGateSkip(ctx context.Context, deps Deps, channelID, messageTS, threadTS, body string) (bool, error) {
	cfg, err := loadEligibilityGate(ctx, deps.DB)
	if err != nil {
		return false, err
	}
	if !eligibilityGateApplies(cfg, channelID) {
		return false, nil
	}
	currentDecision, found, err := loadLatestEligibilityDecision(ctx, deps.DB, channelID, messageTS)
	if err != nil {
		return false, fmt.Errorf("eligibility gate: lookup current decision: %w", err)
	}
	if found {
		return cfg.Mode == eligibilityModeEnforce && currentDecision.decision == "ineligible", nil
	}

	if threadTS != "" && threadTS != messageTS {
		rootDecision, found, err := loadLatestEligibilityDecision(ctx, deps.DB, channelID, threadTS)
		if err != nil {
			return false, fmt.Errorf("eligibility gate: lookup eligible root decision: %w", err)
		}
		if found && rootDecision.decision == "eligible" {
			if _, err := deps.DB.Exec(ctx, `
				INSERT INTO graph.eligibility_decisions
				  (channel_id, message_ts, score, decision, decision_source, mode, scope_version, decided_at)
				VALUES ($1,$2,NULL,'eligible',$3,$4,$5,NOW())`,
				channelID, messageTS, eligibilityDecisionInheritedRoot, cfg.Mode, rootDecision.scopeVersion); err != nil {
				return false, fmt.Errorf("eligibility gate: audit inherited root decision: %w", err)
			}
			return false, nil
		}
	}
	if deps.Gemini == nil {
		return false, errors.New("eligibility gate: nil LLM client")
	}

	scope, scopeVersion, err := loadEligibilityScope(ctx, deps.DB, deps.Gemini, cfg.ScopeSubscriptionID)
	if err != nil {
		return false, err
	}
	messageEmbedding, err := deps.Gemini.EmbedWithOptions(ctx, body, graphEmbeddingOptions())
	if err != nil {
		return false, fmt.Errorf("eligibility gate: embed message: %w", err)
	}
	score, err := cosineSimilarity(scope.embedding, messageEmbedding)
	if err != nil {
		return false, fmt.Errorf("eligibility gate: compare embeddings: %w", err)
	}

	decision := "eligible"
	var adjudicationErr error
	switch {
	case score >= cfg.HighThreshold:
	case score <= cfg.LowThreshold:
		decision = "ineligible"
	case cfg.LLMAdjudicate:
		decision, adjudicationErr = adjudicateEligibility(ctx, deps.Gemini, scope.definition, body)
		if adjudicationErr != nil {
			decision = "eligible"
		}
	}

	if _, err := deps.DB.Exec(ctx, `
		INSERT INTO graph.eligibility_decisions
		  (channel_id, message_ts, score, decision, decision_source, mode, scope_version, decided_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,NOW())`,
		channelID, messageTS, score, decision, eligibilityDecisionScored, cfg.Mode, scopeVersion); err != nil {
		return false, fmt.Errorf("eligibility gate: audit decision: %w", err)
	}
	if adjudicationErr != nil {
		return false, adjudicationErr
	}
	return cfg.Mode == eligibilityModeEnforce && decision == "ineligible", nil
}

func loadEligibilityScope(ctx context.Context, db *pgxpool.Pool, client GeminiClient, subscriptionID int64) (eligibilityScope, time.Time, error) {
	if db == nil {
		return eligibilityScope{}, time.Time{}, errors.New("eligibility gate: nil database")
	}

	if err := ctx.Err(); err != nil {
		return eligibilityScope{}, time.Time{}, fmt.Errorf("eligibility gate: load scope: %w", err)
	}

	var definition string
	var refreshedAt time.Time
	if err := db.QueryRow(ctx, `
		SELECT scope_definition, COALESCE(scope_refreshed_at, created_at)
		FROM graph.topic_subscriptions
		WHERE id=$1`, subscriptionID).Scan(&definition, &refreshedAt); err != nil {
		return eligibilityScope{}, time.Time{}, fmt.Errorf("eligibility gate: load scope subscription %d: %w", subscriptionID, err)
	}
	definition = strings.TrimSpace(definition)
	if definition == "" {
		return eligibilityScope{}, time.Time{}, fmt.Errorf("eligibility gate: scope subscription %d has no scope definition", subscriptionID)
	}

	key := eligibilityScopeKey{subscriptionID: subscriptionID, refreshedAt: refreshedAt}
	eligibilityScopeMu.Lock()
	defer eligibilityScopeMu.Unlock()
	if scope, ok := eligibilityScopeCache[key]; ok {
		return scope, refreshedAt, nil
	}
	embedding, err := client.EmbedWithOptions(ctx, definition, graphEmbeddingOptions())
	if err != nil {
		return eligibilityScope{}, time.Time{}, fmt.Errorf("eligibility gate: embed scope subscription %d: %w", subscriptionID, err)
	}
	if len(embedding) == 0 {
		return eligibilityScope{}, time.Time{}, fmt.Errorf("eligibility gate: scope subscription %d returned an empty embedding", subscriptionID)
	}
	for existing := range eligibilityScopeCache {
		if existing.subscriptionID == subscriptionID {
			delete(eligibilityScopeCache, existing)
		}
	}
	scope := eligibilityScope{definition: definition, embedding: embedding}
	eligibilityScopeCache[key] = scope
	return scope, refreshedAt, nil
}

func cosineSimilarity(a, b []float32) (float64, error) {
	if len(a) == 0 || len(a) != len(b) {
		return 0, fmt.Errorf("embedding dimensions differ: %d and %d", len(a), len(b))
	}
	var dot, normA, normB float64
	for i, av32 := range a {
		av := float64(av32)
		bv := float64(b[i])
		dot += av * bv
		normA += av * av
		normB += bv * bv
	}
	if normA == 0 || normB == 0 {
		return 0, errors.New("embedding has zero magnitude")
	}
	score := dot / math.Sqrt(normA*normB)
	if math.IsNaN(score) || math.IsInf(score, 0) {
		return 0, errors.New("embedding similarity is not finite")
	}
	if score > 1 {
		return 1, nil
	}
	if score < -1 {
		return -1, nil
	}
	return score, nil
}

func adjudicateEligibility(ctx context.Context, client GeminiClient, scope, message string) (string, error) {
	const systemPrompt = "Decide whether a Slack message is relevant to the supplied scope. Reply with exactly yes or no, with no explanation."
	userPrompt := "Scope:\n" + scope + "\n\nSlack message:\n" + message
	out, err := client.GenerateCheap(ctx, systemPrompt, userPrompt)
	if err != nil {
		return "", fmt.Errorf("eligibility gate: adjudicate uncertain message: %w", err)
	}
	switch strings.ToLower(strings.TrimSpace(out)) {
	case "yes":
		return "eligible", nil
	case "no":
		return "ineligible", nil
	default:
		return "", fmt.Errorf("eligibility gate: adjudication returned %q instead of yes or no", out)
	}
}

func (h *Channels) getEligibilityGate(w http.ResponseWriter, r *http.Request) {
	var value string
	err := h.db.QueryRow(r.Context(), `SELECT value FROM settings WHERE key=$1`, eligibilityGateKey).Scan(&value)
	if errors.Is(err, pgx.ErrNoRows) || value == "" {
		value = "{}"
	} else if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, value)
}

func (h *Channels) putEligibilityGate(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read body")
		return
	}
	cfg, err := decodeEligibilityGateConfig(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	normalized, err := json.Marshal(cfg)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if _, err := h.db.Exec(r.Context(), `
		INSERT INTO settings(key,value) VALUES($1,$2)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, eligibilityGateKey, string(normalized)); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	invalidateEligibilityGate()
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(normalized)
}
