package worker

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/rs/zerolog/log"
)

func (s *Server) handleGetLogs(w http.ResponseWriter, r *http.Request) {
	if s.logBuffer == nil {
		http.Error(w, `{"error":"log buffer not configured"}`, http.StatusServiceUnavailable)
		return
	}

	entries := s.logBuffer.Entries()

	// Optional level filter
	level := r.URL.Query().Get("level")

	// Optional tail (return last N entries)
	tail := 0
	if n, err := strconv.Atoi(r.URL.Query().Get("tail")); err == nil && n > 0 {
		tail = n
	}

	// Filter by level
	if level != "" {
		filtered := make([]LogEntry, 0, len(entries))
		for _, e := range entries {
			if matchLevel(e.Level, level) {
				filtered = append(filtered, e)
			}
		}
		entries = filtered
	}

	// Apply tail
	if tail > 0 && tail < len(entries) {
		entries = entries[len(entries)-tail:]
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{
		"entries": entries,
		"total":   len(entries),
	}); err != nil {
		log.Error().Err(err).Msg("Failed to encode logs response")
	}
}

// matchLevel returns true if the entry level is >= the filter level.
func matchLevel(entryLevel, filterLevel string) bool {
	levels := map[string]int{"trace": 0, "debug": 1, "info": 2, "warn": 3, "error": 4, "fatal": 5}
	el, ok1 := levels[entryLevel]
	fl, ok2 := levels[filterLevel]
	if !ok1 || !ok2 {
		return true
	}
	return el >= fl
}
