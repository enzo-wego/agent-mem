package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/agent-mem/agent-mem/internal/anthropic"
	"github.com/agent-mem/agent-mem/internal/config"
	"github.com/agent-mem/agent-mem/internal/database"
	"github.com/agent-mem/agent-mem/internal/gemini"
	graphhandlers "github.com/agent-mem/agent-mem/internal/graph/handlers"
	"github.com/agent-mem/agent-mem/internal/search"
)

// settingsResponse is the JSON shape returned by GET /api/settings.
// GeminiAPIKey and DatabaseURL are masked for security.
type settingsResponse struct {
	WorkerPort  int    `json:"worker_port"`
	DataDir     string `json:"data_dir"`
	LogLevel    string `json:"log_level"`
	DatabaseURL string `json:"database_url"`

	GeminiAPIKey         string `json:"gemini_api_key"`
	GeminiModel          string `json:"gemini_model"`
	GraphGeminiModel     string `json:"graph_gemini_model"`
	GeminiEmbeddingModel string `json:"gemini_embedding_model"`
	GeminiEmbeddingDims  int    `json:"gemini_embedding_dims"`
	LLMProvider          string `json:"llm_provider"`
	GoogleAPIKey         string `json:"google_api_key"`
	GoogleAPIKeys        string `json:"google_api_keys"`
	LLMKeyRotateHours    int    `json:"llm_key_rotate_hours"`
	AnthropicAPIKey      string `json:"anthropic_api_key"`
	AnthropicModel       string `json:"anthropic_model"`

	ContextObservations int    `json:"context_observations"`
	ContextFullCount    int    `json:"context_full_count"`
	ContextSessionCount int    `json:"context_session_count"`
	SkipTools           string `json:"skip_tools"`

	AllowedProjects string `json:"allowed_projects"`
	IgnoredProjects string `json:"ignored_projects"`

	SyncEnabled  bool   `json:"sync_enabled"`
	SyncURL      string `json:"sync_url"`
	SyncInterval string `json:"sync_interval"`
	MachineID    string `json:"machine_id"`
}

func maskKey(key string) string {
	if len(key) <= 4 {
		if key == "" {
			return ""
		}
		return "****"
	}
	return strings.Repeat("*", len(key)-4) + key[len(key)-4:]
}

// maskKeyList masks each key in a pool, one per line — the shape the dashboard
// shows as a placeholder for the keys textarea.
func maskKeyList(keys string) string {
	parsed := config.SplitKeys(keys)
	masked := make([]string, 0, len(parsed))
	for _, k := range parsed {
		masked = append(masked, maskKey(k))
	}
	return strings.Join(masked, "\n")
}

func (s *Server) handleGetSettings(w http.ResponseWriter, _ *http.Request) {
	snap := s.config.Snapshot()
	resp := settingsResponse{
		WorkerPort:           snap.WorkerPort,
		DataDir:              snap.DataDir,
		LogLevel:             snap.LogLevel,
		DatabaseURL:          maskKey(snap.DatabaseURL),
		GeminiAPIKey:         maskKey(snap.GeminiAPIKey),
		GeminiModel:          snap.GeminiModel,
		GraphGeminiModel:     snap.GraphGeminiModel,
		GeminiEmbeddingModel: snap.GeminiEmbeddingModel,
		GeminiEmbeddingDims:  snap.GeminiEmbeddingDims,
		LLMProvider:          snap.LLMProviderOrDefault(),
		GoogleAPIKey:         maskKey(snap.GoogleAPIKey),
		GoogleAPIKeys:        maskKeyList(snap.GoogleAPIKeys),
		LLMKeyRotateHours:    snap.LLMKeyRotateHours,
		AnthropicAPIKey:      maskKey(snap.AnthropicAPIKey),
		AnthropicModel:       snap.AnthropicModel,
		ContextObservations:  snap.ContextObservations,
		ContextFullCount:     snap.ContextFullCount,
		ContextSessionCount:  snap.ContextSessionCount,
		SkipTools:            snap.SkipTools,
		AllowedProjects:      snap.AllowedProjects,
		IgnoredProjects:      snap.IgnoredProjects,
		SyncEnabled:          snap.SyncEnabled,
		SyncURL:              snap.SyncURL,
		SyncInterval:         snap.SyncInterval,
		MachineID:            snap.MachineID,
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Error().Err(err).Msg("Failed to encode settings response")
	}
}

func (s *Server) handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	// Limit request body to 64 KB.
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)

	var partial map[string]any
	if err := json.NewDecoder(r.Body).Decode(&partial); err != nil {
		http.Error(w, `{"error":"invalid JSON"}`, http.StatusBadRequest)
		return
	}

	// Reject attempts to change restart-required fields.
	for _, blocked := range []string{"worker_port", "data_dir", "database_url"} {
		if _, ok := partial[blocked]; ok {
			resp, _ := json.Marshal(map[string]string{"error": blocked + " requires a restart to change"})
			http.Error(w, string(resp), http.StatusBadRequest)
			return
		}
	}

	geminiChanged := s.config.Update(partial)

	// Persist runtime settings to PostgreSQL.
	if err := s.db.SaveSettings(r.Context(), s.config.RuntimeSettings()); err != nil {
		log.Error().Err(err).Msg("Failed to save settings to database")
		http.Error(w, `{"error":"failed to persist settings"}`, http.StatusInternalServerError)
		return
	}

	// Apply side-effects.
	if geminiChanged {
		s.reloadGemini()
	}
	if _, ok := partial["log_level"]; ok {
		snap := s.config.Snapshot()
		lvl, err := zerolog.ParseLevel(snap.LogLevel)
		if err == nil {
			zerolog.SetGlobalLevel(lvl)
			log.Info().Str("level", lvl.String()).Msg("Log level updated")
		}
	}

	log.Info().Interface("updated_keys", keys(partial)).Msg("Settings updated")

	// Return the full current settings (including masked keys) as the response.
	s.handleGetSettings(w, r)
}

// newLLMClient builds a gemini client over the active provider's key pool, with
// DB-backed key blocks wired in: blocked keys are skipped, and a key that dies
// mid-call is persisted as blocked so it stays out of rotation after a restart.
// Returns nil when no key is configured.
func newLLMClient(ctx context.Context, db *database.DB, snap config.ConfigSnapshot, model string) *gemini.Client {
	keys := snap.ActiveLLMKeys()
	if len(keys) == 0 {
		return nil
	}
	blocked, err := db.ActiveLLMKeyBlocks(ctx)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to load LLM key blocks, starting with all keys live")
	}
	return gemini.NewRotatingClient(
		snap.LLMProviderOrDefault(), keys, snap.LLMKeyRotateInterval(),
		model, snap.GeminiEmbeddingModel, snap.GeminiEmbeddingDims,
	).WithKeyBlocks(db, blocked)
}

// handleGetLLMKeys reports the key pool and its block list for the dashboard.
// Keys themselves are never returned — only tails and fingerprints.
func (s *Server) handleGetLLMKeys(w http.ResponseWriter, r *http.Request) {
	snap := s.config.Snapshot()
	blocks, err := s.db.ListLLMKeyBlocks(r.Context())
	if err != nil {
		log.Error().Err(err).Msg("Failed to list LLM key blocks")
		http.Error(w, `{"error":"failed to list key blocks"}`, http.StatusInternalServerError)
		return
	}

	type poolKey struct {
		Fingerprint string `json:"fingerprint"`
		KeyTail     string `json:"key_tail"`
	}
	pool := make([]poolKey, 0)
	for _, k := range snap.ActiveLLMKeys() {
		pool = append(pool, poolKey{Fingerprint: gemini.Fingerprint(k), KeyTail: maskKey(k)})
	}

	// Which key is serving this rotation window — ask the live client, not the
	// config, so the answer reflects blocks too.
	activeNow := ""
	if gc := s.getGemini(); gc != nil {
		activeNow = gc.ActiveFingerprint()
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{
		"provider":     snap.LLMProviderOrDefault(),
		"rotate_hours": snap.LLMKeyRotateHours,
		"keys":         pool,
		"blocked":      blocks,
		"active_now":   activeNow,
	}); err != nil {
		log.Error().Err(err).Msg("Failed to encode LLM keys response")
	}
}

// handleUnblockLLMKey clears a block so the key rejoins rotation immediately
// (quota reset early, key replaced upstream).
func (s *Server) handleUnblockLLMKey(w http.ResponseWriter, r *http.Request) {
	fp := r.URL.Query().Get("fingerprint")
	if fp == "" {
		http.Error(w, `{"error":"fingerprint required"}`, http.StatusBadRequest)
		return
	}
	if err := s.db.UnblockLLMKey(r.Context(), fp); err != nil {
		log.Error().Err(err).Str("fingerprint", fp).Msg("Failed to unblock LLM key")
		http.Error(w, `{"error":"failed to unblock key"}`, http.StatusInternalServerError)
		return
	}
	// Rebuild the clients so the in-memory blocked set drops it too.
	s.reloadGemini()
	log.Info().Str("fingerprint", fp).Msg("LLM key unblocked")
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ok":true}`))
}

func (s *Server) reloadGemini() {
	snap := s.config.Snapshot()
	ctx := context.Background()

	newClient := newLLMClient(ctx, s.db, snap, snap.GeminiModel)
	if newClient == nil {
		s.mu.Lock()
		s.gemini = nil
		s.searcher = nil
		s.mu.Unlock()
		log.Warn().Str("provider", snap.LLMProviderOrDefault()).Msg("LLM API key cleared, observation extraction disabled")
		return
	}

	newSearcher := search.NewSearcher(s.db, newClient)

	s.mu.Lock()
	s.gemini = newClient
	s.searcher = newSearcher
	s.mu.Unlock()

	log.Info().Str("model", snap.GeminiModel).Msg("Gemini client reloaded")

	// Mirror the startup wiring (NewServer): the graph judge/describe client
	// runs graph_gemini_model when it differs from gemini_model, and graph
	// summaries run on Claude when an Anthropic key is set. Swapping in place
	// updates the adapter captured by registered job handlers.
	if s.graphAdapter == nil {
		return // no LLM key at boot; restart required to enable graph LLM calls
	}
	graphClient, graphModel := newClient, snap.GeminiModel
	if snap.GraphGeminiModel != "" && snap.GraphGeminiModel != snap.GeminiModel {
		graphModel = snap.GraphGeminiModel
		graphClient = newLLMClient(ctx, s.db, snap, graphModel)
	}
	var summaryLLM graphhandlers.TextGenerator
	summaryModel := ""
	if snap.AnthropicAPIKey != "" {
		summaryLLM = anthropic.NewClient(snap.AnthropicAPIKey, snap.AnthropicModel)
		summaryModel = snap.AnthropicModel
	}
	s.graphAdapter.Swap(graphClient, summaryLLM)
	log.Info().Str("model", graphModel).Str("summary_model", summaryModel).Msg("Graph LLM client reloaded")
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
