package handlers

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	graphtrace "github.com/agent-mem/agent-mem/internal/graph/trace"
)

const (
	maskEmail  = "canary.mask@example.com"
	maskPhone  = "+6581234567"
	maskCard   = "4111111111111111"
	maskedBody = `{"results":[{"node_id":"slack:C012ABC:1700000000.123456","title":"t %s","summary":"s %s","note":"p12345678z F.4111111111111111"}]}`
)

func stubJSON(body string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if diag, _ := r.Context().Value(requestTraceDiagKey{}).(*requestTraceDiag); diag != nil {
			diag.aclMode = "filtered"
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	})
}

func TestMaskGraphResponseMasksEveryString(t *testing.T) {
	body := `{"a":[{"b":{"c":"` + maskEmail + `"}},"` + maskPhone + `",7,true,null],"d":"` + maskCard + `","ts_ms":1700000000123,"id":"slack:C012ABC:1700000000.123456"}`
	w := httptest.NewRecorder()
	maskGraphResponse(stubJSON(body)).ServeHTTP(w, httptest.NewRequest("GET", "/x", nil))
	got := w.Body.String()
	for _, raw := range []string{maskEmail, maskPhone, maskCard} {
		if strings.Contains(got, raw) {
			t.Fatalf("leaked %q: %s", raw, got)
		}
	}
	var doc map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc["d"] != "[card]" || doc["id"] != "slack:C012ABC:1700000000.123456" || doc["ts_ms"].(float64) != 1700000000123 {
		t.Fatalf("unexpected doc: %s", got)
	}
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("status=%d ct=%q", w.Code, w.Header().Get("Content-Type"))
	}
}

func TestMaskGraphResponseAdminUnchanged(t *testing.T) {
	body := `{"a":"` + maskEmail + `"}`
	r := httptest.NewRequest("GET", "/x", nil)
	r.Header.Set(adminHeader, "1")
	w := httptest.NewRecorder()
	maskGraphResponse(stubJSON(body)).ServeHTTP(w, r)
	if w.Body.String() != body {
		t.Fatalf("admin body changed: %s", w.Body.String())
	}
}

func TestMaskGraphResponseKeepsStatusAndMasksErrorBody(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"no node for ` + maskEmail + `"}`))
	})
	w := httptest.NewRecorder()
	maskGraphResponse(h).ServeHTTP(w, httptest.NewRequest("GET", "/x", nil))
	if w.Code != 404 || strings.Contains(w.Body.String(), maskEmail) || !strings.Contains(w.Body.String(), "[email]") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestMaskGraphResponsePlainTextErrorMaskedStatusKept(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusNotFound} {
		h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "no node for "+maskEmail+" "+maskPhone+" "+maskCard, status)
		})
		w := httptest.NewRecorder()
		maskGraphResponse(h).ServeHTTP(w, httptest.NewRequest("GET", "/x", nil))
		if w.Code != status || strings.TrimSpace(w.Body.String()) != "no node for [email] [phone] [card]" {
			t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
		}
	}
}

func TestMaskGraphResponseFailsClosed(t *testing.T) {
	canary := maskEmail
	cases := []struct {
		name string
		h    http.HandlerFunc
	}{
		{"no content type", func(w http.ResponseWriter, r *http.Request) {
			w.Header()["Content-Type"] = nil
			_, _ = w.Write([]byte(`{"a":"` + canary + `"}`))
		}},
		{"text plain 200", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte(canary))
		}},
		{"invalid json", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"a":"` + canary + `"`))
		}},
		{"trailing garbage", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"a":"` + canary + `"} trailing`))
		}},
		{"two documents", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"a":"` + canary + `"}{"b":1}`))
		}},
		{"empty body", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
		}},
		{"panic after partial write", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"a":"` + canary))
			panic("boom")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &captureTraceStore{}
			recorder := graphtrace.NewRecorder(store, graphtrace.Options{})
			wrapped := withContextTrace(maskGraphResponse(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if diag, _ := r.Context().Value(requestTraceDiagKey{}).(*requestTraceDiag); diag != nil {
					diag.aclMode = "filtered"
				}
				tc.h(w, r)
			})), "search", "rev", recorder)
			w := httptest.NewRecorder()
			wrapped.ServeHTTP(w, httptest.NewRequest("GET", "/api/graph/search?q=x", nil))
			recorder.Close(time.Second)
			if w.Code != 500 || strings.TrimSpace(w.Body.String()) != `{"error":"mask_failed"}` {
				t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
			}
			rec := store.one(t)
			raw, _ := json.Marshal(rec)
			if strings.Contains(string(raw), canary) {
				t.Fatalf("trace holds raw canary: %s", raw)
			}
			if rec.ACLMode != ACLModeFilteredMasked {
				t.Errorf("acl_mode=%q", rec.ACLMode)
			}
		})
	}
}

// The trace wrapper sits outside the masker: recorded hashes are of what the
// client received (masked), not of the handler's raw output.
func TestMaskedTraceRecordsMaskedHashesAndMode(t *testing.T) {
	raw := strings.NewReplacer("%s", maskEmail).Replace(maskedBody)
	for _, withRecorder := range []bool{true, false} {
		store := &captureTraceStore{}
		var recorder *graphtrace.Recorder
		if withRecorder {
			recorder = graphtrace.NewRecorder(store, graphtrace.Options{})
		}
		h := withContextTrace(maskGraphResponse(stubJSON(raw)), "search", "rev", recorder)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/api/graph/search?q=x", nil))
		if strings.Contains(w.Body.String(), maskEmail) {
			t.Fatalf("recorder=%v leaked: %s", withRecorder, w.Body.String())
		}
		if !withRecorder {
			continue
		}
		recorder.Close(time.Second)
		rec := store.one(t)
		if rec.ACLMode != ACLModeFilteredMasked {
			t.Fatalf("acl_mode=%q", rec.ACLMode)
		}
		var resp struct {
			Results []struct{ Title, Summary string } `json:"results"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || len(resp.Results) != 1 {
			t.Fatalf("decode: %v body=%s", err, w.Body.String())
		}
		items, _ := rec.Items.([]map[string]any)
		if len(items) != 1 {
			t.Fatalf("items=%v", rec.Items)
		}
		if items[0]["title_sha256"] != sha256Hex(resp.Results[0].Title) || items[0]["summary_sha256"] != sha256Hex(resp.Results[0].Summary) {
			t.Fatalf("item hashes are not of the returned text: %v", items[0])
		}
		if resp.Results[0].Title != "t [email]" {
			t.Fatalf("title=%q", resp.Results[0].Title)
		}
	}
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
