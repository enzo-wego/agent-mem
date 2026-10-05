package handlers

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
)

type slackMembersConfig struct {
	IntervalMinutes int `json:"interval_minutes"`
}

// getSlackMembersConfig serves GET /api/graph/slack-members using the same
// persisted interval read by the periodic ticker and refresh due gate.
func (h *Channels) getSlackMembersConfig(w http.ResponseWriter, r *http.Request) {
	interval, err := readSlackMembersInterval(r.Context(), h.db)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(slackMembersConfig{IntervalMinutes: interval})
}

// putSlackMembersConfig serves PUT /api/graph/slack-members. The ticker reads
// settings each minute; saving changes cadence without scheduling a new chain.
func (h *Channels) putSlackMembersConfig(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
	decoder := json.NewDecoder(r.Body)
	var cfg slackMembersConfig
	if err := decoder.Decode(&cfg); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}
	if cfg.IntervalMinutes < 15 || cfg.IntervalMinutes > 480 {
		writeError(w, http.StatusBadRequest, "interval_minutes must be an integer from 15 to 480")
		return
	}
	if _, err := h.db.Exec(r.Context(), `
		INSERT INTO settings(key,value) VALUES($1,$2)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, slackMembersIntervalKey, strconv.Itoa(cfg.IntervalMinutes)); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.getSlackMembersConfig(w, r)
}
