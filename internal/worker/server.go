package worker

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/rs/zerolog/log"

	"github.com/agent-mem/agent-mem/internal/config"
	memctx "github.com/agent-mem/agent-mem/internal/context"
	"github.com/agent-mem/agent-mem/internal/database"
	"github.com/agent-mem/agent-mem/internal/gemini"
	"github.com/agent-mem/agent-mem/internal/search"
	memsync "github.com/agent-mem/agent-mem/internal/sync"
)

// Server is the long-lived HTTP worker that handles hook events and serves the API.
type Server struct {
	config     *config.Config
	db         *database.DB
	contextBld *memctx.Builder
	syncEngine *memsync.Engine
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

	var searcher *search.Searcher
	if geminiClient != nil {
		searcher = search.NewSearcher(db, geminiClient)
	}

	var syncEng *memsync.Engine
	if cfg.SyncEnabled && cfg.SyncURL != "" {
		syncEng = memsync.NewEngine(db, cfg)
		log.Info().Str("url", cfg.SyncURL).Msg("Sync engine configured")
	}

	s := &Server{
		config:     cfg,
		db:         db,
		gemini:     geminiClient,
		contextBld: memctx.NewBuilder(db, cfg),
		searcher:   searcher,
		syncEngine: syncEng,
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
	}()

	log.Info().Int("port", s.config.WorkerPort).Msg("Worker started")
	if err := s.http.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("http server: %w", err)
	}
	return nil
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
