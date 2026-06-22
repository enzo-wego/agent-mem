package handlers

import (
	"encoding/json"
	"net/http"
)

// NewSlackUsersHandler returns GET /api/graph/slack-users → { "<uid>": "<name>" }.
// The dashboard fetches this once to render <@U…> mentions as readable names.
func NewSlackUsersHandler(deps Deps) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rows, err := deps.DB.Query(r.Context(),
			`SELECT slack_user_id, display_name FROM graph.slack_users WHERE display_name <> ''`)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer rows.Close()
		m := make(map[string]string)
		for rows.Next() {
			var id, name string
			if rows.Scan(&id, &name) == nil {
				m[id] = name
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(m)
	})
}
