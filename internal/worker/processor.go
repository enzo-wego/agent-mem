package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/agent-mem/agent-mem/internal/database"
	"github.com/agent-mem/agent-mem/internal/gemini"
	"github.com/agent-mem/agent-mem/internal/llmgateway"
)

// llmBackoff is how long the flat-memory loop pauses after a transient LLM
// failure. Without it the 1s ticker would re-claim the requeued message
// immediately and spin against a dead gateway, filling the log and hammering a
// service that is already struggling.
const llmBackoff = 30 * time.Second

// processLoop runs a background loop that picks up pending messages for processing.
func (s *Server) processLoop(ctx context.Context) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	log.Info().Msg("Pending message processor started")
	for {
		select {
		case <-ctx.Done():
			log.Info().Msg("Pending message processor stopped")
			return
		case <-ticker.C:
			// Paused: leave messages queued. They stay claimable, so the backlog
			// drains once the pause lifts — nothing is dropped or failed.
			if s.config.Snapshot().ProcessingPaused {
				continue
			}
			s.processPendingMessages(ctx)
		}
	}
}

// backoffLLM pauses the flat-memory loop after a transient LLM failure,
// returning early if the server is shutting down — a bare 30s sleep would hold
// up every shutdown that happens to land during an LLM outage.
func (s *Server) backoffLLM(ctx context.Context) {
	t := time.NewTimer(llmBackoff)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func (s *Server) processPendingMessages(ctx context.Context) {
	msg, err := s.db.ClaimPendingMessage(ctx)
	if err != nil {
		log.Error().Err(err).Msg("Failed to claim pending message")
		return
	}
	if msg == nil {
		return
	}

	log.Info().
		Int("id", msg.ID).
		Str("type", msg.MessageType).
		Str("session", msg.ContentSessionID).
		Msg("Processing pending message")

	err = s.processMessage(ctx, msg)
	if err != nil {
		// A transient fault must NOT be marked failed: that status is terminal
		// and nothing re-claims it, so an LLM outage would silently discard every
		// observation that arrived during it. Requeue instead and back the loop
		// off, so the backlog simply waits for the LLM to come back.
		if llmgateway.IsRetryable(err) {
			log.Warn().Err(err).Int("id", msg.ID).Msg("LLM unavailable, requeueing message")
			if reqErr := s.db.RequeuePendingMessage(ctx, msg.ID, err.Error()); reqErr != nil {
				log.Error().Err(reqErr).Int("id", msg.ID).Msg("Failed to requeue message")
			}
			s.backoffLLM(ctx)
			return
		}
		log.Error().Err(err).Int("id", msg.ID).Msg("Failed to process message")
		if markErr := s.db.MarkMessageFailed(ctx, msg.ID, err.Error()); markErr != nil {
			log.Error().Err(markErr).Int("id", msg.ID).Msg("Failed to mark message as failed")
		}
		return
	}

	if err := s.db.MarkMessageProcessed(ctx, msg.ID); err != nil {
		log.Error().Err(err).Int("id", msg.ID).Msg("Failed to mark message as processed")
	}
}

// processMessage handles a single pending message by sending it to the LLM.
func (s *Server) processMessage(ctx context.Context, msg *database.PendingMessage) error {
	if s.getFlatLLM() == nil {
		log.Debug().Int("id", msg.ID).Msg("No LLM configured, skipping")
		return nil
	}

	switch msg.MessageType {
	case "observation":
		return s.processObservation(ctx, msg)
	case "summary":
		return s.processSummary(ctx, msg)
	default:
		log.Warn().Str("type", msg.MessageType).Msg("Unknown message type")
		return nil
	}
}

// processObservation extracts a structured observation via Gemini and stores it with an embedding.
func (s *Server) processObservation(ctx context.Context, msg *database.PendingMessage) error {
	gc := s.getFlatLLM()
	if gc == nil {
		return nil
	}

	// Parse payload
	var payload struct {
		ToolName     string          `json:"tool_name"`
		ToolInput    json.RawMessage `json:"tool_input"`
		ToolResponse json.RawMessage `json:"tool_response"`
		CWD          string          `json:"cwd"`
		Project      string          `json:"project"`
	}
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		return fmt.Errorf("parse observation payload: %w", err)
	}

	// Look up the session to get memory_session_id
	session, err := s.db.FindSessionByContentID(ctx, msg.ContentSessionID)
	if err != nil {
		return fmt.Errorf("find session: %w", err)
	}
	if session.MemorySessionID == nil {
		return fmt.Errorf("session %s has no memory_session_id", msg.ContentSessionID)
	}

	// Build prompt and call Gemini
	userMsg := gemini.BuildObservationPrompt(payload.ToolName, payload.ToolInput, payload.ToolResponse, payload.CWD, payload.Project)
	// Cheap tier: this runs once per inbound message, the highest-volume LLM path
	// in the service. It ran on gemini-2.5-flash before the gateway migration
	// silently moved it to Sonnet; haiku restores the original class.
	response, err := gc.GenerateCheap(ctx, gemini.ObservationSystemPrompt, userMsg)
	if err != nil {
		return fmt.Errorf("gemini generate: %w", err)
	}

	// Parse observation
	obs, err := gemini.ParseObservation(response)
	if err != nil {
		return fmt.Errorf("parse gemini response: %w", err)
	}

	// Skip trivial observations
	if obs.Skip {
		log.Debug().Str("tool", payload.ToolName).Msg("Trivial tool use, skipping observation")
		return nil
	}

	// Generate embedding
	embeddingText := gemini.BuildEmbeddingText(obs.Title, obs.Subtitle, obs.Narrative, obs.Facts)
	embedding, err := gc.Embed(ctx, embeddingText)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to generate embedding, storing without it")
		embedding = nil
	}

	// Store observation
	id, err := s.db.StoreObservation(ctx,
		*session.MemorySessionID, payload.Project,
		obs.Type, obs.Title, obs.Subtitle, obs.Narrative,
		obs.Facts, obs.Concepts, obs.FilesRead, obs.FilesModified,
		embedding,
	)
	if err != nil {
		return fmt.Errorf("store observation: %w", err)
	}

	log.Info().
		Int64("id", id).
		Str("type", obs.Type).
		Str("title", obs.Title).
		Str("tool", payload.ToolName).
		Msg("Observation stored")

	return nil
}

// processSummary extracts a session summary via Gemini and stores it with an embedding.
func (s *Server) processSummary(ctx context.Context, msg *database.PendingMessage) error {
	gc := s.getFlatLLM()
	if gc == nil {
		return nil
	}

	// Parse payload
	var payload struct {
		LastAssistantMessage string `json:"last_assistant_message"`
		Project              string `json:"project"`
	}
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		return fmt.Errorf("parse summary payload: %w", err)
	}

	// Look up the session
	session, err := s.db.FindSessionByContentID(ctx, msg.ContentSessionID)
	if err != nil {
		return fmt.Errorf("find session: %w", err)
	}
	if session.MemorySessionID == nil {
		return fmt.Errorf("session %s has no memory_session_id", msg.ContentSessionID)
	}

	// Build prompt and call Gemini
	userMsg := gemini.BuildSummaryPrompt(payload.LastAssistantMessage, payload.Project)
	// Cheap tier, same reasoning as processObservation: session summaries are
	// routine flat-memory volume, not graph analysis.
	response, err := gc.GenerateCheap(ctx, gemini.SummarySystemPrompt, userMsg)
	if err != nil {
		return fmt.Errorf("gemini generate: %w", err)
	}

	// Parse summary
	summary, err := gemini.ParseSummary(response)
	if err != nil {
		return fmt.Errorf("parse gemini response: %w", err)
	}

	// Generate embedding
	embeddingText := gemini.BuildSummaryEmbeddingText(summary)
	embedding, err := gc.Embed(ctx, embeddingText)
	if err != nil {
		log.Warn().Err(err).Msg("Failed to generate summary embedding, storing without it")
		embedding = nil
	}

	// Store summary
	id, err := s.db.StoreSummary(ctx,
		*session.MemorySessionID, payload.Project,
		summary.Request, summary.Investigated, summary.Learned, summary.Completed, summary.NextSteps,
		embedding,
	)
	if err != nil {
		return fmt.Errorf("store summary: %w", err)
	}

	log.Info().
		Int64("id", id).
		Str("session", msg.ContentSessionID).
		Msg("Session summary stored")

	return nil
}
