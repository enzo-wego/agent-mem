package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/agent-mem/agent-mem/internal/graph/scoring"
)

func TestBoostAlphas_RoundTripAndValidation(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping integration test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	cleanup := func() {
		_, _ = pool.Exec(ctx, `DELETE FROM settings WHERE key = ANY($1)`, scoring.BoostAlphaKeys[:])
	}
	cleanup()
	// Cleanup runs LIFO: close the pool after the settings are deleted.
	t.Cleanup(pool.Close)
	t.Cleanup(cleanup)

	h := NewChannels(pool)
	get := func() boostAlphasConfig {
		t.Helper()
		w := httptest.NewRecorder()
		h.getBoostAlphas(w, httptest.NewRequest("GET", "/api/graph/boost-alphas", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("GET status %d: %s", w.Code, w.Body.String())
		}
		var cfg boostAlphasConfig
		if err := json.NewDecoder(w.Body).Decode(&cfg); err != nil {
			t.Fatal(err)
		}
		return cfg
	}

	if cfg := get(); cfg.Alphas != scoring.DefaultBoostAlphas || cfg.LegacyWeights["sem"] == 0 {
		t.Fatalf("unset → %+v, want defaults with legacy weights", cfg)
	}

	put := func(body string) int {
		t.Helper()
		w := httptest.NewRecorder()
		h.putBoostAlphas(w, httptest.NewRequest("PUT", "/api/graph/boost-alphas", strings.NewReader(body)))
		return w.Code
	}
	if code := put(`{"alphas":{"rec":0.3,"team":0,"temporal":0.5,"auth":0.05}}`); code != http.StatusOK {
		t.Fatalf("PUT status %d", code)
	}
	want := scoring.BoostAlphas{Rec: 0.3, Team: 0, Temporal: 0.5, Auth: 0.05}
	if cfg := get(); cfg.Alphas != want {
		t.Errorf("after PUT = %+v, want %+v", cfg.Alphas, want)
	}
	if a, _ := scoring.LoadBoostAlphas(ctx, pool); a != want {
		t.Errorf("LoadBoostAlphas = %+v, want %+v", a, want)
	}
	if code := put(`{"alphas":{"rec":1.5}}`); code != http.StatusBadRequest {
		t.Errorf("alpha > 1 status %d, want 400", code)
	}
	if code := put(`{"alphas":{"rec":-0.1}}`); code != http.StatusBadRequest {
		t.Errorf("negative alpha status %d, want 400", code)
	}
	if code := put(`nope`); code != http.StatusBadRequest {
		t.Errorf("bad json status %d, want 400", code)
	}
	if cfg := get(); cfg.Alphas != want {
		t.Errorf("rejected PUTs must not change stored alphas: %+v", cfg.Alphas)
	}
}
