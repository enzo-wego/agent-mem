package hooks

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// RunHook is the thin CLI entry point for all hook subcommands.
// It reads stdin JSON, adds CWD, POSTs to the worker, and writes the response to stdout.
func RunHook(event string, port int) error {
	// Read stdin (may be empty for session-start)
	input, _ := io.ReadAll(os.Stdin)

	// Build payload with CWD
	payload := addCWD(input)

	// POST to worker with short timeout
	client := &http.Client{Timeout: 25 * time.Second}
	url := fmt.Sprintf("http://localhost:%d/api/hook/%s", port, event)

	resp, err := client.Post(url, "application/json", bytes.NewReader(payload))
	if err != nil {
		// Worker not running — exit silently (don't break Claude Code)
		return nil
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	// For session-start: wrap response as hookSpecificOutput for context injection
	if event == "session-start" {
		context := string(body)
		if len(context) > 0 {
			output := map[string]any{
				"hookSpecificOutput": map[string]any{
					"hookEventName":    "SessionStart",
					"additionalContext": context,
				},
			}
			json.NewEncoder(os.Stdout).Encode(output)
		}
	} else {
		// All other hooks: return continue signal
		fmt.Fprint(os.Stdout, `{"continue":true,"suppressOutput":true}`)
	}

	return nil
}

// addCWD merges the current working directory into the JSON payload.
func addCWD(input []byte) []byte {
	cwd, _ := os.Getwd()

	var payload map[string]any
	if len(input) > 0 {
		if err := json.Unmarshal(input, &payload); err != nil {
			payload = make(map[string]any)
		}
	} else {
		payload = make(map[string]any)
	}

	// Only set CWD if not already present
	if _, ok := payload["cwd"]; !ok {
		payload["cwd"] = cwd
	}

	result, _ := json.Marshal(payload)
	return result
}
