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

func TestBoostAlphas_GraphArmWeight(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping integration test")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	keys := append(append([]string(nil), scoring.BoostAlphaKeys[:]...), scoring.GraphArmWeightKey)
	cleanup := func() {
		if _, err := pool.Exec(ctx, `DELETE FROM settings WHERE key = ANY($1)`, keys); err != nil {
			t.Errorf("cleanup settings: %v", err)
		}
	}
	t.Cleanup(pool.Close)
	t.Cleanup(cleanup)
	cleanup()

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
	put := func(body string, want int) {
		t.Helper()
		w := httptest.NewRecorder()
		h.putBoostAlphas(w, httptest.NewRequest("PUT", "/api/graph/boost-alphas", strings.NewReader(body)))
		if w.Code != want {
			t.Fatalf("PUT %s status %d, want %d: %s", body, w.Code, want, w.Body.String())
		}
	}
	snapshot := func() map[string]string {
		t.Helper()
		rows, err := pool.Query(ctx, `SELECT key,value FROM settings WHERE key = ANY($1)`, keys)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		values := make(map[string]string)
		for rows.Next() {
			var key, value string
			if err := rows.Scan(&key, &value); err != nil {
				t.Fatal(err)
			}
			values[key] = value
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return values
	}
	assertStored := func(want map[string]string) {
		t.Helper()
		got := snapshot()
		if len(got) != len(want) {
			t.Fatalf("stored settings = %v, want %v", got, want)
		}
		for key, value := range want {
			if got[key] != value {
				t.Fatalf("stored settings = %v, want %v", got, want)
			}
		}
	}
	const oldPayload = `{"alphas":{"rec":0.2,"team":0.3,"temporal":0.4,"auth":0.5}}`
	wantAlphas := scoring.BoostAlphas{Rec: 0.2, Team: 0.3, Temporal: 0.4, Auth: 0.5}

	t.Run("default and omitted unset", func(t *testing.T) {
		if cfg := get(); cfg.ArmGraph != 1 {
			t.Fatalf("default arm_graph = %g, want 1", cfg.ArmGraph)
		}
		put(oldPayload, http.StatusOK)
		if _, exists := snapshot()[scoring.GraphArmWeightKey]; exists {
			t.Fatal("omitted arm_graph created a setting")
		}
		before := snapshot()
		put(`{"alphas":{"rec":0.2,"team":0.3,"temporal":0.4,"auth":0.5},"arm_graph":null}`, http.StatusOK)
		assertStored(before)
		if cfg := get(); cfg.ArmGraph != 1 || cfg.Alphas != wantAlphas {
			t.Fatalf("unset weight response = %+v", cfg)
		}
	})
	t.Run("omitted and null preserve stored", func(t *testing.T) {
		if _, err := pool.Exec(ctx, `INSERT INTO settings(key,value) VALUES($1,'0.4')`, scoring.GraphArmWeightKey); err != nil {
			t.Fatal(err)
		}
		before := snapshot()
		put(oldPayload, http.StatusOK)
		assertStored(before)
		put(`{"alphas":{"rec":0.2,"team":0.3,"temporal":0.4,"auth":0.5},"arm_graph":null}`, http.StatusOK)
		assertStored(before)
		if cfg := get(); cfg.ArmGraph != 0.4 {
			t.Fatalf("preserved arm_graph = %g, want .4", cfg.ArmGraph)
		}
	})
	t.Run("invalid values preserve all five settings", func(t *testing.T) {
		before := snapshot()
		for _, value := range []string{"1.5", "-0.1", `"not a number"`} {
			put(`{"alphas":{"rec":0.9,"team":0.8,"temporal":0.7,"auth":0.6},"arm_graph":`+value+`}`, http.StatusBadRequest)
			assertStored(before)
		}
	})
	t.Run("full payload changes only graph weight", func(t *testing.T) {
		before := snapshot()
		put(`{"alphas":{"rec":0.2,"team":0.3,"temporal":0.4,"auth":0.5},"arm_graph":0.7}`, http.StatusOK)
		before[scoring.GraphArmWeightKey] = "0.7"
		assertStored(before)
		if cfg := get(); cfg.ArmGraph != 0.7 || cfg.Alphas != wantAlphas {
			t.Fatalf("full payload response = %+v", cfg)
		}
	})
	t.Run("explicit zero", func(t *testing.T) {
		put(`{"alphas":{"rec":0.2,"team":0.3,"temporal":0.4,"auth":0.5},"arm_graph":0}`, http.StatusOK)
		if cfg := get(); cfg.ArmGraph != 0 || cfg.Alphas != wantAlphas {
			t.Fatalf("zero weight response = %+v", cfg)
		}
		if value := snapshot()[scoring.GraphArmWeightKey]; value != "0" {
			t.Fatalf("stored zero weight = %q", value)
		}
	})
}
