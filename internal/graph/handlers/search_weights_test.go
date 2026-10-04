package handlers_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/agent-mem/agent-mem/internal/graph/handlers"
	"github.com/agent-mem/agent-mem/internal/graph/scoring"
)

func clearWeightSettings(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	clear := func() {
		if _, err := pool.Exec(context.Background(), `DELETE FROM public.settings WHERE key LIKE 'graph.weights.%'`); err != nil {
			t.Fatalf("clear weight settings: %v", err)
		}
	}
	clear()
	t.Cleanup(clear)
}

func TestLoadHybridWeights_Override(t *testing.T) {
	pool := testDB(t)
	clearWeightSettings(t, pool)
	check := func(want scoring.HybridWeights) {
		t.Helper()
		got, err := scoring.LoadHybridWeights(context.Background(), pool)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("weights = %+v, want %+v", got, want)
		}
	}
	check(scoring.DefaultHybridWeights())
	if _, err := pool.Exec(context.Background(), `INSERT INTO public.settings(key,value) VALUES
		('graph.weights.hybrid_rec','0.05'), ('graph.weights.kw','0.3'), ('graph.weights.title','0.1')
		ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value`); err != nil {
		t.Fatal(err)
	}
	check(scoring.HybridWeights{Rec: 0.05, KW: 0.3, Title: 0.1})
	if _, err := pool.Exec(context.Background(), `UPDATE public.settings SET value='oops' WHERE key='graph.weights.kw'`); err != nil {
		t.Fatal(err)
	}
	check(scoring.HybridWeights{Rec: 0.05, KW: 0.15, Title: 0.1})
}

func TestSearchWeightsHandler_GetPut(t *testing.T) {
	pool := testDB(t)
	clearWeightSettings(t, pool)
	h := handlers.NewSearchWeightsHandler(handlers.Deps{DB: pool})
	request := func(method, body string, status int, want *scoring.HybridWeights) {
		t.Helper()
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(method, "/api/graph/search-weights", strings.NewReader(body)))
		if w.Code != status {
			t.Fatalf("%s %s: status %d, want %d: %s", method, body, w.Code, status, w.Body.String())
		}
		if want != nil {
			var got scoring.HybridWeights
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got != *want {
				t.Fatalf("%s: weights = %+v, want %+v", method, got, *want)
			}
		}
	}
	defaults := scoring.DefaultHybridWeights()
	request(http.MethodGet, "", http.StatusOK, &defaults)
	want := scoring.HybridWeights{Rec: 0.05, KW: 0.3, Title: 0.1}
	request(http.MethodPut, `{"hybrid_rec":0.05,"kw":0.3,"title":0.1}`, http.StatusOK, &want)
	request(http.MethodGet, "", http.StatusOK, &want)
	for _, setting := range []struct{ key, value string }{
		{scoring.KeyHybridRec, "0.05"}, {scoring.KeyHybridKW, "0.3"}, {scoring.KeyHybridTitle, "0.1"},
	} {
		var got string
		if err := pool.QueryRow(context.Background(), `SELECT value FROM public.settings WHERE key=$1`, setting.key).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != setting.value {
			t.Fatalf("setting %s = %q, want %q", setting.key, got, setting.value)
		}
	}
	for _, body := range []string{
		`{"kw":0.3,"title":0.1}`,
		`{"hybrid_rec":0.05,"kw":1.5,"title":0.1}`,
		`{"hybrid_rec":0.05,"kw":0.3,"title":-0.1}`,
		`{"kw":`,
	} {
		request(http.MethodPut, body, http.StatusBadRequest, nil)
	}
	request(http.MethodGet, "", http.StatusOK, &want)
	request(http.MethodDelete, "", http.StatusMethodNotAllowed, nil)
}
