package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// GraphRateConfig holds per-source concurrency caps for the graph job dispatcher.
type GraphRateConfig struct {
	Slack      int64 `json:"slack"`
	Jira       int64 `json:"jira"`
	Github     int64 `json:"github"`
	Confluence int64 `json:"confluence"`
	Pagerduty  int64 `json:"pagerduty"`
	Datadog    int64 `json:"datadog"`
	Sentry     int64 `json:"sentry"`
	GWS        int64 `json:"gws"`
	Gemini     int64 `json:"gemini"`
}

// GraphConfig holds all graph-memory specific configuration.
type GraphConfig struct {
	Runner string `json:"runner"` // "any" | "vps" | "local"

	// Fetcher tokens / base URLs
	SlackBotToken string `json:"slack_bot_token"`

	// SlackDMUserID is the Slack user id (U…) enzobot DMs for hot-topic alerts
	// when a subscription doesn't specify its own subscriber. Env:
	// AGENT_MEM_SLACK_DM_USER.
	SlackDMUserID string `json:"slack_dm_user_id"`

	JiraEmail   string `json:"jira_email"`
	JiraToken   string `json:"jira_token"`
	JiraBaseURL string `json:"jira_base_url"`

	GHToken   string `json:"gh_token"`
	GHBaseURL string `json:"gh_base_url"`

	CFToken   string `json:"cf_token"`
	CFBaseURL string `json:"cf_base_url"`

	PagerDutyToken   string `json:"pagerduty_token"`
	PagerDutyBaseURL string `json:"pagerduty_base_url"`

	DatadogAPIKey  string `json:"datadog_api_key"`
	DatadogAppKey  string `json:"datadog_app_key"`
	DatadogBaseURL string `json:"datadog_base_url"`

	SentryAuthToken string `json:"sentry_auth_token"`
	SentryBaseURL   string `json:"sentry_base_url"`
	SentryOrg       string `json:"sentry_org"`

	GWSServiceKeyPath string `json:"gws_service_key_path"`

	WegoHubToken   string `json:"wegohub_token"`
	WegoHubBaseURL string `json:"wegohub_base_url"`

	Rate GraphRateConfig `json:"rate"`
}

type Config struct {
	mu sync.RWMutex `json:"-"`

	WorkerPort  int    `json:"worker_port"`
	DataDir     string `json:"data_dir"`
	LogLevel    string `json:"log_level"`
	DatabaseURL string `json:"database_url"`

	GeminiAPIKey         string `json:"gemini_api_key"`
	GeminiModel          string `json:"gemini_model"`
	GraphGeminiModel     string `json:"graph_gemini_model"` // graph judge/describe model; empty = use GeminiModel (flat memory keeps its tuned model)
	GeminiEmbeddingModel string `json:"gemini_embedding_model"`
	GeminiEmbeddingDims  int    `json:"gemini_embedding_dims"`

	// LLMProvider picks the gemini-client backend: "openrouter" (default; uses
	// GeminiAPIKey/sk-or) or "google" (direct Gemini API; uses GoogleAPIKeys/AIza).
	// Flip + restart the worker to fail over when OpenRouter is out of quota.
	LLMProvider string `json:"llm_provider"`

	// GoogleAPIKeys is the pool of AIza… keys (comma- or newline-separated) used
	// on the google provider — one key is enough, a pool spreads per-key quota.
	// The active key switches every LLMKeyRotateHours.
	GoogleAPIKeys     string `json:"google_api_keys"`
	LLMKeyRotateHours int    `json:"llm_key_rotate_hours"`

	// AnthropicAPIKey, when set, routes graph summaries (cluster/thread/feature)
	// to Claude instead of Gemini Flash. Embeddings always stay on Gemini.
	AnthropicAPIKey string `json:"anthropic_api_key"`
	AnthropicModel  string `json:"anthropic_model"`

	ContextObservations int    `json:"context_observations"`
	ContextFullCount    int    `json:"context_full_count"`
	ContextSessionCount int    `json:"context_session_count"`
	SkipTools           string `json:"skip_tools"`

	AllowedProjects string `json:"allowed_projects"`
	IgnoredProjects string `json:"ignored_projects"`

	// ProcessingPaused suspends all job execution — graph dispatchers and the
	// flat-memory pending-message loop — while leaving the HTTP API up. Ingest
	// keeps accepting webhooks and enqueueing work; nothing is claimed or sent
	// to an LLM. Unpause and the backlog drains.
	//
	// This exists because the obvious alternative, stopping the worker, takes
	// the API down with it: inbound Slack webhooks arrive via another service
	// and are lost rather than queued if nothing answers. Pausing costs a
	// growing queue; stopping costs data.
	ProcessingPaused bool `json:"processing_paused"`

	SyncEnabled bool   `json:"sync_enabled"`
	SyncURL     string `json:"sync_url"`
	// PublicBaseURL is the public dashboard origin (e.g. https://enzogo.io.vn)
	// used when composing outward links (alert DMs). Empty = no links emitted.
	PublicBaseURL string `json:"public_base_url"`
	SyncInterval  string `json:"sync_interval"`
	APIKey        string `json:"api_key"`
	MachineID     string `json:"machine_id"`

	Graph GraphConfig `json:"graph"`
}

// Snapshot returns a thread-safe, mutex-free copy of the config for reading.
func (c *Config) Snapshot() ConfigSnapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.snapshot()
}

// Save is a no-op — runtime settings are persisted via SaveToDB.
// Kept for backward compatibility.
func (c *Config) Save() error {
	return nil
}

// normalizeProvider maps any value to a valid provider, defaulting to openrouter.
func normalizeProvider(p string) string {
	if p == "google" {
		return "google"
	}
	return "openrouter"
}

// LLMProviderOrDefault returns the configured LLM provider, defaulting to openrouter.
func (c *Config) LLMProviderOrDefault() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return normalizeProvider(c.LLMProvider)
}

// ActiveLLMKey returns the first API key for the active provider (see
// ActiveLLMKeys); callers that only need "is a key configured" use this.
func (c *Config) ActiveLLMKey() string { return c.Snapshot().ActiveLLMKey() }

// ActiveLLMKeys returns the key pool for the active provider.
func (c *Config) ActiveLLMKeys() []string { return c.Snapshot().ActiveLLMKeys() }

// LLMKeyRotateInterval returns the key-rotation window for the active provider.
func (c *Config) LLMKeyRotateInterval() time.Duration { return c.Snapshot().LLMKeyRotateInterval() }

// LLMProviderOrDefault returns the snapshot's provider, defaulting to openrouter.
func (s ConfigSnapshot) LLMProviderOrDefault() string { return normalizeProvider(s.LLMProvider) }

// ActiveLLMKeys returns the key pool for the active provider: the
// google_api_keys list when provider is google, else the OpenRouter key. A
// single google key is just a pool of one. Never contains empty strings.
func (s ConfigSnapshot) ActiveLLMKeys() []string {
	if normalizeProvider(s.LLMProvider) == "google" {
		return SplitKeys(s.GoogleAPIKeys)
	}
	return SplitKeys(s.GeminiAPIKey)
}

// ActiveLLMKey returns the snapshot's first key for the active provider.
func (s ConfigSnapshot) ActiveLLMKey() string {
	keys := s.ActiveLLMKeys()
	if len(keys) == 0 {
		return ""
	}
	return keys[0]
}

// LLMKeyRotateInterval is how long one key stays active before the client
// switches to another key in the pool. 0 = no rotation (single key pinned).
func (s ConfigSnapshot) LLMKeyRotateInterval() time.Duration {
	if s.LLMKeyRotateHours <= 0 {
		return 0
	}
	return time.Duration(s.LLMKeyRotateHours) * time.Hour
}

// SplitKeys parses a key list written with commas, newlines, or spaces between
// entries — whatever the operator pasted into the dashboard. Key pools are
// usually kept labelled ("-- n8n key" above each key), so `#`, `--` and `//`
// comments are dropped: a label parsed as a key would be blocked on first use.
func SplitKeys(s string) []string {
	out := []string{}
	seen := map[string]bool{}
	for line := range strings.SplitSeq(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || isComment(line) {
			continue
		}
		// Trailing comment on a key line ("AIza… # personal"). The leading space
		// keeps a "-" inside a key from being mistaken for a comment marker.
		for _, marker := range []string{" #", " --", " //"} {
			if i := strings.Index(line, marker); i >= 0 {
				line = line[:i]
			}
		}
		fields := strings.FieldsFunc(line, func(r rune) bool {
			return r == ',' || r == '\r' || r == '\t' || r == ' ' || r == ';'
		})
		for _, f := range fields {
			if f != "" && !isComment(f) && !seen[f] {
				seen[f] = true
				out = append(out, f)
			}
		}
	}
	return out
}

func isComment(s string) bool {
	return strings.HasPrefix(s, "#") || strings.HasPrefix(s, "--") || strings.HasPrefix(s, "//")
}

// RuntimeSettings returns the runtime settings as a string map for DB storage.
func (c *Config) RuntimeSettings() map[string]string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return map[string]string{
		"gemini_api_key":         c.GeminiAPIKey,
		"gemini_model":           c.GeminiModel,
		"graph_gemini_model":     c.GraphGeminiModel,
		"gemini_embedding_model": c.GeminiEmbeddingModel,
		"gemini_embedding_dims":  strconv.Itoa(c.GeminiEmbeddingDims),
		"llm_provider":           c.LLMProvider,
		"google_api_keys":        c.GoogleAPIKeys,
		"llm_key_rotate_hours":   strconv.Itoa(c.LLMKeyRotateHours),
		"anthropic_api_key":      c.AnthropicAPIKey,
		"anthropic_model":        c.AnthropicModel,
		"context_observations":   strconv.Itoa(c.ContextObservations),
		"context_full_count":     strconv.Itoa(c.ContextFullCount),
		"context_session_count":  strconv.Itoa(c.ContextSessionCount),
		"skip_tools":             c.SkipTools,
		"allowed_projects":       c.AllowedProjects,
		"ignored_projects":       c.IgnoredProjects,
		"log_level":              c.LogLevel,
		"processing_paused":      strconv.FormatBool(c.ProcessingPaused),
		"sync_enabled":           strconv.FormatBool(c.SyncEnabled),
		"sync_url":               c.SyncURL,
		"public_base_url":        c.PublicBaseURL,
		"sync_interval":          c.SyncInterval,
		"api_key":                c.APIKey,
		"machine_id":             c.MachineID,
	}
}

// ApplyDBSettings overlays settings loaded from the database onto the config.
// Env vars still take final precedence (applied after this).
func (c *Config) ApplyDBSettings(dbSettings map[string]string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	for k, v := range dbSettings {
		switch k {
		case "gemini_api_key":
			c.GeminiAPIKey = v
		case "gemini_model":
			c.GeminiModel = v
		case "graph_gemini_model":
			c.GraphGeminiModel = v
		case "gemini_embedding_model":
			c.GeminiEmbeddingModel = v
		case "gemini_embedding_dims":
			if n, err := strconv.Atoi(v); err == nil {
				c.GeminiEmbeddingDims = n
			}
		case "llm_provider":
			c.LLMProvider = v
		case "google_api_keys":
			c.GoogleAPIKeys = v
		case "llm_key_rotate_hours":
			if n, err := strconv.Atoi(v); err == nil {
				c.LLMKeyRotateHours = n
			}
		case "anthropic_api_key":
			c.AnthropicAPIKey = v
		case "anthropic_model":
			c.AnthropicModel = v
		case "context_observations":
			if n, err := strconv.Atoi(v); err == nil {
				c.ContextObservations = n
			}
		case "context_full_count":
			if n, err := strconv.Atoi(v); err == nil {
				c.ContextFullCount = n
			}
		case "context_session_count":
			if n, err := strconv.Atoi(v); err == nil {
				c.ContextSessionCount = n
			}
		case "skip_tools":
			c.SkipTools = v
		case "allowed_projects":
			c.AllowedProjects = v
		case "ignored_projects":
			c.IgnoredProjects = v
		case "log_level":
			c.LogLevel = v
		case "processing_paused":
			c.ProcessingPaused = strings.EqualFold(v, "true")
		case "sync_enabled":
			c.SyncEnabled = strings.EqualFold(v, "true")
		case "sync_url":
			c.SyncURL = v
		case "public_base_url":
			c.PublicBaseURL = v
		case "sync_interval":
			c.SyncInterval = v
		case "api_key":
			c.APIKey = v
		case "machine_id":
			c.MachineID = v
		}
	}
}

// snapshot returns a mutex-free copy for safe marshaling. Must be called under lock.
func (c *Config) snapshot() ConfigSnapshot {
	return ConfigSnapshot{
		WorkerPort:           c.WorkerPort,
		DataDir:              c.DataDir,
		LogLevel:             c.LogLevel,
		DatabaseURL:          c.DatabaseURL,
		GeminiAPIKey:         c.GeminiAPIKey,
		GeminiModel:          c.GeminiModel,
		GraphGeminiModel:     c.GraphGeminiModel,
		GeminiEmbeddingModel: c.GeminiEmbeddingModel,
		GeminiEmbeddingDims:  c.GeminiEmbeddingDims,
		LLMProvider:          c.LLMProvider,
		GoogleAPIKeys:        c.GoogleAPIKeys,
		LLMKeyRotateHours:    c.LLMKeyRotateHours,
		AnthropicAPIKey:      c.AnthropicAPIKey,
		AnthropicModel:       c.AnthropicModel,
		ContextObservations:  c.ContextObservations,
		ContextFullCount:     c.ContextFullCount,
		ContextSessionCount:  c.ContextSessionCount,
		SkipTools:            c.SkipTools,
		AllowedProjects:      c.AllowedProjects,
		IgnoredProjects:      c.IgnoredProjects,
		ProcessingPaused:     c.ProcessingPaused,
		SyncEnabled:          c.SyncEnabled,
		SyncURL:              c.SyncURL,
		PublicBaseURL:        c.PublicBaseURL,
		SyncInterval:         c.SyncInterval,
		APIKey:               c.APIKey,
		MachineID:            c.MachineID,
		Graph:                c.Graph,
	}
}

// ConfigSnapshot is a plain struct without mutex for safe JSON marshaling and reading.
type ConfigSnapshot struct {
	WorkerPort           int    `json:"worker_port"`
	DataDir              string `json:"data_dir"`
	LogLevel             string `json:"log_level"`
	DatabaseURL          string `json:"database_url"`
	GeminiAPIKey         string `json:"gemini_api_key"`
	GeminiModel          string `json:"gemini_model"`
	GraphGeminiModel     string `json:"graph_gemini_model"` // graph judge/describe model; empty = use GeminiModel (flat memory keeps its tuned model)
	GeminiEmbeddingModel string `json:"gemini_embedding_model"`
	GeminiEmbeddingDims  int    `json:"gemini_embedding_dims"`

	// LLMProvider picks the gemini-client backend: "openrouter" (default; uses
	// GeminiAPIKey/sk-or) or "google" (direct Gemini API; uses GoogleAPIKeys/AIza).
	// Flip + restart the worker to fail over when OpenRouter is out of quota.
	LLMProvider         string      `json:"llm_provider"`
	GoogleAPIKeys       string      `json:"google_api_keys"`
	LLMKeyRotateHours   int         `json:"llm_key_rotate_hours"`
	AnthropicAPIKey     string      `json:"anthropic_api_key"`
	AnthropicModel      string      `json:"anthropic_model"`
	ContextObservations int         `json:"context_observations"`
	ContextFullCount    int         `json:"context_full_count"`
	ContextSessionCount int         `json:"context_session_count"`
	SkipTools           string      `json:"skip_tools"`
	AllowedProjects     string      `json:"allowed_projects"`
	IgnoredProjects     string      `json:"ignored_projects"`
	ProcessingPaused    bool        `json:"processing_paused"`
	SyncEnabled         bool        `json:"sync_enabled"`
	SyncURL             string      `json:"sync_url"`
	PublicBaseURL       string      `json:"public_base_url"`
	SyncInterval        string      `json:"sync_interval"`
	APIKey              string      `json:"api_key"`
	MachineID           string      `json:"machine_id"`
	Graph               GraphConfig `json:"graph"`
}

// Update applies partial updates from a JSON object to the config.
// Only mutable fields are updated; restart-required fields are ignored.
// Returns true if any LLM key/model/provider changed (caller should reinit clients).
func (c *Config) Update(partial map[string]any) (geminiChanged bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	oldKey := c.GeminiAPIKey
	oldModel := c.GeminiModel
	oldGraphModel := c.GraphGeminiModel
	oldEmbModel := c.GeminiEmbeddingModel
	oldEmbDims := c.GeminiEmbeddingDims
	oldProvider := c.LLMProvider
	oldGoogleKeys := c.GoogleAPIKeys
	oldRotateHours := c.LLMKeyRotateHours
	oldAnthropicKey := c.AnthropicAPIKey
	oldAnthropicModel := c.AnthropicModel

	for k, v := range partial {
		switch k {
		case "gemini_api_key":
			if s, ok := v.(string); ok {
				c.GeminiAPIKey = s
			}
		case "gemini_model":
			if s, ok := v.(string); ok {
				c.GeminiModel = s
			}
		case "graph_gemini_model":
			if s, ok := v.(string); ok {
				c.GraphGeminiModel = s
			}
		case "gemini_embedding_model":
			if s, ok := v.(string); ok {
				c.GeminiEmbeddingModel = s
			}
		case "gemini_embedding_dims":
			if n, ok := toInt(v); ok {
				c.GeminiEmbeddingDims = n
			}
		case "llm_provider":
			if s, ok := v.(string); ok {
				c.LLMProvider = s
			}
		case "google_api_keys":
			if s, ok := v.(string); ok {
				c.GoogleAPIKeys = s
			}
		case "llm_key_rotate_hours":
			if n, ok := toInt(v); ok {
				c.LLMKeyRotateHours = n
			}
		case "anthropic_api_key":
			if s, ok := v.(string); ok {
				c.AnthropicAPIKey = s
			}
		case "anthropic_model":
			if s, ok := v.(string); ok {
				c.AnthropicModel = s
			}
		case "allowed_projects":
			if s, ok := v.(string); ok {
				c.AllowedProjects = s
			}
		case "ignored_projects":
			if s, ok := v.(string); ok {
				c.IgnoredProjects = s
			}
		case "skip_tools":
			if s, ok := v.(string); ok {
				c.SkipTools = s
			}
		case "context_observations":
			if n, ok := toInt(v); ok {
				c.ContextObservations = n
			}
		case "context_full_count":
			if n, ok := toInt(v); ok {
				c.ContextFullCount = n
			}
		case "context_session_count":
			if n, ok := toInt(v); ok {
				c.ContextSessionCount = n
			}
		case "log_level":
			if s, ok := v.(string); ok {
				c.LogLevel = s
			}
		case "processing_paused":
			if b, ok := v.(bool); ok {
				c.ProcessingPaused = b
			}
		case "sync_enabled":
			if b, ok := v.(bool); ok {
				c.SyncEnabled = b
			}
		case "sync_url":
			if s, ok := v.(string); ok {
				c.SyncURL = s
			}
		case "public_base_url":
			if s, ok := v.(string); ok {
				c.PublicBaseURL = s
			}
		case "sync_interval":
			if s, ok := v.(string); ok {
				c.SyncInterval = s
			}
		case "api_key":
			if s, ok := v.(string); ok {
				c.APIKey = s
			}
		case "machine_id":
			if s, ok := v.(string); ok {
				c.MachineID = s
			}
		}
	}

	return c.GeminiAPIKey != oldKey ||
		c.GeminiModel != oldModel ||
		c.GraphGeminiModel != oldGraphModel ||
		c.GeminiEmbeddingModel != oldEmbModel ||
		c.GeminiEmbeddingDims != oldEmbDims ||
		c.LLMProvider != oldProvider ||
		c.GoogleAPIKeys != oldGoogleKeys ||
		c.LLMKeyRotateHours != oldRotateHours ||
		c.AnthropicAPIKey != oldAnthropicKey ||
		c.AnthropicModel != oldAnthropicModel
}

func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	}
	return 0, false
}

func defaults() *Config {
	home, _ := os.UserHomeDir()
	return &Config{
		WorkerPort:           34567,
		DataDir:              filepath.Join(home, ".agent-mem"),
		LogLevel:             "info",
		DatabaseURL:          "postgresql://agentmem:agentmem@localhost:5433/agentmem",
		GeminiModel:          "google/gemini-2.5-flash",
		GeminiEmbeddingModel: "google/gemini-embedding-001",
		GeminiEmbeddingDims:  768,
		LLMKeyRotateHours:    6,
		AnthropicModel:       "claude-sonnet-5",
		ContextObservations:  50,
		ContextFullCount:     5,
		ContextSessionCount:  10,
		SkipTools:            "ListMcpResourcesTool,SlashCommand",
		SyncInterval:         "60s",
		Graph: GraphConfig{
			Runner:           "any",
			GHBaseURL:        "https://api.github.com",
			PagerDutyBaseURL: "https://api.pagerduty.com",
			DatadogBaseURL:   "https://api.datadoghq.com",
			SentryBaseURL:    "https://sentry.io",
			Rate: GraphRateConfig{
				Slack:      5,
				Jira:       5,
				Github:     10,
				Confluence: 5,
				Pagerduty:  3,
				Datadog:    3,
				Sentry:     5,
				GWS:        5,
				Gemini:     4,
			},
		},
	}
}

func Load() *Config {
	cfg := defaults()

	// Bootstrap settings come from env vars only.
	// Runtime settings are loaded from PostgreSQL after DB connection (in server.go).
	ApplyEnv(cfg)
	return cfg
}

// ApplyEnv overrides config values from environment variables. Exported for use after DB load.
func ApplyEnv(cfg *Config) {
	if v := os.Getenv("AGENT_MEM_WORKER_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.WorkerPort = n
		}
	}
	if v := os.Getenv("AGENT_MEM_DATA_DIR"); v != "" {
		cfg.DataDir = v
	}
	if v := os.Getenv("AGENT_MEM_LOG_LEVEL"); v != "" {
		cfg.LogLevel = v
	}
	if v := os.Getenv("DATABASE_URL"); v != "" {
		cfg.DatabaseURL = v
	}
	if v := os.Getenv("AGENT_MEM_GEMINI_API_KEY"); v != "" {
		cfg.GeminiAPIKey = v
	} else if v := os.Getenv("GEMINI_API_KEY"); v != "" && cfg.GeminiAPIKey == "" {
		cfg.GeminiAPIKey = v
	}
	if v := os.Getenv("AGENT_MEM_LLM_PROVIDER"); v != "" {
		cfg.LLMProvider = v
	}
	if v := os.Getenv("AGENT_MEM_GOOGLE_API_KEYS"); v != "" {
		cfg.GoogleAPIKeys = v
	}
	// Legacy single-key var: joins the pool (SplitKeys dedupes).
	if v := os.Getenv("AGENT_MEM_GOOGLE_API_KEY"); v != "" {
		cfg.GoogleAPIKeys = strings.TrimSpace(cfg.GoogleAPIKeys + "\n" + v)
	}
	if v := os.Getenv("AGENT_MEM_LLM_KEY_ROTATE_HOURS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.LLMKeyRotateHours = n
		}
	}
	if v := os.Getenv("AGENT_MEM_ANTHROPIC_API_KEY"); v != "" {
		cfg.AnthropicAPIKey = v
	} else if v := os.Getenv("ANTHROPIC_API_KEY"); v != "" && cfg.AnthropicAPIKey == "" {
		cfg.AnthropicAPIKey = v
	}
	if v := os.Getenv("AGENT_MEM_ANTHROPIC_MODEL"); v != "" {
		cfg.AnthropicModel = v
	}
	if v := os.Getenv("AGENT_MEM_GEMINI_MODEL"); v != "" {
		cfg.GeminiModel = v
	}
	if v := os.Getenv("AGENT_MEM_GEMINI_EMBEDDING_MODEL"); v != "" {
		cfg.GeminiEmbeddingModel = v
	}
	if v := os.Getenv("AGENT_MEM_GEMINI_EMBEDDING_DIMS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.GeminiEmbeddingDims = n
		}
	}
	if v := os.Getenv("AGENT_MEM_CONTEXT_OBSERVATIONS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.ContextObservations = n
		}
	}
	if v := os.Getenv("AGENT_MEM_CONTEXT_FULL_COUNT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.ContextFullCount = n
		}
	}
	if v := os.Getenv("AGENT_MEM_CONTEXT_SESSION_COUNT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.ContextSessionCount = n
		}
	}
	if v := os.Getenv("AGENT_MEM_SKIP_TOOLS"); v != "" {
		cfg.SkipTools = v
	}
	if v := os.Getenv("AGENT_MEM_ALLOWED_PROJECTS"); v != "" {
		cfg.AllowedProjects = v
	}
	if v := os.Getenv("AGENT_MEM_IGNORED_PROJECTS"); v != "" {
		cfg.IgnoredProjects = v
	}
	if v := os.Getenv("AGENT_MEM_SYNC_ENABLED"); v != "" {
		cfg.SyncEnabled = strings.EqualFold(v, "true")
	}
	if v := os.Getenv("AGENT_MEM_SYNC_URL"); v != "" {
		cfg.SyncURL = v
	}
	if v := os.Getenv("AGENT_MEM_PUBLIC_BASE_URL"); v != "" {
		cfg.PublicBaseURL = v
	}
	if v := os.Getenv("AGENT_MEM_SYNC_INTERVAL"); v != "" {
		cfg.SyncInterval = v
	}
	if v := os.Getenv("AGENT_MEM_API_KEY"); v != "" {
		cfg.APIKey = v
	}
	if v := os.Getenv("AGENT_MEM_MACHINE_ID"); v != "" {
		cfg.MachineID = v
	}

	// Graph config
	if v := os.Getenv("AGENT_MEM_GRAPH_RUNNER"); v != "" {
		cfg.Graph.Runner = v
	}
	if v := os.Getenv("AGENT_MEM_SLACK_BOT_TOKEN"); v != "" {
		cfg.Graph.SlackBotToken = v
	}
	if v := os.Getenv("AGENT_MEM_SLACK_DM_USER"); v != "" {
		cfg.Graph.SlackDMUserID = v
	}
	if v := os.Getenv("AGENT_MEM_JIRA_EMAIL"); v != "" {
		cfg.Graph.JiraEmail = v
	}
	if v := os.Getenv("AGENT_MEM_JIRA_TOKEN"); v != "" {
		cfg.Graph.JiraToken = v
	}
	if v := os.Getenv("AGENT_MEM_JIRA_BASE_URL"); v != "" {
		cfg.Graph.JiraBaseURL = v
	}
	if v := os.Getenv("AGENT_MEM_GH_TOKEN"); v != "" {
		cfg.Graph.GHToken = v
	}
	if v := os.Getenv("AGENT_MEM_GH_BASE_URL"); v != "" {
		cfg.Graph.GHBaseURL = v
	}
	if v := os.Getenv("AGENT_MEM_CF_TOKEN"); v != "" {
		cfg.Graph.CFToken = v
	}
	if v := os.Getenv("AGENT_MEM_CF_BASE_URL"); v != "" {
		cfg.Graph.CFBaseURL = v
	}
	if v := os.Getenv("AGENT_MEM_PAGERDUTY_TOKEN"); v != "" {
		cfg.Graph.PagerDutyToken = v
	}
	if v := os.Getenv("AGENT_MEM_PAGERDUTY_BASE_URL"); v != "" {
		cfg.Graph.PagerDutyBaseURL = v
	}
	if v := os.Getenv("AGENT_MEM_DATADOG_API_KEY"); v != "" {
		cfg.Graph.DatadogAPIKey = v
	}
	if v := os.Getenv("AGENT_MEM_DATADOG_APP_KEY"); v != "" {
		cfg.Graph.DatadogAppKey = v
	}
	if v := os.Getenv("AGENT_MEM_DATADOG_BASE_URL"); v != "" {
		cfg.Graph.DatadogBaseURL = v
	}
	if v := os.Getenv("AGENT_MEM_SENTRY_AUTH_TOKEN"); v != "" {
		cfg.Graph.SentryAuthToken = v
	}
	if v := os.Getenv("AGENT_MEM_SENTRY_BASE_URL"); v != "" {
		cfg.Graph.SentryBaseURL = v
	}
	if v := os.Getenv("AGENT_MEM_SENTRY_ORG"); v != "" {
		cfg.Graph.SentryOrg = v
	}
	if v := os.Getenv("AGENT_MEM_GWS_SERVICE_KEY_PATH"); v != "" {
		cfg.Graph.GWSServiceKeyPath = v
	}
	if v := os.Getenv("AGENT_MEM_WEGOHUB_TOKEN"); v != "" {
		cfg.Graph.WegoHubToken = v
	}
	if v := os.Getenv("AGENT_MEM_WEGOHUB_BASE_URL"); v != "" {
		cfg.Graph.WegoHubBaseURL = v
	}
	// Rate limits
	if v := os.Getenv("AGENT_MEM_GRAPH_RATE_SLACK"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			cfg.Graph.Rate.Slack = n
		}
	}
	if v := os.Getenv("AGENT_MEM_GRAPH_RATE_JIRA"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			cfg.Graph.Rate.Jira = n
		}
	}
	if v := os.Getenv("AGENT_MEM_GRAPH_RATE_GITHUB"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			cfg.Graph.Rate.Github = n
		}
	}
	if v := os.Getenv("AGENT_MEM_GRAPH_RATE_CONFLUENCE"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			cfg.Graph.Rate.Confluence = n
		}
	}
	if v := os.Getenv("AGENT_MEM_GRAPH_RATE_PAGERDUTY"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			cfg.Graph.Rate.Pagerduty = n
		}
	}
	if v := os.Getenv("AGENT_MEM_GRAPH_RATE_DATADOG"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			cfg.Graph.Rate.Datadog = n
		}
	}
	if v := os.Getenv("AGENT_MEM_GRAPH_RATE_SENTRY"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			cfg.Graph.Rate.Sentry = n
		}
	}
	if v := os.Getenv("AGENT_MEM_GRAPH_RATE_GWS"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			cfg.Graph.Rate.GWS = n
		}
	}
	if v := os.Getenv("AGENT_MEM_GRAPH_RATE_GEMINI"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			cfg.Graph.Rate.Gemini = n
		}
	}
}
