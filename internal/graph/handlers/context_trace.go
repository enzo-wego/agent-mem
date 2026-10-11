package handlers

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	graphtrace "github.com/agent-mem/agent-mem/internal/graph/trace"
)

type traceHTTP struct {
	next            http.Handler
	route, buildRev string
	recorder        *graphtrace.Recorder
}
type bufferedResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (w *bufferedResponse) Header() http.Header { return w.header }
func (w *bufferedResponse) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}
func (w *bufferedResponse) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(p)
}

func withContextTrace(next http.Handler, route, buildRev string, recorder *graphtrace.Recorder) http.Handler {
	if recorder == nil {
		return next
	}
	return &traceHTTP{next: next, route: route, buildRev: buildRev, recorder: recorder}
}
func (h *traceHTTP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	id := newContextID()
	w.Header().Set("X-Graph-Request-Id", id)
	requestBytes, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(requestBytes))
	diag := &requestTraceDiag{}
	r = r.WithContext(context.WithValue(contextWithTraceBody(r, requestBytes), requestTraceDiagKey{}, diag))
	bw := &bufferedResponse{header: make(http.Header)}
	h.next.ServeHTTP(bw, r)
	status := bw.status
	if status == 0 {
		status = http.StatusOK
	}
	body := bw.body.Bytes()
	if status >= 200 && status < 300 && strings.HasPrefix(bw.header.Get("Content-Type"), "application/json") {
		var top map[string]any
		if json.Unmarshal(body, &top) == nil {
			top["request_id"] = id
			body, _ = json.Marshal(top)
			body = append(body, '\n')
		}
	}
	for key, values := range bw.header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.Header().Set("X-Graph-Request-Id", id)
	w.WriteHeader(status)
	_, _ = w.Write(body)
	hash := sha256.Sum256(body)
	record := graphtrace.Record{ID: id, CreatedAt: started.UTC(), Route: h.route, BuildRev: unknownIfEmpty(h.buildRev), Caller: capString(defaultString(r.Header.Get("X-Graph-Caller"), "unknown"), 64), Status: status, ACLMode: "not-evaluated", Request: traceRequest(r, h.route), AskerInput: traceAsker(r, h.route), Items: traceItems(body, h.route), ResponseSHA256: hex.EncodeToString(hash[:]), LatencyMS: time.Since(started).Milliseconds()}
	mergeTraceItemMeta(record.Items, diag)
	if status >= 400 {
		message := capString(strings.TrimSpace(string(body)), 500)
		record.Error = &message
	} else {
		record.ACLMode = traceACLMode(r, h.route)
	}
	if items, ok := record.Items.([]map[string]any); ok && len(items) > 200 {
		record.Items = items[:200]
		record.ItemsTruncated = true
	}
	h.recorder.Enqueue(r.Context(), record)
}
func newContextID() string {
	var b [13]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return "ctx_" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:])[:20])
}
func unknownIfEmpty(s string) string {
	if strings.TrimSpace(s) == "" {
		return "unknown"
	}
	return s
}
func defaultString(s, d string) string {
	if strings.TrimSpace(s) == "" {
		return d
	}
	return s
}
func capString(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
func traceAsker(r *http.Request, route string) map[string]any {
	out := map[string]any{"body_asker_eeid": nil, "header_asker_user": nil}
	if v := r.Header.Get("X-Asker-User"); v != "" {
		out["header_asker_user"] = v
	}
	if route == "resolve" {
		var body map[string]any
		_ = json.NewDecoder(bytes.NewReader(requestBody(r))).Decode(&body)
		if v, ok := body["asker_eeid"]; ok {
			out["body_asker_eeid"] = v
		}
	}
	return out
}
func requestBody(r *http.Request) []byte {
	if b, ok := r.Context().Value(traceBodyKey{}).([]byte); ok {
		return b
	}
	return nil
}

type traceBodyKey struct{}

func traceRequest(r *http.Request, route string) any {
	if route == "search" {
		out := map[string]any{}
		for k, v := range r.URL.Query() {
			if len(v) == 1 {
				out[k] = capString(v[0], 2000)
			} else {
				out[k] = v
			}
		}
		if top, ok := traceItemsRaw(r).(map[string]any); ok {
			if e, ok := top["arm_errors"]; ok {
				out["arm_errors"] = e
			}
		}
		return out
	}
	var out any
	if json.Unmarshal(requestBody(r), &out) != nil {
		return map[string]any{}
	}
	return out
}
func traceItemsRaw(r *http.Request) any { return r.Context().Value(traceResponseKey{}) }

type traceResponseKey struct{}

func contextWithTraceBody(r *http.Request, body []byte) context.Context {
	return context.WithValue(r.Context(), traceBodyKey{}, body)
}

type requestTraceDiag struct {
	aclMode  string
	itemMeta map[string]map[string]any
}
type requestTraceDiagKey struct{}

func traceItems(body []byte, route string) any {
	var top map[string]any
	if json.Unmarshal(body, &top) != nil {
		return []map[string]any{}
	}
	key := "results"
	if route == "resolve" {
		key = "artifacts"
	}
	raws, _ := top[key].([]any)
	out := make([]map[string]any, 0, len(raws))
	for rank, raw := range raws {
		m, _ := raw.(map[string]any)
		id := m["node_id"]
		if id == nil {
			id = m["id"]
		}
		item := map[string]any{"node_id": id, "rank": rank, "score": m["score"], "score_breakdown": m["score_breakdown"]}
		if route == "resolve" {
			item["hop"] = m["hop"]
			item["links"] = m["links"]
			_, item["body_included"] = m["body"]
			if text, ok := m["body"].(string); ok {
				sum := sha256.Sum256([]byte(text))
				item["body_bytes"] = len([]byte(text))
				item["body_sha256"] = hex.EncodeToString(sum[:])
			} else {
				item["body_bytes"], item["body_sha256"] = nil, nil
			}
		}
		for _, field := range []string{"title", "summary"} {
			if text, ok := m[field].(string); ok {
				sum := sha256.Sum256([]byte(text))
				item[field+"_sha256"] = hex.EncodeToString(sum[:])
			}
		}
		out = append(out, item)
	}
	return out
}

func mergeTraceItemMeta(items any, diag *requestTraceDiag) {
	if diag == nil || len(diag.itemMeta) == 0 {
		return
	}
	typed, _ := items.([]map[string]any)
	for _, item := range typed {
		for key, value := range diag.itemMeta[fmt.Sprint(item["node_id"])] {
			item[key] = value
		}
	}
}

func traceACLMode(r *http.Request, route string) string {
	if diag, _ := r.Context().Value(requestTraceDiagKey{}).(*requestTraceDiag); diag != nil && diag.aclMode != "" {
		return diag.aclMode
	}
	if route == "resolve" {
		var body map[string]any
		_ = json.Unmarshal(requestBody(r), &body)
		if n, ok := body["asker_eeid"].(float64); !ok || n == 0 {
			return "unfiltered"
		}
		return "filtered"
	}
	if strings.TrimSpace(r.Header.Get("X-Asker-User")) == "" {
		return "unfiltered"
	}
	return "public-only"
}
