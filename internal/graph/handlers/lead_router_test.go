package handlers

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"
)

// NewResolve returns nil when the scoring weights fail to load. Mount must not
// panic on that (main mounted the nil handler, so only the resolve route fails,
// at request time), with lead mode on or off.
func TestMountSurvivesNewResolveFailure(t *testing.T) {
	for _, lead := range []bool{false, true} {
		pool, err := pgxpool.New(context.Background(), "postgres://u:p@127.0.0.1:1/none")
		if err != nil {
			t.Fatal(err)
		}
		pool.Close() // every query now fails, so LoadWeights fails
		if r, err := NewResolve(pool); r != nil || err == nil {
			t.Fatalf("NewResolve on a closed pool = (%v, %v), want (nil, error)", r, err)
		}
		r := chi.NewRouter()
		r.Use(middleware.Recoverer)
		func() {
			defer func() {
				if p := recover(); p != nil {
					t.Fatalf("lead=%v: Mount panicked: %v", lead, p)
				}
			}()
			Mount(r, Deps{DB: pool, LeadModeEnabled: lead})
		}()
		req := httptest.NewRequest(http.MethodPost, "/api/graph/resolve", bytes.NewBufferString(`{}`))
		req.Header.Set("X-Graph-Lead", "1")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code == http.StatusOK {
			t.Fatalf("lead=%v: resolve answered 200 with no handler", lead)
		}
	}
}
