package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/agent-mem/agent-mem/internal/llmgateway"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
)

const subjectQueriesPrompt = `From the Jira ticket below, write 2 or 3 short search queries (3 to 7 words each) that would find the Slack threads where this work's requirement or acceptance was discussed with stakeholders. Use the subject's own words: the product area, the document or artefact, the team asking. Never include the ticket key, URLs, people's names, dates or status words. Reply with a JSON array of strings only.`

type subjectQueriesResponse struct {
	Queries []string `json:"queries"`
	Cached  bool     `json:"cached"`
	Error   string   `json:"error,omitempty"`
}

// NewSubjectQueries builds cached search phrases from a Jira ticket.
// Thread visibility is enforced by the subsequent hybrid search, not this endpoint.
func NewSubjectQueries(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		if !strings.HasPrefix(id, "jira:") {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "subject queries are only built for jira nodes"})
			return
		}
		var title, body string
		err := deps.DB.QueryRow(r.Context(), `SELECT COALESCE(title,''), COALESCE(body,'') FROM graph.nodes WHERE id=$1 AND deleted_at IS NULL`, id).Scan(&title, &body)
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "node not found"})
			return
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not read ticket"})
			return
		}
		body = truncateRunes(body, 4000)
		sum := sha256.Sum256([]byte(title + "\n" + body))
		sig := "v1:" + hex.EncodeToString(sum[:])[:16]
		resp := subjectQueriesResponse{Queries: []string{}}
		var cached []byte
		err = deps.DB.QueryRow(r.Context(), `SELECT queries FROM graph.subject_queries WHERE node_id=$1 AND signature=$2`, id, sig).Scan(&cached)
		if err == nil {
			if json.Unmarshal(cached, &resp.Queries) == nil && resp.Queries != nil {
				resp.Cached = true
				writeJSON(w, http.StatusOK, resp)
				return
			}
			resp.Queries = []string{}
		} else if !errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not read subject queries"})
			return
		}
		if deps.Gemini == nil {
			resp.Error = "llm not configured"
			writeJSON(w, http.StatusOK, resp)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		reply, err := deps.Gemini.GenerateCheap(ctx, subjectQueriesPrompt, "Title: "+title+"\n\n"+body)
		if err != nil {
			switch {
			case errors.Is(err, llmgateway.ErrCapped):
				resp.Error = "llm hourly cap reached"
			case errors.Is(err, context.DeadlineExceeded):
				resp.Error = "llm timed out"
			default:
				resp.Error = "llm generation failed"
			}
			writeJSON(w, http.StatusOK, resp)
			return
		}
		resp.Queries = parseSubjectQueries(reply, strings.TrimPrefix(id, "jira:"))
		if len(resp.Queries) > 0 {
			queries, err := json.Marshal(resp.Queries)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not encode subject queries"})
				return
			}
			_, err = deps.DB.Exec(r.Context(), `INSERT INTO graph.subject_queries (node_id, signature, queries) VALUES ($1,$2,$3)
ON CONFLICT (node_id) DO UPDATE SET signature=EXCLUDED.signature, queries=EXCLUDED.queries, updated_at=now()`, id, sig, queries)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not cache subject queries"})
				return
			}
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

func parseSubjectQueries(reply, ticketKey string) []string {
	reply = strings.TrimSpace(reply)
	if strings.HasPrefix(reply, "```") {
		_, reply, _ = strings.Cut(reply, "\n")
		reply = strings.TrimSpace(reply)
		if i := strings.LastIndex(reply, "\n"); i >= 0 && strings.TrimSpace(reply[i+1:]) == "```" {
			reply = strings.TrimSpace(reply[:i])
		}
	}
	out := []string{}
	var queries []string
	if json.Unmarshal([]byte(reply), &queries) != nil {
		return out
	}
	seen := make(map[string]bool, 3)
	key := strings.ToLower(ticketKey)
	for _, query := range queries {
		query = strings.TrimSpace(query)
		words := len(strings.Fields(query))
		lower := strings.ToLower(query)
		if words < 2 || words > 10 || strings.Contains(lower, key) || seen[lower] {
			continue
		}
		seen[lower] = true
		out = append(out, query)
		if len(out) == 3 {
			break
		}
	}
	return out
}
