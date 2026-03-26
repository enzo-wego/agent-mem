package worker

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
)

var systemReminderRe = regexp.MustCompile(`(?s)<system-reminder>.*?</system-reminder>`)

// extractLastAssistantMessage reads a Claude Code JSONL transcript file
// and returns the last assistant message text with system-reminder tags stripped.
func extractLastAssistantMessage(transcriptPath string) (string, error) {
	f, err := os.Open(transcriptPath)
	if err != nil {
		return "", fmt.Errorf("open transcript: %w", err)
	}
	defer f.Close()

	// Read all lines, then search backward for the last assistant message
	var lines []string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024) // 10MB max line
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("read transcript: %w", err)
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

	return "", fmt.Errorf("no assistant message found in transcript")
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
