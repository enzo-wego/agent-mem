package worker

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/agent-mem/agent-mem/internal/config"
	graphhandlers "github.com/agent-mem/agent-mem/internal/graph/handlers"
	"github.com/agent-mem/agent-mem/internal/llmgateway"
	"github.com/agent-mem/agent-mem/internal/search"
)

// settingsResponse is the JSON shape returned by GET /api/settings.
// DatabaseURL and LLMGatewayAPIKey are masked for security.
type settingsResponse struct {
	WorkerPort  int    `json:"worker_port"`
	DataDir     string `json:"data_dir"`
	LogLevel    string `json:"log_level"`
	DatabaseURL string `json:"database_url"`

	// No provider keys or model names: agent-mem holds neither. Model choice
	// belongs to llm-gateway, which every LLM call goes through. What remains is
	// this service's own client config — which gateway to call, and how wide its
	// embedding columns are.
	GeminiEmbeddingDims int    `json:"gemini_embedding_dims"`
	LLMGatewayURL       string `json:"llm_gateway_url"`
	LLMGatewayAPIKey    string `json:"llm_gateway_api_key"`

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
		WorkerPort:          snap.WorkerPort,
		DataDir:             snap.DataDir,
		LogLevel:            snap.LogLevel,
		DatabaseURL:         maskKey(snap.DatabaseURL),
		GeminiEmbeddingDims: snap.GeminiEmbeddingDims,
		LLMGatewayURL:       snap.LLMGatewayURL,
		LLMGatewayAPIKey:    maskKey(snap.LLMGatewayAPIKey),
		ContextObservations: snap.ContextObservations,
		ContextFullCount:    snap.ContextFullCount,
		ContextSessionCount: snap.ContextSessionCount,
		SkipTools:           snap.SkipTools,
		AllowedProjects:     snap.AllowedProjects,
		IgnoredProjects:     snap.IgnoredProjects,
		SyncEnabled:         snap.SyncEnabled,
		SyncURL:             snap.SyncURL,
		SyncInterval:        snap.SyncInterval,
		MachineID:           snap.MachineID,
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

	llmChanged := s.config.Update(partial)

	// Persist runtime settings to PostgreSQL.
	if err := s.db.SaveSettings(r.Context(), s.config.RuntimeSettings()); err != nil {
		log.Error().Err(err).Msg("Failed to save settings to database")
		http.Error(w, `{"error":"failed to persist settings"}`, http.StatusInternalServerError)
		return
	}

	// Apply side-effects.
	if llmChanged {
		s.reloadLLM()
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

// newGatewayClient returns an llm-gateway client at the given embedding width,
// or nil when no URL is configured. Empty URL is the documented off switch.
//
// dims must match the destination column, and the two callers differ:
// observations.embedding is vector(768) while the graph uses halfvec(3072).
// Passing the wrong one fails every insert with "expected 768 dimensions, not
// 3072", which reads like a schema fault rather than a config one — hence two
// clients rather than one shared default.
func newGatewayClient(snap config.ConfigSnapshot, dims int) *llmgateway.Client {
	if strings.TrimSpace(snap.LLMGatewayURL) == "" {
		return nil
	}
	return llmgateway.New(snap.LLMGatewayURL, snap.LLMGatewayAPIKey, dims)
}

// flatLLMFor returns the client flat memory should call, or nil when no gateway
// is configured — in which case observation extraction and session summaries are
// simply skipped, the same as having no LLM at all. There is no direct-provider
// path to fall back to by design.
//
// dims comes from gemini_embedding_dims (768) because observations.embedding is
// vector(768) — NOT the graph's 3072.
//
// The nil case must be a nil INTERFACE, not a typed nil inside one, or every
// `!= nil` guard passes and the first call panics.
func flatLLMFor(snap config.ConfigSnapshot) flatLLM {
	if gw := newGatewayClient(snap, snap.GeminiEmbeddingDims); gw != nil {
		return gw
	}
	return nil
}

// newGraphGateway returns the gateway as a graph GeminiClient, or a nil
// interface when unconfigured.
//
// The nil-interface dance matters: returning a typed (*llmgateway.Client)(nil)
// inside a non-nil interface would make the adapter's `gw != nil` check pass and
// every call would panic. Build the interface only when there is a real client.
func newGraphGateway(snap config.ConfigSnapshot, dims int) graphhandlers.GeminiClient {
	if c := newGatewayClient(snap, dims); c != nil {
		return c
	}
	return nil
}

// reloadLLM rebuilds the LLM clients after a settings change, so editing the
// gateway URL or key takes effect without a worker restart.
//
// Named for what it does now: there is no Gemini client to reload, only the
// gateway one. Both graph and flat memory are rebuilt from the same snapshot so
// they cannot drift apart.
func (s *Server) reloadLLM() {
	snap := s.config.Snapshot()

	// Flat memory reads flatLLM per call, so replacing it IS the reload. The
	// searcher must embed queries through the same client that embedded the
	// stored rows, or query vectors land in a different space than the corpus.
	newFlat := flatLLMFor(snap)
	var newSearcher *search.Searcher
	if newFlat != nil {
		newSearcher = search.NewSearcher(s.db, newFlat)
	}

	s.mu.Lock()
	s.flatLLM = newFlat
	s.searcher = newSearcher
	s.mu.Unlock()

	if newFlat == nil {
		log.Warn().Msg("llm_gateway_url cleared — flat-memory extraction and search disabled")
	}

	// The graph adapter is swapped rather than replaced: job handlers captured
	// the interface value at RegisterAll time and never see a rebuilt Deps.
	if s.graphAdapter == nil {
		return // no gateway at boot; a restart is required to enable graph LLM calls
	}
	if gw := newGraphGateway(snap, graphhandlers.GraphEmbeddingDims); gw != nil {
		s.graphAdapter.Swap(gw)
		log.Info().Str("url", snap.LLMGatewayURL).Msg("LLM gateway client reloaded")
	}
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
