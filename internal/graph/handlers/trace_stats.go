package handlers

import (
	"encoding/json"
	"net/http"

	graphtrace "github.com/agent-mem/agent-mem/internal/graph/trace"
)

func traceStatsHandler(recorder *graphtrace.Recorder) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(recorder.Stats())
	}
}
