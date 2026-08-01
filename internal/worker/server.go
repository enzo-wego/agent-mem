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

	mu     sync.RWMutex // protects gemini, flatLLM and searcher
	gemini *gemini.Client
	// flatLLM serves every flat-memory LLM call: observation extraction, session
	// summaries and their embeddings. It is the llm-gateway client when one is
	// configured, else the direct provider client. Kept separate from gemini
	// because that field is still the provider client the key-rotation UI
	// inspects (ActiveFingerprint), which the gateway cannot answer for.
	flatLLM  flatLLM
	searcher *search.Searcher

	// graphAdapter is the graph handlers' LLM adapter; swapped in place on
	// settings reload. Nil when no LLM key was set at boot (restart required
	// to enable graph LLM calls in that case).
	graphAdapter *graphhandlers.GeminiAdapter

	logBuffer *LogBuffer
}

// flatLLM is the flat-memory LLM surface: extract an observation, summarise a
// session, embed the result. Both *gemini.Client and *llmgateway.Client satisfy
// it, which is what lets the gateway stand in without touching call sites.
type flatLLM interface {
	Generate(ctx context.Context, systemPrompt, userMessage string) (string, error)
	Embed(ctx context.Context, text string) ([]float32, error)
}

// getGemini returns the direct provider client (may be nil). Use this ONLY for
// provider-specific concerns such as key rotation; anything that makes an LLM
// call must use getFlatLLM so it honours the gateway setting.
func (s *Server) getGemini() *gemini.Client {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.gemini
}

// getFlatLLM returns the client flat memory should call (may be nil).
func (s *Server) getFlatLLM() flatLLM {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.flatLLM
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

	snap := cfg.Snapshot()
	llmProvider := snap.LLMProviderOrDefault()

	geminiClient := newLLMClient(ctx, db, snap, cfg.GeminiModel)
	if geminiClient != nil {
		log.Info().Str("provider", llmProvider).Str("model", cfg.GeminiModel).
			Int("keys", len(snap.ActiveLLMKeys())).Dur("key_rotate", snap.LLMKeyRotateInterval()).
			Msg("LLM client initialized")
	} else {
		log.Warn().Str("provider", llmProvider).Msg("No API key configured for LLM provider, observation extraction disabled")
	}

	// Graph judge/describe can run a different Gemini model than flat memory —
	// flat memory's prompts (observation extraction, session summaries) are
	// tuned against gemini_model and must not silently change with it.
	graphGeminiClient := geminiClient
	if geminiClient != nil && cfg.GraphGeminiModel != "" && cfg.GraphGeminiModel != cfg.GeminiModel {
		graphGeminiClient = newLLMClient(ctx, db, snap, cfg.GraphGeminiModel)
		log.Info().Str("provider", llmProvider).Str("model", cfg.GraphGeminiModel).Msg("Graph LLM client initialized (separate from flat memory)")
	}

	// When llm-gateway is configured it serves EVERY graph LLM call — generate,
	// cheap judge, embed and describe — so metering, alerting and failover have a
	// single place to live. Never the Anthropic API directly: metered per token
	// with no ceiling, which is how an amplification bug spent ~$11/hour.
	//
	// 3072 dims: the graph writes halfvec(3072). Flat memory builds its own
	// client at 768 below.
	graphGateway := newGraphGateway(snap, graphhandlers.GraphEmbeddingDims)
	if graphGateway != nil {
		log.Info().Str("url", snap.LLMGatewayURL).Msg("llm-gateway wired for all graph LLM calls")
	}

	// Concrete handle kept on the Server so settings reload can Swap the
	// underlying clients in place (job handlers capture the interface value
	// at RegisterAll time and never see a rebuilt Deps). Deps.Gemini must be
	// assigned the interface value, not the typed pointer — a nil *GeminiAdapter
	// in the interface would defeat handlers' nil checks.
	graphGemini := graphhandlers.NewGeminiAdapter(graphGeminiClient, graphGateway)
	graphAdapter, _ := graphGemini.(*graphhandlers.GeminiAdapter)

	// Flat memory: gateway when configured, else the provider client. 768 dims —
	// observations.embedding is vector(768), unlike the graph's 3072.
	flat := flatLLMFor(snap, geminiClient)
	if flat != nil && graphGateway != nil {
		log.Info().Msg("llm-gateway wired for flat-memory generation and embeddings")
	}

	// The searcher must embed queries through the same client that embedded the
	// stored rows, or query vectors land in a different space than the corpus.
	var searcher *search.Searcher
	if flat != nil {
		searcher = search.NewSearcher(db, flat)
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
		Gemini:      graphGemini,
		LiteParse:   liteparseConfigFromEnv(),

		SlackBotToken: cfg.Graph.SlackBotToken,
		SlackDMUserID: cfg.Graph.SlackDMUserID,
		Runner:        cfg.Graph.Runner,
		PublicBaseURL: cfg.PublicBaseURL,
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

	// Resolve Slack bot_id authors (B…) to bot names on startup — users.list can't
	// reach them, so they'd otherwise show as raw ids in author chips. Deduped like
	// the channel refresh; best-effort.
	var botsPending bool
	_ = pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM graph.jobs WHERE type='refresh_slack_bots' AND status IN ('queued','running'))`).
		Scan(&botsPending)
	if !botsPending {
		if _, err := jobs.Enqueue(ctx, pool, "refresh_slack_bots", map[string]any{},
			jobs.EnqueueOptions{TargetRunner: cfg.Graph.Runner, MachineID: cfg.MachineID}); err != nil {
			graphLog.Warn().Err(err).Msg("startup: enqueue refresh_slack_bots failed")
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

	// Recompute evidence-backed person roles daily. The handler schedules its next run;
	// startup only repairs a missing chain and triggers the first computation after deploy.
	var rolesPending bool
	_ = pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM graph.jobs WHERE type='derive_person_roles' AND status IN ('queued','running'))`).
		Scan(&rolesPending)
	if !rolesPending {
		if _, err := jobs.Enqueue(ctx, pool, "derive_person_roles", map[string]any{},
			jobs.EnqueueOptions{TargetRunner: "any", MachineID: cfg.MachineID}); err != nil {
			graphLog.Warn().Err(err).Msg("startup: enqueue derive_person_roles failed")
		}
	}

	// Kick off the self-rescheduling Jira board→epic map refresh (deduped).
	var jiraBoardPending bool
	_ = pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM graph.jobs WHERE type='refresh_jira_board' AND status IN ('queued','running'))`).
		Scan(&jiraBoardPending)
	if !jiraBoardPending {
		if _, err := jobs.Enqueue(ctx, pool, "refresh_jira_board", map[string]any{},
			jobs.EnqueueOptions{TargetRunner: cfg.Graph.Runner, MachineID: cfg.MachineID}); err != nil {
			graphLog.Warn().Err(err).Msg("startup: enqueue refresh_jira_board failed")
		}
	}

	// Kick off the self-rescheduling watch-channels notifier (DMs every message in
	// the Payment Partners group). Deduped: skip if one is already queued/running.
	var watchPending bool
	_ = pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM graph.jobs WHERE type='notify_watch_channels' AND status IN ('queued','running'))`).
		Scan(&watchPending)
	if !watchPending {
		if _, err := jobs.Enqueue(ctx, pool, "notify_watch_channels", map[string]any{},
			jobs.EnqueueOptions{TargetRunner: cfg.Graph.Runner, MachineID: cfg.MachineID}); err != nil {
			graphLog.Warn().Err(err).Msg("startup: enqueue notify_watch_channels failed")
		}
	}

	// Arm the 7-day hourly monitor (threaded DM report). Deduped; the handler
	// self-expires 7 days after its first run, so a restart after that just no-ops.
	var monitorPending bool
	_ = pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM graph.jobs WHERE type='monitor_hourly_report' AND status IN ('queued','running'))`).
		Scan(&monitorPending)
	if !monitorPending {
		if _, err := jobs.Enqueue(ctx, pool, "monitor_hourly_report", map[string]any{},
			jobs.EnqueueOptions{TargetRunner: cfg.Graph.Runner, MachineID: cfg.MachineID}); err != nil {
			graphLog.Warn().Err(err).Msg("startup: enqueue monitor_hourly_report failed")
		}
	}

	// Backfill summaries for discussion threads (2+ messages) that never got one
	// — the lazy popup path only summarizes what a user happens to open. LLM
	// required; idempotent (summarized threads no longer match the query).
	if graphDeps.Gemini != nil {
		go func() {
			if n := graphhandlers.BackfillMissingThreadSummaries(ctx, pool, 1000); n > 0 {
				graphLog.Info().Int("enqueued", n).Msg("startup: thread-summary backfill")
			}
		}()
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
		// Read per claim rather than captured once, so toggling the setting from
		// the dashboard takes effect within one idle interval without a restart.
		Paused: func() bool { return cfg.Snapshot().ProcessingPaused },
	})

	s := &Server{
		config:       cfg,
		db:           db,
		gemini:       geminiClient,
		flatLLM:      flat,
		contextBld:   memctx.NewBuilder(db, cfg),
		searcher:     searcher,
		syncEngine:   syncEng,
		manager:      mgr,
		logBuffer:    logBuf,
		graphAdapter: graphAdapter,
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
		r.Get("/api/llm-keys", s.handleGetLLMKeys)
		r.Delete("/api/llm-keys/block", s.handleUnblockLLMKey)

		// Logs endpoint
		r.Get("/api/logs", s.handleGetLogs)

		// OpenRouter usage endpoint
		r.Get("/api/openrouter/usage", s.handleOpenRouterUsage)

		// Sync endpoints
		r.Post("/api/sync/push", s.handleSyncPush)
		r.Get("/api/sync/pull", s.handleSyncPull)
		r.Get("/api/sync/pull_derived", s.handleSyncPullDerived)
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
		SlackBotToken:     cfg.Graph.SlackBotToken,
		JiraEmail:         cfg.Graph.JiraEmail,
		JiraToken:         cfg.Graph.JiraToken,
		JiraBaseURL:       cfg.Graph.JiraBaseURL,
		GHToken:           cfg.Graph.GHToken,
		GHBaseURL:         cfg.Graph.GHBaseURL,
		CFToken:           cfg.Graph.CFToken,
		CFBaseURL:         cfg.Graph.CFBaseURL,
		PagerDutyToken:    cfg.Graph.PagerDutyToken,
		PagerDutyBaseURL:  cfg.Graph.PagerDutyBaseURL,
		DatadogAPIKey:     cfg.Graph.DatadogAPIKey,
		DatadogAppKey:     cfg.Graph.DatadogAppKey,
		DatadogBaseURL:    cfg.Graph.DatadogBaseURL,
		SentryAuthToken:   cfg.Graph.SentryAuthToken,
		SentryBaseURL:     cfg.Graph.SentryBaseURL,
		SentryOrg:         cfg.Graph.SentryOrg,
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
