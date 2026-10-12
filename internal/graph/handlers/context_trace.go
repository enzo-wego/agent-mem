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
	const maxTraceRequestBody = 1 << 20
	prefix, _ := io.ReadAll(io.LimitReader(r.Body, maxTraceRequestBody+1))
	requestBytes := prefix
	if len(requestBytes) > maxTraceRequestBody {
		requestBytes = requestBytes[:maxTraceRequestBody]
	}
	r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(prefix), r.Body))
	diag := &requestTraceDiag{}
	r = r.WithContext(context.WithValue(contextWithTraceBody(r, requestBytes), requestTraceDiagKey{}, diag))
	bw := &bufferedResponse{header: make(http.Header)}
	h.next.ServeHTTP(bw, r)
	status := bw.status
	if status == 0 {
		status = http.StatusOK
	}
	body := bw.body.Bytes()
	if status >= 200 && status < 300 && strings.HasPrefix(bw.header.Get("Content-Type"), "application/json") && json.Valid(body) {
		body = insertRequestID(body, id)
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
	record := graphtrace.Record{ID: id, CreatedAt: started.UTC(), Route: h.route, BuildRev: unknownIfEmpty(h.buildRev), Caller: capString(defaultString(r.Header.Get("X-Graph-Caller"), "unknown"), 64), Status: status, ACLMode: traceACLMode(r), Request: traceRequest(r, h.route), AskerInput: traceAsker(r, h.route), Items: traceItems(body, h.route), ResponseSHA256: hex.EncodeToString(hash[:]), LatencyMS: time.Since(started).Milliseconds()}
	mergeTraceItemMeta(record.Items, diag)
	if status >= 400 {
		message := capString(strings.TrimSpace(string(body)), 500)
		record.Error = &message
	}
	if items, ok := record.Items.([]map[string]any); ok && len(items) > 200 {
		record.Items = items[:200]
		record.ItemsTruncated = true
	}
	if headerNoSideEffects(r) {
		return
	}
	h.recorder.Enqueue(r.Context(), record)
}

// headerNoSideEffects reports whether X-Graph-No-Side-Effects alone asks for
// no writes: "1" or "true" (case-insensitive).
func headerNoSideEffects(r *http.Request) bool {
	value := r.Header.Get("X-Graph-No-Side-Effects")
	return value == "1" || strings.EqualFold(value, "true")
}

// noSideEffects applies the shared rule: an explicit header overrides the body
// flag; "1" and "true" (case-insensitive) enable it, any other non-empty value
// disables it.
func noSideEffects(r *http.Request, bodyFlag bool) bool {
	if r.Header.Get("X-Graph-No-Side-Effects") != "" {
		return headerNoSideEffects(r)
	}
	return bodyFlag
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
	s = strings.ReplaceAll(s, "\x00", "")
	runes := []rune(s)
	if len(runes) > n {
		return string(runes[:n])
	}
	return s
}
func traceAsker(r *http.Request, route string) map[string]any {
	out := map[string]any{"body_asker_eeid": nil, "body_asker_slack_id": nil, "header_asker_user": nil}
	if v := r.Header.Get("X-Asker-User"); v != "" {
		out["header_asker_user"] = capString(v, 2000)
	}
	if route == "resolve" {
		if diag, _ := r.Context().Value(requestTraceDiagKey{}).(*requestTraceDiag); diag != nil && diag.askerDecoded {
			out["body_asker_eeid"] = diag.askerEEID
			if diag.askerSlackID != "" {
				out["body_asker_slack_id"] = capString(diag.askerSlackID, 2000)
			}
		} else {
			var body map[string]any
			_ = json.NewDecoder(bytes.NewReader(requestBody(r))).Decode(&body)
			if v, ok := body["asker_eeid"]; ok {
				out["body_asker_eeid"] = capTraceValue(v)
			}
			if v, ok := body["asker_slack_id"]; ok {
				out["body_asker_slack_id"] = capTraceValue(v)
			}
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
		for k, values := range r.URL.Query() {
			values = capStrings(values, 50)
			if len(values) == 1 {
				out[capString(k, 2000)] = capString(values[0], 2000)
			} else {
				out[capString(k, 2000)] = values
			}
		}
		if diag, _ := r.Context().Value(requestTraceDiagKey{}).(*requestTraceDiag); diag != nil && len(diag.armErrors) > 0 {
			armErrors := make(map[string]string, len(diag.armErrors))
			for arm, reason := range diag.armErrors {
				armErrors[capString(arm, 2000)] = capString(reason, 2000)
			}
			out["arm_errors"] = armErrors
		}
		return out
	}
	var decoded map[string]any
	if json.Unmarshal(requestBody(r), &decoded) != nil {
		return map[string]any{}
	}
	out := capTraceValue(decoded).(map[string]any)
	if diag, _ := r.Context().Value(requestTraceDiagKey{}).(*requestTraceDiag); diag != nil && diag.canonicalSeeds != nil {
		out["canonical_seeds"] = capStrings(diag.canonicalSeeds, 50)
	}
	return out
}

func capStrings(values []string, max int) []string {
	if len(values) > max {
		values = values[:max]
	}
	out := make([]string, len(values))
	for i, value := range values {
		out[i] = capString(value, 2000)
	}
	return out
}

// capTraceValue returns value with every string capped at 2,000 runes, every
// array at 50 entries and every NUL byte removed (jsonb rejects U+0000), in
// object keys as well as values.
func capTraceValue(value any) any {
	switch typed := value.(type) {
	case string:
		return capString(typed, 2000)
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, child := range typed {
			out[capString(key, 2000)] = capTraceValue(child)
		}
		return out
	case []any:
		if len(typed) > 50 {
			typed = typed[:50]
		}
		out := make([]any, len(typed))
		for i, child := range typed {
			out[i] = capTraceValue(child)
		}
		return out
	}
	return value
}

func contextWithTraceBody(r *http.Request, body []byte) context.Context {
	return context.WithValue(r.Context(), traceBodyKey{}, body)
}

type requestTraceDiag struct {
	aclMode        string
	itemMeta       map[string]map[string]any
	armErrors      map[string]string
	canonicalSeeds []string
	askerEEID      int
	askerSlackID   string
	askerDecoded   bool
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

// traceACLMode reports the decision the handler recorded in diag; it never
// recomputes one from the raw request.
func traceACLMode(r *http.Request) string {
	if diag, _ := r.Context().Value(requestTraceDiagKey{}).(*requestTraceDiag); diag != nil && diag.aclMode != "" {
		return diag.aclMode
	}
	return "not-evaluated"
}

// insertRequestID adds "request_id" right after the leading brace of a JSON
// object without re-encoding the rest. Non-objects are returned unchanged.
func insertRequestID(body []byte, id string) []byte {
	space := func(b byte) bool { return b == ' ' || b == '\t' || b == '\r' || b == '\n' }
	i := 0
	for i < len(body) && space(body[i]) {
		i++
	}
	if i >= len(body) || body[i] != '{' {
		return body
	}
	j := i + 1
	for j < len(body) && space(body[j]) {
		j++
	}
	member, _ := json.Marshal(id)
	insert := append([]byte(`"request_id":`), member...)
	if j < len(body) && body[j] != '}' {
		insert = append(insert, ',')
	}
	out := make([]byte, 0, len(body)+len(insert))
	out = append(out, body[:i+1]...)
	out = append(out, insert...)
	return append(out, body[i+1:]...)
}
