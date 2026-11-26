package hooks

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestNormalizeProvider(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "empty defaults to claude", input: "", want: ProviderClaude},
		{name: "claude stays claude", input: "claude", want: ProviderClaude},
		{name: "codex allowed", input: "codex", want: ProviderCodex},
		{name: "case insensitive", input: "CODEX", want: ProviderCodex},
		{name: "explicit OmO suffix", input: " OmO ", want: "omo"},
		{name: "unknown rejected", input: "other", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := NormalizeProvider(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q", tt.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeProvider(%q) returned error: %v", tt.input, err)
			}
			if got != tt.want {
				t.Fatalf("NormalizeProvider(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestRunHookOmOSessionStartRecall(t *testing.T) {
	for _, tt := range []struct {
		name     string
		provider string
		status   int
		want     map[string]any
	}{
		{
			name: "successful recall", provider: "omo", status: http.StatusOK,
			want: map[string]any{"hookSpecificOutput": map[string]any{
				"hookEventName": "SessionStart", "additionalContext": "worker response",
			}},
		},
		{
			name: "worker error is not context", provider: " OmO ", status: http.StatusBadRequest,
			want: map[string]any{},
		},
		{
			name: "existing Claude behavior", provider: "", status: http.StatusBadRequest,
			want: map[string]any{"hookSpecificOutput": map[string]any{
				"hookEventName": "SessionStart", "additionalContext": "worker response",
			}},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var payload struct {
					SessionID string `json:"session_id"`
					CWD       string `json:"cwd"`
				}
				if r.Method != http.MethodPost || r.URL.Path != "/api/hook/session-start" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || payload.SessionID != "test-session" || payload.CWD != "/test/project" {
					t.Errorf("request payload = %+v, error = %v", payload, err)
				}
				w.WriteHeader(tt.status)
				fmt.Fprint(w, "worker response")
			}))
			defer server.Close()
			port := server.Listener.Addr().(*net.TCPAddr).Port

			dir := t.TempDir()
			input := filepath.Join(dir, "payload.json")
			if err := os.WriteFile(input, []byte(`{"session_id":"test-session","cwd":"/test/project"}`), 0o600); err != nil {
				t.Fatal(err)
			}
			in, err := os.Open(input)
			if err != nil {
				t.Fatal(err)
			}
			defer in.Close()
			out, err := os.CreateTemp(dir, "response-*")
			if err != nil {
				t.Fatal(err)
			}
			defer out.Close()

			stdin, stdout := os.Stdin, os.Stdout
			os.Stdin, os.Stdout = in, out
			err = RunHook("session-start", tt.provider, port)
			os.Stdin, os.Stdout = stdin, stdout
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(out.Name())
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]any
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("hook stdout = %s, want %+v", data, tt.want)
			}
		})
	}
}

func TestNormalizeHookPayloadDefaultsToClaudeAliases(t *testing.T) {
	t.Parallel()

	input := []byte(`{"session_id":"sess-1","prompt":"fix this","tool_name":"Read"}`)

	payload, err := normalizeHookPayload(input, "", "prompt-submit")
	if err != nil {
		t.Fatalf("normalizeHookPayload returned error: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}

	if got["session_id"] != "sess-1" {
		t.Fatalf("session_id = %#v, want %q", got["session_id"], "sess-1")
	}
	if got["prompt"] != "fix this" {
		t.Fatalf("prompt = %#v, want %q", got["prompt"], "fix this")
	}
	if got["tool_name"] != "Read" {
		t.Fatalf("tool_name = %#v, want %q", got["tool_name"], "Read")
	}
	if _, ok := got["cwd"]; !ok {
		t.Fatal("expected cwd to be populated")
	}
}

func TestNormalizeHookPayloadCodexAliases(t *testing.T) {
	t.Parallel()

	input := []byte(`{
		"sessionId":"sess-2",
		"input":"search memory",
		"toolName":"Bash",
		"toolInput":{"cmd":"pwd"},
		"toolResponse":{"output":"ok"},
		"transcriptPath":"/tmp/trace.jsonl",
		"lastAssistantMessage":"done"
	}`)

	payload, err := normalizeHookPayload(input, ProviderCodex, "post-tool-use")
	if err != nil {
		t.Fatalf("normalizeHookPayload returned error: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}

	assertEqual(t, got["session_id"], "sess-2", "session_id")
	assertEqual(t, got["prompt"], "search memory", "prompt")
	assertEqual(t, got["tool_name"], "Bash", "tool_name")
	assertEqual(t, got["transcript_path"], "/tmp/trace.jsonl", "transcript_path")
	assertEqual(t, got["last_assistant_message"], "done", "last_assistant_message")

	toolInput, ok := got["tool_input"].(map[string]any)
	if !ok {
		t.Fatalf("tool_input type = %T, want map[string]any", got["tool_input"])
	}
	assertEqual(t, toolInput["cmd"], "pwd", "tool_input.cmd")
}

func assertEqual(t *testing.T, got any, want any, field string) {
	t.Helper()
	if got != want {
		t.Fatalf("%s = %#v, want %#v", field, got, want)
	}
}
