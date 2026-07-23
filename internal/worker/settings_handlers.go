package worker

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/agent-mem/agent-mem/internal/anthropic"
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

func (s *Server) reloadGemini() {
	snap := s.config.Snapshot()

	if snap.ActiveLLMKey() == "" {
		s.mu.Lock()
		s.gemini = nil
		s.searcher = nil
		s.mu.Unlock()
		log.Warn().Str("provider", snap.LLMProviderOrDefault()).Msg("LLM API key cleared, observation extraction disabled")
		return
	}

	newClient := gemini.NewClient(snap.LLMProviderOrDefault(), snap.ActiveLLMKey(), snap.GeminiModel, snap.GeminiEmbeddingModel, snap.GeminiEmbeddingDims)
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
		graphClient = gemini.NewClient(snap.LLMProviderOrDefault(), snap.ActiveLLMKey(), graphModel, snap.GeminiEmbeddingModel, snap.GeminiEmbeddingDims)
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
