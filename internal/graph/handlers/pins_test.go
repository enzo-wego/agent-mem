package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Validation must reject bad input before touching the DB (h.db is nil here —
// a panic means validation ran too late).
func TestPinsCreateValidation(t *testing.T) {
	h := NewPins(nil)
	cases := []struct {
		name string
		body string
	}{
		{"bad json", `{not json`},
		{"missing thread_ts", `{"channel_id":"C1"}`},
		{"missing channel_id", `{"thread_ts":"100.000001"}`},
		{"whitespace only", `{"channel_id":"  ","thread_ts":"100.000001"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/api/graph/pins", strings.NewReader(tc.body))
			h.create(w, r)
			if w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", w.Code)
			}
		})
	}
}

func TestPinsDeleteValidation(t *testing.T) {
	h := NewPins(nil)
	for _, qs := range []string{"", "channel=C1", "thread=100.000001"} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodDelete, "/api/graph/pins?"+qs, nil)
		h.delete(w, r)
		if w.Code != http.StatusBadRequest {
			t.Errorf("qs=%q status = %d, want 400", qs, w.Code)
		}
	}
}
