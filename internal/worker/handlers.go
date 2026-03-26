package worker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/rs/zerolog/log"

	"github.com/agent-mem/agent-mem/internal/gemini"
)

// hookPayload represents the JSON received from hook CLI subcommands.
type hookPayload struct {
	SessionID      string          `json:"session_id"`
	CWD            string          `json:"cwd"`
	Prompt         string          `json:"prompt"`
	ToolName       string          `json:"tool_name"`
	ToolInput      json.RawMessage `json:"tool_input"`
	ToolResponse   json.RawMessage `json:"tool_response"`
	TranscriptPath string          `json:"transcript_path"`
}

// handleHealth returns worker health status.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	err := s.db.Pool.Ping(ctx)

	status := map[string]any{
		"status":   "ok",
		"postgres": err == nil,
	}
	if err != nil {
		status["status"] = "degraded"
		status["error"] = err.Error()
	}

	pending, _ := s.db.PendingMessageCount(ctx)
	status["pending_messages"] = pending

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(status)
}

// handleSessionStart handles the SessionStart hook.
// Returns markdown context for injection into Claude's system prompt.
func (s *Server) handleSessionStart(w http.ResponseWriter, r *http.Request) {
	var payload hookPayload
	if err := readPayload(r, &payload); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	project := extractProject(payload.CWD)
	if project == "" {
		w.WriteHeader(http.StatusOK)
		return
	}

	if !s.isProjectAllowed(project) {
		log.Debug().Str("project", project).Msg("Project filtered, skipping session-start")
		w.WriteHeader(http.StatusOK)
		return
	}

	// Build and inject context from past observations
	contextMD, err := s.contextBld.BuildContext(r.Context(), project)
	if err != nil {
		log.Warn().Err(err).Str("project", project).Msg("Failed to build context")
		w.WriteHeader(http.StatusOK)
		return
	}

	log.Info().Str("project", project).Int("context_len", len(contextMD)).Msg("Session started")

	if contextMD != "" {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte(contextMD))
		return
	}
	w.WriteHeader(http.StatusOK)
}

// handlePromptSubmit handles the UserPromptSubmit hook.
// Stores the user prompt and returns immediately.
func (s *Server) handlePromptSubmit(w http.ResponseWriter, r *http.Request) {
	var payload hookPayload
	if err := readPayload(r, &payload); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if payload.SessionID == "" || payload.CWD == "" {
		http.Error(w, "missing session_id or cwd", http.StatusBadRequest)
		return
	}

	project := extractProject(payload.CWD)
	if !s.isProjectAllowed(project) {
		w.WriteHeader(http.StatusOK)
		return
	}

	// Ensure session exists
	_, err := s.db.UpsertSession(r.Context(), payload.SessionID, project)
	if err != nil {
		log.Error().Err(err).Msg("Failed to upsert session")
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Store prompt
	if payload.Prompt != "" {
		id, num, err := s.db.StorePrompt(r.Context(), payload.SessionID, project, payload.Prompt)
		if err != nil {
			log.Error().Err(err).Msg("Failed to store prompt")
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		log.Info().Int64("id", id).Int("prompt_number", num).Str("project", project).Msg("Prompt stored")

		// Async: generate and store prompt embedding
		if gc := s.getGemini(); gc != nil {
			go func(promptID int64, promptText string, gc *gemini.Client) {
				embedding, err := gc.Embed(context.Background(), promptText)
				if err != nil {
					log.Warn().Err(err).Int64("prompt_id", promptID).Msg("Failed to generate prompt embedding")
					return
				}
				if err := s.db.UpdatePromptEmbedding(context.Background(), promptID, embedding); err != nil {
					log.Warn().Err(err).Int64("prompt_id", promptID).Msg("Failed to update prompt embedding")
				}
			}(id, payload.Prompt, gc)
		}
	}

	w.WriteHeader(http.StatusOK)
}

// handlePostToolUse handles the PostToolUse hook.
// Queues a pending message for async Gemini extraction.
func (s *Server) handlePostToolUse(w http.ResponseWriter, r *http.Request) {
	var payload hookPayload
	if err := readPayload(r, &payload); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if payload.SessionID == "" || payload.CWD == "" {
		http.Error(w, "missing session_id or cwd", http.StatusBadRequest)
		return
	}

	// Check skip tools filter
	if s.isToolSkipped(payload.ToolName) {
		w.WriteHeader(http.StatusOK)
		return
	}

	project := extractProject(payload.CWD)
	if !s.isProjectAllowed(project) {
		w.WriteHeader(http.StatusOK)
		return
	}

	// Ensure session exists
	_, err := s.db.UpsertSession(r.Context(), payload.SessionID, project)
	if err != nil {
		log.Error().Err(err).Msg("Failed to upsert session")
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Queue pending message for Gemini processing (Phase 03)
	msgPayload, _ := json.Marshal(map[string]any{
		"tool_name":     payload.ToolName,
		"tool_input":    payload.ToolInput,
		"tool_response": payload.ToolResponse,
		"cwd":           payload.CWD,
		"project":       project,
	})

	id, err := s.db.QueuePendingMessage(r.Context(), payload.SessionID, "observation", msgPayload)
	if err != nil {
		log.Error().Err(err).Msg("Failed to queue pending message")
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	log.Debug().Int64("id", id).Str("tool", payload.ToolName).Msg("Tool use queued")
	w.WriteHeader(http.StatusOK)
}

// handleStop handles the Stop hook.
// Reads the transcript, queues a summary job, and marks the session completed.
func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	var payload hookPayload
	if err := readPayload(r, &payload); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if payload.SessionID == "" {
		http.Error(w, "missing session_id", http.StatusBadRequest)
		return
	}

	project := extractProject(payload.CWD)
	if !s.isProjectAllowed(project) {
		w.WriteHeader(http.StatusOK)
		return
	}

	// Extract last assistant message from transcript
	var lastMessage string
	if payload.TranscriptPath != "" {
		msg, err := extractLastAssistantMessage(payload.TranscriptPath)
		if err != nil {
			log.Warn().Err(err).Str("path", payload.TranscriptPath).Msg("Failed to read transcript")
		} else {
			lastMessage = msg
		}
	}

	// Queue summary job if we have content
	if lastMessage != "" {
		msgPayload, _ := json.Marshal(map[string]any{
			"last_assistant_message": lastMessage,
			"project":               project,
		})

		_, err := s.db.QueuePendingMessage(r.Context(), payload.SessionID, "summary", msgPayload)
		if err != nil {
			log.Error().Err(err).Msg("Failed to queue summary")
		}
	}

	// Mark session completed
	if err := s.db.CompleteSession(r.Context(), payload.SessionID); err != nil {
		log.Warn().Err(err).Msg("Failed to complete session")
	}

	log.Info().Str("session", payload.SessionID).Msg("Session stopped")
	w.WriteHeader(http.StatusOK)
}

// readPayload reads and decodes the JSON request body.
func readPayload(r *http.Request, v any) error {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	defer r.Body.Close()

	if len(body) == 0 {
		return nil
	}
	return json.Unmarshal(body, v)
}
