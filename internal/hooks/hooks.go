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

const (
	ProviderClaude = "claude"
	ProviderCodex  = "codex"
	ProviderGemini = "gemini"
)

// RunHook is the thin CLI entry point for all hook subcommands.
// It reads stdin JSON, normalizes provider-specific payloads, POSTs to the
// worker, and writes the response to stdout.
func RunHook(event, provider string, port int) error {
	// Read stdin (may be empty for session-start)
	input, _ := io.ReadAll(os.Stdin)

	payload, err := normalizeHookPayload(input, provider, event)
	if err != nil {
		return err
	}

	// POST to worker with short timeout
	client := &http.Client{Timeout: 25 * time.Second}
	url := fmt.Sprintf("http://localhost:%d/api/hook/%s", port, event)

	resp, err := client.Post(url, "application/json", bytes.NewReader(payload))
	if err != nil {
		// Worker not running — exit silently but return continue signal to not block the agent
		if event == "session-start" {
			fmt.Println("{}")
			return nil
		}
		resolvedProvider, _ := NormalizeProvider(provider)
		if resolvedProvider == ProviderGemini {
			fmt.Println(`{"continue":true,"decision":"allow"}`)
		} else {
			fmt.Println(`{"continue":true}`)
		}
		return nil
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	// For session-start: wrap response as hookSpecificOutput for context injection
	if event == "session-start" {
		context := string(body)
		if len(context) > 0 {
			// Providers may have different response requirements.
			// Claude/Codex use hookSpecificOutput.
			// Gemini may use a different structure if it starts supporting it,
			// but for now we follow the general pattern.
			output := map[string]any{
				"hookSpecificOutput": map[string]any{
					"hookEventName":     "SessionStart",
					"additionalContext": context,
				},
			}
			json.NewEncoder(os.Stdout).Encode(output)
		} else {
			// Always return valid JSON to avoid hook failure in Gemini CLI
			fmt.Println("{}")
		}
	} else {
		// All other hooks: return continue signal
		resolvedProvider, _ := NormalizeProvider(provider)
		if resolvedProvider == ProviderGemini {
			fmt.Println(`{"continue":true,"decision":"allow"}`)
		} else {
			fmt.Println(`{"continue":true,"suppressOutput":true}`)
		}
	}

	return nil
}

// NormalizeProvider resolves the optional provider suffix while keeping the
// existing Claude path as the default contract.
func NormalizeProvider(provider string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "", ProviderClaude:
		return ProviderClaude, nil
	case ProviderCodex:
		return ProviderCodex, nil
	case ProviderGemini:
		return ProviderGemini, nil
	default:
		return "", fmt.Errorf("unsupported hook provider %q", provider)
	}
}

func normalizeHookPayload(input []byte, provider, event string) ([]byte, error) {
	resolvedProvider, err := NormalizeProvider(provider)
	if err != nil {
		return nil, err
	}

	payload := parsePayload(input)
	normalizeCommonFields(payload)
	addCWDToMap(payload)

	// For Claude stop events: read transcript NOW while the file still exists.
	// Claude Code may delete the transcript file after the hook returns, so we
	// extract the content in the CLI process, not in the worker.
	if resolvedProvider == ProviderClaude && event == "stop" {
		extractTranscriptContentInPlace(payload)
	}

	result, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return result, nil
}

func parsePayload(input []byte) map[string]any {
	if len(input) == 0 {
		return make(map[string]any)
	}

	var payload map[string]any
	if err := json.Unmarshal(input, &payload); err != nil {
		return make(map[string]any)
	}
	if payload == nil {
		return make(map[string]any)
	}
	return payload
}

func normalizeCommonFields(payload map[string]any) {
	copyAlias(payload, "session_id", "sessionId")
	copyAlias(payload, "prompt", "input", "user_prompt", "userPrompt", "text")
	copyAlias(payload, "tool_name", "toolName")
	copyAlias(payload, "tool_input", "toolInput")
	copyAlias(payload, "tool_response", "toolResponse")
	copyAlias(payload, "transcript_path", "transcriptPath")
	copyAlias(payload, "last_assistant_message", "lastAssistantMessage")
}

func copyAlias(payload map[string]any, target string, aliases ...string) {
	if value, ok := payload[target]; ok && !isEmptyValue(value) {
		return
	}
	for _, alias := range aliases {
		if value, ok := payload[alias]; ok && !isEmptyValue(value) {
			payload[target] = value
			return
		}
	}
}

func addCWDToMap(payload map[string]any) {
	if value, ok := payload["cwd"]; ok && !isEmptyValue(value) {
		return
	}

	cwd, _ := os.Getwd()
	payload["cwd"] = cwd
}

func isEmptyValue(value any) bool {
	switch v := value.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(v) == ""
	default:
		return false
	}
}

// extractTranscriptContent reads the transcript file referenced in the payload,
// extracts the last assistant message, and replaces transcript_path with the
// actual content. This must happen in the CLI process because Claude Code may
// clean up the transcript file after the hook returns.
func extractTranscriptContent(payload []byte) []byte {
	data := parsePayload(payload)
	if len(data) == 0 && len(payload) > 0 {
		return payload
	}

	extractTranscriptContentInPlace(data)

	result, err := json.Marshal(data)
	if err != nil {
		return payload
	}
	return result
}

func extractTranscriptContentInPlace(data map[string]any) {
	transcriptPath, _ := data["transcript_path"].(string)
	if transcriptPath == "" {
		return
	}

	msg, err := readLastAssistantMessage(transcriptPath)
	if err != nil {
		// Can't read file — leave transcript_path for worker to handle/log
		return
	}

	// Replace transcript_path with the extracted content
	data["last_assistant_message"] = msg
	delete(data, "transcript_path")
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
	payload := parsePayload(input)
	addCWDToMap(payload)
	result, _ := json.Marshal(payload)
	return result
}
