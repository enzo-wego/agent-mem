package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func tzRestore(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	var prev string
	had := pool.QueryRow(ctx, `SELECT value FROM settings WHERE key=$1`, temporalTimezoneKey).Scan(&prev) == nil
	t.Cleanup(func() {
		if had {
			_, _ = pool.Exec(ctx, `INSERT INTO settings(key,value) VALUES($1,$2) ON CONFLICT(key) DO UPDATE SET value=EXCLUDED.value`, temporalTimezoneKey, prev)
		} else {
			_, _ = pool.Exec(ctx, `DELETE FROM settings WHERE key=$1`, temporalTimezoneKey)
		}
	})
}

func TestParseWhen_Timezone(t *testing.T) {
	ict, err := time.LoadLocation("Asia/Ho_Chi_Minh")
	if err != nil {
		t.Fatal(err)
	}
	t.Run("date_only", func(t *testing.T) {
		got, dateOnly, err := parseWhen("2026-09-01", ict)
		if err != nil || !dateOnly {
			t.Fatalf("err=%v dateOnly=%v", err, dateOnly)
		}
		if want := time.Date(2026, 9, 1, 0, 0, 0, 0, ict); !got.Equal(want) || got.Format(time.RFC3339) != "2026-09-01T00:00:00+07:00" {
			t.Fatalf("got %s", got.Format(time.RFC3339))
		}
	})
	t.Run("rfc3339_kept", func(t *testing.T) {
		got, dateOnly, err := parseWhen("2026-09-01T00:00:00Z", ict)
		if err != nil || dateOnly {
			t.Fatalf("err=%v dateOnly=%v", err, dateOnly)
		}
		if !got.Equal(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)) {
			t.Fatalf("got %s", got)
		}
	})
	t.Run("rfc3339_offset_kept", func(t *testing.T) {
		in := "2026-09-01T00:00:00+05:00"
		got, _, err := parseWhen(in, ict)
		if err != nil {
			t.Fatal(err)
		}
		if !got.Equal(time.Date(2026, 8, 31, 19, 0, 0, 0, time.UTC)) || got.Format(time.RFC3339) != in {
			t.Fatalf("got %s", got.Format(time.RFC3339))
		}
	})
}

func TestTemporalTimezoneSetting(t *testing.T) {
	pool := openTestDB(t)
	tzRestore(t, pool)
	ctx := context.Background()
	h := NewChannels(pool)
	get := func() temporalTimezoneConfig {
		t.Helper()
		w := httptest.NewRecorder()
		h.getTemporalTimezone(w, httptest.NewRequest("GET", "/api/graph/temporal-timezone", nil))
		var c temporalTimezoneConfig
		if err := json.NewDecoder(w.Body).Decode(&c); err != nil {
			t.Fatal(err)
		}
		return c
	}
	put := func(name string) int {
		w := httptest.NewRecorder()
		h.putTemporalTimezone(w, httptest.NewRequest("PUT", "/api/graph/temporal-timezone", strings.NewReader(`{"timezone":"`+name+`"}`)))
		if (name == "Mars/Base" || name == "Local") && !strings.Contains(w.Body.String(), "unknown timezone "+name) {
			t.Errorf("body = %s", w.Body.String())
		}
		return w.Code
	}

	t.Run("put_valid", func(t *testing.T) {
		if code := put("Asia/Tokyo"); code != http.StatusOK {
			t.Fatalf("code %d", code)
		}
		if c := get(); c.Timezone != "Asia/Tokyo" || c.Effective != "Asia/Tokyo" {
			t.Fatalf("%+v", c)
		}
	})
	t.Run("put_invalid", func(t *testing.T) {
		for _, name := range []string{"Mars/Base", "Local"} {
			if code := put(name); code != http.StatusBadRequest {
				t.Fatalf("%s: code %d", name, code)
			}
		}
	})
	t.Run("absent_defaults", func(t *testing.T) {
		if _, err := pool.Exec(ctx, `DELETE FROM settings WHERE key=$1`, temporalTimezoneKey); err != nil {
			t.Fatal(err)
		}
		if c := get(); c.Timezone != "Asia/Ho_Chi_Minh" || c.Effective != "Asia/Ho_Chi_Minh" {
			t.Fatalf("%+v", c)
		}
	})
	t.Run("empty_defaults", func(t *testing.T) {
		saveSetting(ctx, pool, temporalTimezoneKey, "")
		if c := get(); c.Effective != "Asia/Ho_Chi_Minh" {
			t.Fatalf("%+v", c)
		}
	})
	t.Run("stored_invalid_utc", func(t *testing.T) {
		saveSetting(ctx, pool, temporalTimezoneKey, "Mars/Base")
		if c := get(); c.Effective != "UTC" {
			t.Fatalf("%+v", c)
		}
	})
}

func TestSearch_TimezoneWiring(t *testing.T) {
	pool := winReset(t)
	tzRestore(t, pool)
	ctx := context.Background()
	winNode(t, pool, "slack:C1:1.0", "slack", "2026-10-03T23:30:00+07:00", "2026-10-03T23:30:00+07:00")
	winNode(t, pool, "slack:C1:2.0", "slack", "2026-10-04T00:30:00+07:00", "2026-10-04T00:30:00+07:00")
	if _, err := pool.Exec(ctx, `UPDATE graph.nodes SET scope='public'`); err != nil {
		t.Fatal(err)
	}
	s, err := NewSearch(pool)
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Date(2026, 10, 4, 6, 0, 0, 0, time.FixedZone("ICT", 7*3600)) }

	ids := func() map[string]bool {
		t.Helper()
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest("GET", "/api/graph/search?q=yesterday&arms=temporal&limit=20", nil))
		if w.Code != 200 {
			t.Fatalf("code %d: %s", w.Code, w.Body.String())
		}
		var resp struct {
			Results []struct {
				NodeID string `json:"node_id"`
			} `json:"results"`
		}
		if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for _, r := range resp.Results {
			out[r.NodeID] = true
		}
		return out
	}

	saveSetting(ctx, pool, temporalTimezoneKey, "Asia/Ho_Chi_Minh")
	if got := ids(); !got["slack:C1:1.0"] || got["slack:C1:2.0"] || len(got) != 1 {
		t.Fatalf("ICT yesterday: %v", got)
	}
	saveSetting(ctx, pool, temporalTimezoneKey, "UTC")
	if got := ids(); len(got) != 0 {
		t.Fatalf("UTC yesterday: %v", got)
	}
}

func TestSearchWindow_APIOpenFlag(t *testing.T) {
	s := &Search{}
	now := time.Date(2026, 10, 4, 6, 0, 0, 0, time.UTC)
	cases := []struct {
		name, qs string
		open     bool
	}{
		{"since_only", "since=2026-09-01", true},
		{"until_only", "until=2026-09-01", true},
		{"both", "since=2026-08-01&until=2026-09-01", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			qv := map[string][]string{}
			for _, kv := range strings.Split(tc.qs, "&") {
				p := strings.SplitN(kv, "=", 2)
				qv[p[0]] = []string{p[1]}
			}
			w, _, has, err := s.window(qv, "q", now, time.UTC)
			if err != nil || !has {
				t.Fatalf("err=%v has=%v", err, has)
			}
			if w.Open != tc.open {
				t.Errorf("Open = %v, want %v", w.Open, tc.open)
			}
		})
	}
}
