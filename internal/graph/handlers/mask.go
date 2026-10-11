package handlers

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"

	"github.com/agent-mem/agent-mem/internal/redact"
)

// ACLModeFilteredMasked tags traces whose response went through the masker.
// Replay cannot reproduce masked digests (see cmd/agent-mem/trace.go).
const ACLModeFilteredMasked = "filtered-masked:v1"

// maskGraphResponse masks emails, phone numbers and card-like numbers in every
// string of a non-admin JSON response (D2). Admin requests pass through
// unbuffered. Responses with status >= 400 and a non-JSON body (handlers use plain-text
// http.Error) have the text masked and keep their status. Anything else that is
// not a single valid JSON document, and any panic, is replaced by 500
// {"error":"mask_failed"}; nothing of the original body is written.
//
// Mount it inside withContextTrace so traces hash what the client received.
func maskGraphResponse(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isAdminRequest(r) {
			next.ServeHTTP(w, r)
			return
		}
		bw := &bufferedResponse{header: make(http.Header)}
		panicked := serveRecovering(next, bw, r)
		if diag, _ := r.Context().Value(requestTraceDiagKey{}).(*requestTraceDiag); diag != nil && diag.aclMode == "filtered" {
			diag.aclMode = ACLModeFilteredMasked
		}
		status := bw.status
		if status == 0 {
			status = http.StatusOK
		}
		var out []byte
		switch {
		case panicked:
		case isJSONContentType(bw.header.Get("Content-Type")):
			out = maskJSON(bw.body.Bytes())
		case status >= 400:
			// Handlers answer errors with plain-text http.Error: mask the text, keep the status.
			out = []byte(redact.Mask(bw.body.String()))
		}
		if out == nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"mask_failed"}` + "\n"))
			return
		}
		for key, values := range bw.header {
			if key == "Content-Length" || key == "Content-Encoding" || key == "Etag" {
				continue
			}
			for _, v := range values {
				w.Header().Add(key, v)
			}
		}
		w.WriteHeader(status)
		_, _ = w.Write(out)
	})
}

// serveRecovering runs next and reports whether it panicked (a partial write
// is then discarded by the caller).
func serveRecovering(next http.Handler, w http.ResponseWriter, r *http.Request) (panicked bool) {
	defer func() {
		if recover() != nil {
			panicked = true
		}
	}()
	next.ServeHTTP(w, r)
	return false
}

func isJSONContentType(ct string) bool {
	mt, _, err := mime.ParseMediaType(ct)
	return err == nil && mt == "application/json"
}

// maskJSON decodes body as exactly one JSON document, masks every string value
// at every depth and re-encodes it. nil means failure (fail closed).
func maskJSON(body []byte) []byte {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil // trailing data
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(maskValue(doc)); err != nil {
		return nil
	}
	return buf.Bytes()
}

func maskValue(v any) any {
	switch t := v.(type) {
	case string:
		return redact.Mask(t)
	case []any:
		for i := range t {
			t[i] = maskValue(t[i])
		}
		return t
	case map[string]any:
		for k, e := range t {
			t[k] = maskValue(e)
		}
		return t
	default:
		return v
	}
}
