package hooks

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
)

var systemReminderRe = regexp.MustCompile(`(?s)<system-reminder>.*?</system-reminder>`)

// RunHook is the thin CLI entry point for all hook subcommands.
// It reads stdin JSON, adds CWD, POSTs to the worker, and writes the response to stdout.
func RunHook(event string, port int) error {
	// Read stdin (may be empty for session-start)
	input, _ := io.ReadAll(os.Stdin)

	// Build payload with CWD
	payload := addCWD(input)

	// For stop events: read transcript NOW while the file still exists.
	// Claude Code may delete the transcript file after the hook returns,
	// so we must extract the content in the CLI process, not in the worker.
	if event == "stop" {
		payload = extractTranscriptContent(payload)
	}

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

// extractTranscriptContent reads the transcript file referenced in the payload,
// extracts the last assistant message, and replaces transcript_path with the
// actual content. This must happen in the CLI process because Claude Code may
// clean up the transcript file after the hook returns.
func extractTranscriptContent(payload []byte) []byte {
	var data map[string]any
	if err := json.Unmarshal(payload, &data); err != nil {
		return payload
	}

	transcriptPath, _ := data["transcript_path"].(string)
	if transcriptPath == "" {
		return payload
	}

	msg, err := readLastAssistantMessage(transcriptPath)
	if err != nil {
		// Can't read file — leave transcript_path for worker to handle/log
		return payload
	}

	// Replace transcript_path with the extracted content
	data["last_assistant_message"] = msg
	delete(data, "transcript_path")

	result, err := json.Marshal(data)
	if err != nil {
		return payload
	}
	return result
}

// readLastAssistantMessage reads a Claude Code JSONL transcript and returns
// the last assistant message text with system-reminder tags stripped.
func readLastAssistantMessage(transcriptPath string) (string, error) {
	f, err := os.Open(transcriptPath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	var lines []string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024) // 10MB max line
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}

	// Iterate backward to find last assistant message
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}

		var entry struct {
			Type    string `json:"type"`
			Message struct {
				Role    string `json:"role"`
				Content any    `json:"content"`
			} `json:"message"`
		}

		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}

		if entry.Type != "assistant" && entry.Message.Role != "assistant" {
			continue
		}

		text := extractContentText(entry.Message.Content)
		if text == "" {
			continue
		}

		// Strip system-reminder tags
		text = systemReminderRe.ReplaceAllString(text, "")
		text = strings.TrimSpace(text)
		return text, nil
	}

	return "", fmt.Errorf("no assistant message found")
}

// extractContentText extracts text from content which may be a string or array of content blocks.
func extractContentText(content any) string {
	switch v := content.(type) {
	case string:
		return v
	case []any:
		var parts []string
		for _, item := range v {
			if block, ok := item.(map[string]any); ok {
				if block["type"] == "text" {
					if text, ok := block["text"].(string); ok {
						parts = append(parts, text)
					}
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
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
