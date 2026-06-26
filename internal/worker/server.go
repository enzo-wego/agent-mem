package worker

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/rs/zerolog/log"
	"golang.org/x/sync/semaphore"

	"github.com/agent-mem/agent-mem/internal/anthropic"
	"github.com/agent-mem/agent-mem/internal/config"
	memctx "github.com/agent-mem/agent-mem/internal/context"
	"github.com/agent-mem/agent-mem/internal/database"
	"github.com/agent-mem/agent-mem/internal/gemini"
	"github.com/agent-mem/agent-mem/internal/graph/extractor"
	"github.com/agent-mem/agent-mem/internal/graph/fetchers"
	graphhandlers "github.com/agent-mem/agent-mem/internal/graph/handlers"
	"github.com/agent-mem/agent-mem/internal/graph/identity"
	"github.com/agent-mem/agent-mem/internal/graph/jobs"
	"github.com/agent-mem/agent-mem/internal/graph/normalizer"
	"github.com/agent-mem/agent-mem/internal/search"
	memsync "github.com/agent-mem/agent-mem/internal/sync"
)

// Server is the long-lived HTTP worker that handles hook events and serves the API.
type Server struct {
	config     *config.Config
	db         *database.DB
	contextBld *memctx.Builder
	syncEngine *memsync.Engine
	manager    *jobs.Manager
	router     chi.Router
	http       *http.Server
	cancel     context.CancelFunc

	mu       sync.RWMutex // protects gemini and searcher
	gemini   *gemini.Client
	searcher *search.Searcher

	logBuffer *LogBuffer
}

// getGemini returns the current Gemini client (may be nil).
func (s *Server) getGemini() *gemini.Client {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.gemini
}

// getSearcher returns the current searcher (may be nil).
func (s *Server) getSearcher() *search.Searcher {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.searcher
}

// NewServer creates a new worker server. logBuf may be nil.
func NewServer(cfg *config.Config, logBuf *LogBuffer) (*Server, error) {
	ctx := context.Background()

	// Run goose migrations before connecting the pool
	if err := database.RunMigrations(cfg.DatabaseURL); err != nil {
		return nil, fmt.Errorf("run migrations: %w", err)
	}

	pool, err := database.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("connect to database: %w", err)
	}

	db := database.NewDB(pool)

	// Load runtime settings from database (overrides file defaults, env still wins)
	if dbSettings, err := db.GetAllSettings(ctx); err == nil && len(dbSettings) > 0 {
		cfg.ApplyDBSettings(dbSettings)
		config.ApplyEnv(cfg) // env vars always take final precedence
		log.Info().Int("count", len(dbSettings)).Msg("Runtime settings loaded from database")
	}

	var geminiClient *gemini.Client
	if cfg.GeminiAPIKey != "" {
		geminiClient = gemini.NewClient(cfg.GeminiAPIKey, cfg.GeminiModel, cfg.GeminiEmbeddingModel, cfg.GeminiEmbeddingDims)
		log.Info().Str("model", cfg.GeminiModel).Msg("Gemini client initialized")
	} else {
		log.Warn().Msg("No Gemini API key configured, observation extraction disabled")
	}

	// When an Anthropic key is set, graph summaries (cluster/thread/feature) run
	// on Claude instead of Gemini Flash to cut hallucination. Embeddings stay on Gemini.
	var summaryLLM graphhandlers.TextGenerator
	if cfg.AnthropicAPIKey != "" {
		summaryLLM = anthropic.NewClient(cfg.AnthropicAPIKey, cfg.AnthropicModel)
		log.Info().Str("model", cfg.AnthropicModel).Msg("Anthropic client initialized for graph summaries")
	}

	var searcher *search.Searcher
	if geminiClient != nil {
		searcher = search.NewSearcher(db, geminiClient)
	}

	var syncEng *memsync.Engine
	if cfg.SyncEnabled && cfg.SyncURL != "" {
		syncEng = memsync.NewEngine(db, cfg)
		log.Info().Str("url", cfg.SyncURL).Msg("Sync engine configured")
	}

	// Build graph deps and dispatcher
	graphLog := log.Logger
	graphDeps := graphhandlers.Deps{
		DB:          pool,
		Logger:      graphLog,
		MachineID:   cfg.MachineID,
		Fetchers:    fetchers.NewRegistry(fetchersConfigFromAppConfig(cfg), graphLog),
		Normalizers: normalizer.NewDefault(newDBNameCache(pool)),
		Extractor:   extractor.New(pool, graphLog),
		Identity:    identity.NewService(pool, graphLog),
		Gemini:      graphhandlers.NewGeminiAdapter(geminiClient, summaryLLM),
		LiteParse:   liteparseConfigFromEnv(),

		SlackBotToken: cfg.Graph.SlackBotToken,
		SlackDMUserID: cfg.Graph.SlackDMUserID,
		Runner:        cfg.Graph.Runner,
	}

	rate := rateFromAppConfig(cfg)
	sems := map[string]*semaphore.Weighted{
		"slack":      semaphore.NewWeighted(rate.Slack),
		"jira":       semaphore.NewWeighted(rate.Jira),
		"github":     semaphore.NewWeighted(rate.Github),
		"confluence": semaphore.NewWeighted(rate.Confluence),
		"pagerduty":  semaphore.NewWeighted(rate.Pagerduty),
		"datadog":    semaphore.NewWeighted(rate.Datadog),
		"sentry":     semaphore.NewWeighted(rate.Sentry),
		"gws":        semaphore.NewWeighted(rate.GWS),
		"gemini":     semaphore.NewWeighted(rate.Gemini),
	}
	reg := jobs.NewRegistry()
	graphhandlers.RegisterAll(reg, graphDeps)

	// Refresh Slack channel id→name on startup so the map labels channels by name
	// (covers channels added since the last boot). Deduped: skip if one is already
	// queued/running. Best-effort — a failure just leaves names unresolved.
	var refreshPending bool
	_ = pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM graph.jobs WHERE type='refresh_slack_channels' AND status IN ('queued','running'))`).
		Scan(&refreshPending)
	if !refreshPending {
		if _, err := jobs.Enqueue(ctx, pool, "refresh_slack_channels", map[string]any{},
			jobs.EnqueueOptions{TargetRunner: cfg.Graph.Runner, MachineID: cfg.MachineID}); err != nil {
			graphLog.Warn().Err(err).Msg("startup: enqueue refresh_slack_channels failed")
		}
	}

	// Kick off the self-rescheduling hot-topic detector (deduped: skip if one is
	// already queued/running). Each run re-enqueues the next tick.
	var detectPending bool
	_ = pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM graph.jobs WHERE type='detect_hot_topics' AND status IN ('queued','running'))`).
		Scan(&detectPending)
	if !detectPending {
		if _, err := jobs.Enqueue(ctx, pool, "detect_hot_topics", map[string]any{},
			jobs.EnqueueOptions{TargetRunner: cfg.Graph.Runner, MachineID: cfg.MachineID}); err != nil {
			graphLog.Warn().Err(err).Msg("startup: enqueue detect_hot_topics failed")
		}
	}
	mgr := jobs.NewManager(jobs.ManagerConfig{
		Registry:            reg,
		DB:                  pool,
		WorkerID:            cfg.MachineID,
		Runner:              cfg.Graph.Runner,
		Semaphores:          sems,
		IdleInterval:        5 * time.Second,
		BackoffBase:         30 * time.Second,
		BackoffCap:          time.Hour,
		JanitorScanInterval: 30 * time.Second,
		JanitorBatchSize:    100,
		Logger:              graphLog,
	})

	s := &Server{
		config:     cfg,
		db:         db,
		gemini:     geminiClient,
		contextBld: memctx.NewBuilder(db, cfg),
		searcher:   searcher,
		syncEngine: syncEng,
		manager:    mgr,
		logBuffer:  logBuf,
	}

	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(middleware.RealIP)
	r.Use(corsMiddleware)

	// Public endpoints (no auth required)
	r.Get("/api/health", s.handleHealth)

	// Hook endpoints (called by local CLI hooks, no auth required)
	r.Post("/api/hook/session-start", s.handleSessionStart)
	r.Post("/api/hook/prompt-submit", s.handlePromptSubmit)
	r.Post("/api/hook/post-tool-use", s.handlePostToolUse)
	r.Post("/api/hook/stop", s.handleStop)

	// Protected API endpoints (require Bearer api_key when configured)
	r.Group(func(r chi.Router) {
		r.Use(s.apiKeyMiddleware)

		// Search endpoints
		r.Get("/api/search", s.handleSearch)
		r.Get("/api/search/by-file", s.handleSearchByFile)
		r.Get("/api/search/timeline", s.handleSearchTimeline)
		r.Get("/api/stats", s.handleStats)
		r.Get("/api/projects", s.handleListProjects)
		r.Get("/api/observations", s.handleListObservations)
		r.Get("/api/observations/{id}", s.handleGetObservation)
		r.Get("/api/summaries", s.handleListSummaries)
		r.Get("/api/prompts", s.handleListPrompts)

		// Settings endpoints
		r.Get("/api/settings", s.handleGetSettings)
		r.Put("/api/settings", s.handleUpdateSettings)

		// Logs endpoint
		r.Get("/api/logs", s.handleGetLogs)

		// Sync endpoints
		r.Post("/api/sync/push", s.handleSyncPush)
		r.Get("/api/sync/pull", s.handleSyncPull)
		r.Get("/api/sync/info", s.handleSyncInfo)
		r.Get("/api/sync/cloud-stats", s.handleSyncCloudStats)

		// Graph memory endpoints
		graphhandlers.Mount(r, graphDeps)
	})

	// Dashboard (served at root, after API routes)
	r.Handle("/*", serveDashboard())

	s.router = r
	return s, nil
}

// Run starts the HTTP server and background processor, blocking until shutdown.
func (s *Server) Run() error {
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel

	// Start background message processor
	go s.processLoop(ctx)

	// Start sync engine if configured
	if s.syncEngine != nil {
		go s.syncEngine.Start(ctx)
	}

	// Start graph job manager (dispatchers + janitor)
	if s.manager != nil {
		go s.manager.Run(ctx)
	}

	s.http = &http.Server{
		Addr:              fmt.Sprintf(":%d", s.config.WorkerPort),
		Handler:           s.router,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Graceful shutdown on SIGTERM/SIGINT
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		sig := <-sigCh
		log.Info().Str("signal", sig.String()).Msg("Shutting down")
		cancel()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()
		if err := s.http.Shutdown(shutdownCtx); err != nil {
			log.Error().Err(err).Msg("HTTP shutdown error")
		}
		// Wait for in-flight graph jobs to finish.
		if s.manager != nil {
			s.manager.Wait()
		}
	}()

	log.Info().Int("port", s.config.WorkerPort).Msg("Worker started")
	if err := s.http.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("http server: %w", err)
	}
	return nil
}

// liteparseConfigFromEnv builds LiteParseConfig from environment variables.
//
// Env vars:
//
//	LITEPARSE_BIN_PATH          — path to the lit binary (default "lit")
//	LITEPARSE_SCREENSHOT_ENABLED — enable per-page screenshots for thin-text fallback (default "true")
//	LITEPARSE_TEMP_DIR           — working directory for temp files (default os.TempDir())
func liteparseConfigFromEnv() graphhandlers.LiteParseConfig {
	cfg := graphhandlers.LiteParseConfig{
		BinPath:           "lit",
		ScreenshotEnabled: true,
		TempDir:           os.TempDir(),
	}
	if v := os.Getenv("LITEPARSE_BIN_PATH"); v != "" {
		cfg.BinPath = v
	}
	if v := os.Getenv("LITEPARSE_SCREENSHOT_ENABLED"); v != "" {
		cfg.ScreenshotEnabled = strings.EqualFold(v, "true") || v == "1"
	}
	if v := os.Getenv("LITEPARSE_TEMP_DIR"); v != "" {
		cfg.TempDir = v
	}
	return cfg
}

// fetchersConfigFromAppConfig converts app Config to fetchers.Config.
func fetchersConfigFromAppConfig(cfg *config.Config) fetchers.Config {
	return fetchers.Config{
		SlackBotToken:    cfg.Graph.SlackBotToken,
		JiraEmail:        cfg.Graph.JiraEmail,
		JiraToken:        cfg.Graph.JiraToken,
		JiraBaseURL:      cfg.Graph.JiraBaseURL,
		GHToken:          cfg.Graph.GHToken,
		GHBaseURL:        cfg.Graph.GHBaseURL,
		CFToken:          cfg.Graph.CFToken,
		CFBaseURL:        cfg.Graph.CFBaseURL,
		PagerDutyToken:   cfg.Graph.PagerDutyToken,
		PagerDutyBaseURL: cfg.Graph.PagerDutyBaseURL,
		DatadogAPIKey:    cfg.Graph.DatadogAPIKey,
		DatadogAppKey:    cfg.Graph.DatadogAppKey,
		DatadogBaseURL:   cfg.Graph.DatadogBaseURL,
		SentryAuthToken:  cfg.Graph.SentryAuthToken,
		SentryBaseURL:    cfg.Graph.SentryBaseURL,
		SentryOrg:        cfg.Graph.SentryOrg,
		GWSServiceKeyPath: cfg.Graph.GWSServiceKeyPath,
		WegoHubToken:      cfg.Graph.WegoHubToken,
		WegoHubBaseURL:    cfg.Graph.WegoHubBaseURL,
	}
}

// rateFromAppConfig converts app GraphRateConfig to jobs.Rate.
func rateFromAppConfig(cfg *config.Config) jobs.Rate {
	r := cfg.Graph.Rate
	rate := jobs.DefaultRate()
	if r.Slack > 0 {
		rate.Slack = r.Slack
	}
	if r.Jira > 0 {
		rate.Jira = r.Jira
	}
	if r.Github > 0 {
		rate.Github = r.Github
	}
	if r.Confluence > 0 {
		rate.Confluence = r.Confluence
	}
	if r.Pagerduty > 0 {
		rate.Pagerduty = r.Pagerduty
	}
	if r.Datadog > 0 {
		rate.Datadog = r.Datadog
	}
	if r.Sentry > 0 {
		rate.Sentry = r.Sentry
	}
	if r.GWS > 0 {
		rate.GWS = r.GWS
	}
	if r.Gemini > 0 {
		rate.Gemini = r.Gemini
	}
	return rate
}

// corsMiddleware handles CORS preflight OPTIONS requests and adds CORS headers.
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}
