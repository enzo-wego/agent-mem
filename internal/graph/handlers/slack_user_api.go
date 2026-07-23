package handlers

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
)

// slackUserProfile is the resolved identity of a single Slack user, joining the
// name-only graph.slack_users cache with the richer graph.people row (email,
// department, eeid) when one is linked. Nullable fields are omitted when empty.
type slackUserProfile struct {
	SlackUserID string `json:"slack_user_id"`
	DisplayName string `json:"display_name,omitempty"`
	RealName    string `json:"real_name,omitempty"`
	IsBot       bool   `json:"is_bot"`
	Email       string `json:"email,omitempty"`
	Department  string `json:"department,omitempty"`
	EEID        int    `json:"eeid,omitempty"`
}

// NewSlackUserHandler returns GET /api/graph/slack-user?id=<uid> → a single
// user's profile. Unlike /api/graph/slack-users (which dumps the whole
// name-only table for the dashboard), this resolves ONE uid to name + org
// context by joining slack_users with people. Used by EnzoBot to stamp each
// injected Slack turn with who sent it. Read-only; returns 404 "not found" if
// the uid is in neither table.
func NewSlackUserHandler(deps Deps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("id")
		if id == "" {
			http.Error(w, "missing id", http.StatusBadRequest)
			return
		}

		// FULL OUTER JOIN so a uid present in only one table still resolves:
		// slack_users carries the name for the whole roster; people carries
		// email/department/eeid but only for users seen through ingestion.
		var p slackUserProfile
		var real, email, dept *string
		var eeid *int
		err := deps.DB.QueryRow(r.Context(), `
			SELECT COALESCE(su.slack_user_id, p.slack_user_id)            AS slack_user_id,
			       COALESCE(NULLIF(su.display_name, ''), p.display_name)  AS display_name,
			       su.real_name,
			       COALESCE(su.is_bot, p.is_bot, FALSE)                   AS is_bot,
			       p.email, p.department, p.eeid
			FROM graph.slack_users su
			FULL OUTER JOIN graph.people p
			  ON p.slack_user_id = su.slack_user_id AND p.merged_into IS NULL
			WHERE su.slack_user_id = $1 OR p.slack_user_id = $1
			LIMIT 1`,
			id,
		).Scan(&p.SlackUserID, &p.DisplayName, &real, &p.IsBot, &email, &dept, &eeid)
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if real != nil {
			p.RealName = *real
		}
		if email != nil {
			p.Email = *email
		}
		if dept != nil {
			p.Department = *dept
		}
		if eeid != nil {
			p.EEID = *eeid
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(p)
	})
}
