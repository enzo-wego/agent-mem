package worker

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/rs/zerolog/log"
)

type searchResponse struct {
	Results []searchResultJSON `json:"results"`
	Query   string             `json:"query"`
	Total   int                `json:"total"`
}

type searchResultJSON struct {
	ID            int     `json:"id"`
	Type          string  `json:"type"`
	Title         string  `json:"title"`
	Subtitle      string  `json:"subtitle,omitempty"`
	Narrative     string  `json:"narrative,omitempty"`
	Project       string  `json:"project"`
	CreatedAt     string  `json:"created_at"`
	CombinedScore float64 `json:"combined_score,omitempty"`
}

// handleSearch performs hybrid search across observations and summaries.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query().Get("q")
	project := r.URL.Query().Get("project")
	limitStr := r.URL.Query().Get("limit")

	if query == "" {
		http.Error(w, "missing q parameter", http.StatusBadRequest)
		return
	}

	limit := 10
	if limitStr != "" {
		if n, err := strconv.Atoi(limitStr); err == nil && n > 0 {
			limit = n
		}
	}

	srch := s.getSearcher()
	if srch == nil {
		http.Error(w, "search not available (no Gemini client)", http.StatusServiceUnavailable)
		return
	}

	results, err := srch.Search(r.Context(), query, project, limit)
	if err != nil {
		log.Error().Err(err).Str("query", query).Msg("Search failed")
		http.Error(w, "search error", http.StatusInternalServerError)
		return
	}

	resp := searchResponse{
		Query: query,
		Total: len(results),
	}
	for _, r := range results {
		resp.Results = append(resp.Results, searchResultJSON{
			ID:            r.ID,
			Type:          r.Type,
			Title:         r.Title,
			Subtitle:      r.Subtitle,
			Narrative:     r.Narrative,
			Project:       r.Project,
			CreatedAt:     r.CreatedAt.Format(time.RFC3339),
			CombinedScore: r.CombinedScore,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// handleSearchByFile searches observations by file path.
func (s *Server) handleSearchByFile(w http.ResponseWriter, r *http.Request) {
	filePath := r.URL.Query().Get("path")
	project := r.URL.Query().Get("project")

	if filePath == "" || project == "" {
		http.Error(w, "missing path or project parameter", http.StatusBadRequest)
		return
	}

	limit := 20
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 {
		limit = n
	}

	results, err := s.db.SearchByFile(r.Context(), filePath, project, limit)
	if err != nil {
		log.Error().Err(err).Msg("File search failed")
		http.Error(w, "search error", http.StatusInternalServerError)
		return
	}

	resp := searchResponse{Query: filePath, Total: len(results)}
	for _, r := range results {
		resp.Results = append(resp.Results, searchResultJSON{
			ID:        r.ID,
			Type:      r.Type,
			Title:     r.Title,
			Narrative: r.Narrative,
			Project:   r.Project,
			CreatedAt: r.CreatedAt.Format(time.RFC3339),
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// handleSearchTimeline returns observations within a date range.
func (s *Server) handleSearchTimeline(w http.ResponseWriter, r *http.Request) {
	project := r.URL.Query().Get("project")
	fromStr := r.URL.Query().Get("from")
	toStr := r.URL.Query().Get("to")

	if project == "" {
		http.Error(w, "missing project parameter", http.StatusBadRequest)
		return
	}

	var fromEpoch, toEpoch int64
	if fromStr != "" {
		if t, err := time.Parse("2006-01-02", fromStr); err == nil {
			fromEpoch = t.Unix()
		}
	}
	if toStr != "" {
		if t, err := time.Parse("2006-01-02", toStr); err == nil {
			toEpoch = t.Add(24*time.Hour - time.Second).Unix()
		}
	} else {
		toEpoch = time.Now().Unix()
	}

	limit := 50
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 {
		limit = n
	}

	results, err := s.db.SearchTimeline(r.Context(), project, fromEpoch, toEpoch, limit)
	if err != nil {
		log.Error().Err(err).Msg("Timeline search failed")
		http.Error(w, "search error", http.StatusInternalServerError)
		return
	}

	resp := searchResponse{Query: project, Total: len(results)}
	for _, r := range results {
		resp.Results = append(resp.Results, searchResultJSON{
			ID:        r.ID,
			Type:      r.Type,
			Title:     r.Title,
			Narrative: r.Narrative,
			Project:   r.Project,
			CreatedAt: r.CreatedAt.Format(time.RFC3339),
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// handleListObservations lists observations filtered by project and type.
func (s *Server) handleListObservations(w http.ResponseWriter, r *http.Request) {
	project := r.URL.Query().Get("project")
	obsType := r.URL.Query().Get("type")

	if project == "" {
		http.Error(w, "missing project parameter", http.StatusBadRequest)
		return
	}

	limit := 20
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 {
		limit = n
	}

	results, err := s.db.ListObservations(r.Context(), project, obsType, limit)
	if err != nil {
		log.Error().Err(err).Msg("List observations failed")
		http.Error(w, "error", http.StatusInternalServerError)
		return
	}

	resp := searchResponse{Total: len(results)}
	for _, r := range results {
		resp.Results = append(resp.Results, searchResultJSON{
			ID:        r.ID,
			Type:      r.Type,
			Title:     r.Title,
			Subtitle:  r.Subtitle,
			Narrative: r.Narrative,
			Project:   r.Project,
			CreatedAt: r.CreatedAt.Format(time.RFC3339),
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// summaryJSON is the JSON representation of a session summary for the dashboard.
type summaryJSON struct {
	ID              int    `json:"id"`
	MemorySessionID string `json:"memory_session_id"`
	Project         string `json:"project"`
	Request         string `json:"request,omitempty"`
	Investigated    string `json:"investigated,omitempty"`
	Learned         string `json:"learned,omitempty"`
	Completed       string `json:"completed,omitempty"`
	NextSteps       string `json:"next_steps,omitempty"`
	Notes           string `json:"notes,omitempty"`
	CreatedAt       string `json:"created_at"`
}

type summariesResponse struct {
	Summaries []summaryJSON `json:"summaries"`
	Total     int           `json:"total"`
}

// handleListSummaries returns session summaries for a project.
func (s *Server) handleListSummaries(w http.ResponseWriter, r *http.Request) {
	project := r.URL.Query().Get("project")
	if project == "" {
		http.Error(w, "missing project parameter", http.StatusBadRequest)
		return
	}

	limit := 20
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 {
		limit = n
	}

	summaries, err := s.db.ListSummaries(r.Context(), project, limit)
	if err != nil {
		log.Error().Err(err).Msg("List summaries failed")
		http.Error(w, "error", http.StatusInternalServerError)
		return
	}

	resp := summariesResponse{Total: len(summaries)}
	for _, ss := range summaries {
		var req, inv, lrn, comp, ns, notes string
		if ss.Request != nil {
			req = *ss.Request
		}
		if ss.Investigated != nil {
			inv = *ss.Investigated
		}
		if ss.Learned != nil {
			lrn = *ss.Learned
		}
		if ss.Completed != nil {
			comp = *ss.Completed
		}
		if ss.NextSteps != nil {
			ns = *ss.NextSteps
		}
		if ss.Notes != nil {
			notes = *ss.Notes
		}
		resp.Summaries = append(resp.Summaries, summaryJSON{
			ID:              ss.ID,
			MemorySessionID: ss.MemorySessionID,
			Project:         ss.Project,
			Request:         req,
			Investigated:    inv,
			Learned:         lrn,
			Completed:       comp,
			NextSteps:       ns,
			Notes:           notes,
			CreatedAt:       ss.CreatedAt.Format(time.RFC3339),
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// promptJSON is the JSON representation of a user prompt for the dashboard.
type promptJSON struct {
	ID               int    `json:"id"`
	ContentSessionID string `json:"content_session_id"`
	Project          string `json:"project"`
	Prompt           string `json:"prompt"`
	PromptNumber     int    `json:"prompt_number"`
	CreatedAt        string `json:"created_at"`
}

type promptsResponse struct {
	Prompts []promptJSON `json:"prompts"`
	Total   int          `json:"total"`
}

// handleListPrompts returns user prompts for a project.
func (s *Server) handleListPrompts(w http.ResponseWriter, r *http.Request) {
	project := r.URL.Query().Get("project")
	if project == "" {
		http.Error(w, "missing project parameter", http.StatusBadRequest)
		return
	}

	limit := 50
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 {
		limit = n
	}

	prompts, err := s.db.ListPrompts(r.Context(), project, limit)
	if err != nil {
		log.Error().Err(err).Msg("List prompts failed")
		http.Error(w, "error", http.StatusInternalServerError)
		return
	}

	resp := promptsResponse{Total: len(prompts)}
	for _, p := range prompts {
		resp.Prompts = append(resp.Prompts, promptJSON{
			ID:               p.ID,
			ContentSessionID: p.ContentSessionID,
			Project:          p.Project,
			Prompt:           p.Prompt,
			PromptNumber:     p.PromptNumber,
			CreatedAt:        p.CreatedAt.Format(time.RFC3339),
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// handleStats returns aggregate counts for observations, summaries, and prompts.
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	project := r.URL.Query().Get("project")
	stats, err := s.db.GetStats(r.Context(), project)
	if err != nil {
		http.Error(w, "error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(stats)
}

// handleListProjects returns all projects with observation counts.
func (s *Server) handleListProjects(w http.ResponseWriter, r *http.Request) {
	projects, err := s.db.ListProjects(r.Context())
	if err != nil {
		log.Error().Err(err).Msg("List projects failed")
		http.Error(w, "error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(projects)
}

// handleGetObservation returns a single observation by ID.
func (s *Server) handleGetObservation(w http.ResponseWriter, r *http.Request) {
	idStr := chi.URLParam(r, "id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	obs, err := s.db.GetObservationByID(r.Context(), id)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	// Parse JSONB arrays for clean response
	var facts, concepts, filesRead, filesModified []string
	json.Unmarshal(obs.Facts, &facts)
	json.Unmarshal(obs.Concepts, &concepts)
	json.Unmarshal(obs.FilesRead, &filesRead)
	json.Unmarshal(obs.FilesModified, &filesModified)

	resp := map[string]any{
		"id":               obs.ID,
		"memory_session_id": obs.MemorySessionID,
		"project":          obs.Project,
		"type":             obs.Type,
		"title":            obs.Title,
		"subtitle":         obs.Subtitle,
		"narrative":        obs.Narrative,
		"text":             obs.Text,
		"facts":            facts,
		"concepts":         concepts,
		"files_read":       filesRead,
		"files_modified":   filesModified,
		"discovery_tokens": obs.DiscoveryTokens,
		"created_at":       obs.CreatedAt.Format(time.RFC3339),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}
