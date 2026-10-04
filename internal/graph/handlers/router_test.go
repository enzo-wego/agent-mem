package handlers_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/agent-mem/agent-mem/internal/graph/handlers"
)

func TestSearchWeightsRouteRemoved(t *testing.T) {
	pool := testDB(t)
	r := chi.NewRouter()
	handlers.Mount(r, handlers.Deps{DB: pool})
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(method, "/api/graph/search-weights", strings.NewReader(`{}`)))
		if w.Code != http.StatusNotFound {
			t.Errorf("%s /api/graph/search-weights = %d, want 404", method, w.Code)
		}
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/graph/boost-alphas", nil))
	if w.Code == http.StatusNotFound {
		t.Errorf("control route /api/graph/boost-alphas is also 404: Mount did not register routes")
	}
}
