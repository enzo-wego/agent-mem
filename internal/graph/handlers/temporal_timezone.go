package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog/log"
)

// temporalTimezoneKey is the settings row naming the IANA zone that "today",
// "yesterday", "this month" and date-only since/until are resolved in.
const temporalTimezoneKey = "graph.temporal.timezone"

// defaultTemporalTimezone applies when the row is absent, unreadable or blank.
const defaultTemporalTimezone = "Asia/Ho_Chi_Minh"

var badTimezoneWarn sync.Once

// storedTemporalTimezone is the configured value, or the default when the
// row is absent, unreadable, or blank.
func storedTemporalTimezone(ctx context.Context, db *pgxpool.Pool) string {
	v := strings.TrimSpace(loadSetting(ctx, db, temporalTimezoneKey))
	if v == "" {
		return defaultTemporalTimezone
	}
	return v
}

// temporalLocation resolves the zone time windows are built in. A stored name
// time.LoadLocation rejects falls back to UTC (warned once per process).
// Blank is checked before LoadLocation, which maps "" to UTC.
func temporalLocation(ctx context.Context, db *pgxpool.Pool) *time.Location {
	name := storedTemporalTimezone(ctx, db)
	loc, err := time.LoadLocation(name)
	if err != nil {
		badTimezoneWarn.Do(func() {
			log.Warn().Err(err).Str("timezone", name).Msg("invalid " + temporalTimezoneKey + "; using UTC")
		})
		return time.UTC
	}
	return loc
}

type temporalTimezoneConfig struct {
	Timezone  string `json:"timezone"`
	Effective string `json:"effective,omitempty"`
}

// getTemporalTimezone serves GET /api/graph/temporal-timezone.
func (h *Channels) getTemporalTimezone(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(temporalTimezoneConfig{
		Timezone:  storedTemporalTimezone(r.Context(), h.db),
		Effective: temporalLocation(r.Context(), h.db).String(),
	})
}

// putTemporalTimezone serves PUT /api/graph/temporal-timezone. Read per
// search request; no cache to invalidate.
func (h *Channels) putTemporalTimezone(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
	var cfg temporalTimezoneConfig
	if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	cfg.Timezone = strings.TrimSpace(cfg.Timezone)
	if cfg.Timezone == "" {
		writeError(w, http.StatusBadRequest, "timezone required")
		return
	}
	if _, err := time.LoadLocation(cfg.Timezone); err != nil {
		writeError(w, http.StatusBadRequest, "unknown timezone "+cfg.Timezone)
		return
	}
	if _, err := h.db.Exec(r.Context(), `
		INSERT INTO settings(key,value) VALUES($1,$2)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, temporalTimezoneKey, cfg.Timezone); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.getTemporalTimezone(w, r)
}
