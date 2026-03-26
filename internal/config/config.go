package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

type Config struct {
	mu sync.RWMutex `json:"-"`

	WorkerPort  int    `json:"worker_port"`
	DataDir     string `json:"data_dir"`
	LogLevel    string `json:"log_level"`
	DatabaseURL string `json:"database_url"`

	GeminiAPIKey         string `json:"gemini_api_key"`
	GeminiModel          string `json:"gemini_model"`
	GeminiEmbeddingModel string `json:"gemini_embedding_model"`
	GeminiEmbeddingDims  int    `json:"gemini_embedding_dims"`

	ContextObservations int    `json:"context_observations"`
	ContextFullCount    int    `json:"context_full_count"`
	ContextSessionCount int    `json:"context_session_count"`
	SkipTools           string `json:"skip_tools"`

	AllowedProjects string `json:"allowed_projects"`
	IgnoredProjects string `json:"ignored_projects"`

	SyncEnabled  bool   `json:"sync_enabled"`
	SyncURL      string `json:"sync_url"`
	SyncInterval string `json:"sync_interval"`
	APIKey       string `json:"api_key"`
	MachineID    string `json:"machine_id"`
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

// RuntimeSettings returns the runtime settings as a string map for DB storage.
func (c *Config) RuntimeSettings() map[string]string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return map[string]string{
		"gemini_api_key":         c.GeminiAPIKey,
		"gemini_model":           c.GeminiModel,
		"gemini_embedding_model": c.GeminiEmbeddingModel,
		"gemini_embedding_dims":  strconv.Itoa(c.GeminiEmbeddingDims),
		"context_observations":   strconv.Itoa(c.ContextObservations),
		"context_full_count":     strconv.Itoa(c.ContextFullCount),
		"context_session_count":  strconv.Itoa(c.ContextSessionCount),
		"skip_tools":             c.SkipTools,
		"allowed_projects":       c.AllowedProjects,
		"ignored_projects":       c.IgnoredProjects,
		"log_level":              c.LogLevel,
		"sync_enabled":           strconv.FormatBool(c.SyncEnabled),
		"sync_url":               c.SyncURL,
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
		case "gemini_embedding_model":
			c.GeminiEmbeddingModel = v
		case "gemini_embedding_dims":
			if n, err := strconv.Atoi(v); err == nil {
				c.GeminiEmbeddingDims = n
			}
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
		case "sync_enabled":
			c.SyncEnabled = strings.EqualFold(v, "true")
		case "sync_url":
			c.SyncURL = v
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
		GeminiEmbeddingModel: c.GeminiEmbeddingModel,
		GeminiEmbeddingDims:  c.GeminiEmbeddingDims,
		ContextObservations:  c.ContextObservations,
		ContextFullCount:     c.ContextFullCount,
		ContextSessionCount:  c.ContextSessionCount,
		SkipTools:            c.SkipTools,
		AllowedProjects:      c.AllowedProjects,
		IgnoredProjects:      c.IgnoredProjects,
		SyncEnabled:          c.SyncEnabled,
		SyncURL:              c.SyncURL,
		SyncInterval:         c.SyncInterval,
		APIKey:               c.APIKey,
		MachineID:            c.MachineID,
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
	GeminiEmbeddingModel string `json:"gemini_embedding_model"`
	GeminiEmbeddingDims  int    `json:"gemini_embedding_dims"`
	ContextObservations  int    `json:"context_observations"`
	ContextFullCount     int    `json:"context_full_count"`
	ContextSessionCount  int    `json:"context_session_count"`
	SkipTools            string `json:"skip_tools"`
	AllowedProjects      string `json:"allowed_projects"`
	IgnoredProjects      string `json:"ignored_projects"`
	SyncEnabled          bool   `json:"sync_enabled"`
	SyncURL              string `json:"sync_url"`
	SyncInterval         string `json:"sync_interval"`
	APIKey               string `json:"api_key"`
	MachineID            string `json:"machine_id"`
}

// Update applies partial updates from a JSON object to the config.
// Only mutable fields are updated; restart-required fields are ignored.
// Returns true if the Gemini API key or model changed (caller should reinit client).
func (c *Config) Update(partial map[string]any) (geminiChanged bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	oldKey := c.GeminiAPIKey
	oldModel := c.GeminiModel
	oldEmbModel := c.GeminiEmbeddingModel
	oldEmbDims := c.GeminiEmbeddingDims

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
		case "gemini_embedding_model":
			if s, ok := v.(string); ok {
				c.GeminiEmbeddingModel = s
			}
		case "gemini_embedding_dims":
			if n, ok := toInt(v); ok {
				c.GeminiEmbeddingDims = n
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
		case "sync_enabled":
			if b, ok := v.(bool); ok {
				c.SyncEnabled = b
			}
		case "sync_url":
			if s, ok := v.(string); ok {
				c.SyncURL = s
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
		c.GeminiEmbeddingModel != oldEmbModel ||
		c.GeminiEmbeddingDims != oldEmbDims
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
		GeminiModel:          "gemini-2.5-flash",
		GeminiEmbeddingModel: "gemini-embedding-001",
		GeminiEmbeddingDims:  768,
		ContextObservations:  50,
		ContextFullCount:     5,
		ContextSessionCount:  10,
		SkipTools:            "ListMcpResourcesTool,SlashCommand",
		SyncInterval:         "60s",
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
	if v := os.Getenv("AGENT_MEM_SYNC_INTERVAL"); v != "" {
		cfg.SyncInterval = v
	}
	if v := os.Getenv("AGENT_MEM_API_KEY"); v != "" {
		cfg.APIKey = v
	}
	if v := os.Getenv("AGENT_MEM_MACHINE_ID"); v != "" {
		cfg.MachineID = v
	}
}
