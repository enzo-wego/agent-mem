package gemini

import (
	"encoding/json"
	"fmt"
	"strings"
)

// --- Observation Extraction ---

const ObservationSystemPrompt = `You are an observation extractor for a developer's coding session.
Given a tool use event (tool name, input, output), extract a structured observation.

Output JSON:
{
  "type": "decision|bugfix|feature|refactor|discovery",
  "title": "Short title (max 80 chars)",
  "subtitle": "One-line description",
  "narrative": "2-3 sentences explaining what happened and why",
  "facts": ["specific fact 1", "specific fact 2"],
  "concepts": ["pattern", "how-it-works"],
  "files_read": ["path/to/file.go"],
  "files_modified": ["path/to/other.go"]
}

Rules:
- Be concise. Title should be scannable.
- Narrative should explain WHY, not just WHAT.
- Facts are specific, reusable pieces of knowledge.
- Only include files actually mentioned in the tool event.
- If the tool event is trivial (e.g., listing files, reading a small config), return {"skip": true}.`

// ObservationResult is the structured observation extracted by Gemini.
type ObservationResult struct {
	Skip          bool     `json:"skip"`
	Type          string   `json:"type"`
	Title         string   `json:"title"`
	Subtitle      string   `json:"subtitle"`
	Narrative     string   `json:"narrative"`
	Facts         []string `json:"facts"`
	Concepts      []string `json:"concepts"`
	FilesRead     []string `json:"files_read"`
	FilesModified []string `json:"files_modified"`
}

// BuildObservationPrompt constructs the user message for observation extraction.
func BuildObservationPrompt(toolName string, toolInput, toolResponse json.RawMessage, cwd, project string) string {
	inputStr := truncate(string(toolInput), 4000)
	responseStr := truncate(string(toolResponse), 4000)

	return fmt.Sprintf(`Tool: %s
Project: %s
Working Directory: %s

Input:
%s

Output:
%s`, toolName, project, cwd, inputStr, responseStr)
}

// ParseObservation parses the JSON response from Gemini into an ObservationResult.
func ParseObservation(response string) (*ObservationResult, error) {
	var obs ObservationResult
	if err := json.Unmarshal([]byte(response), &obs); err != nil {
		return nil, fmt.Errorf("parse observation: %w", err)
	}
	return &obs, nil
}

// --- Summary Extraction ---

const SummarySystemPrompt = `You are a session summarizer. Given the last assistant message from a coding session,
extract a structured summary.

Output JSON:
{
  "request": "What the user originally asked for",
  "investigated": "What was explored/researched",
  "learned": "Key insights discovered",
  "completed": "What was actually accomplished",
  "next_steps": "What remains to be done"
}`

// SummaryResult is the structured summary extracted by Gemini.
type SummaryResult struct {
	Request      string `json:"request"`
	Investigated string `json:"investigated"`
	Learned      string `json:"learned"`
	Completed    string `json:"completed"`
	NextSteps    string `json:"next_steps"`
}

// BuildSummaryPrompt constructs the user message for summary extraction.
func BuildSummaryPrompt(lastAssistantMessage, project string) string {
	msg := truncate(lastAssistantMessage, 8000)
	return fmt.Sprintf("Project: %s\n\nLast assistant message:\n%s", project, msg)
}

// ParseSummary parses the JSON response from Gemini into a SummaryResult.
func ParseSummary(response string) (*SummaryResult, error) {
	var summary SummaryResult
	if err := json.Unmarshal([]byte(response), &summary); err != nil {
		return nil, fmt.Errorf("parse summary: %w", err)
	}
	return &summary, nil
}

// --- Embedding Text ---

// BuildEmbeddingText constructs the text used for generating an observation embedding.
func BuildEmbeddingText(title, subtitle, narrative string, facts []string) string {
	parts := []string{title, subtitle, narrative}
	if len(facts) > 0 {
		parts = append(parts, strings.Join(facts, " "))
	}
	return strings.Join(parts, " ")
}

// BuildSummaryEmbeddingText constructs the text for a summary embedding.
func BuildSummaryEmbeddingText(s *SummaryResult) string {
	parts := []string{s.Request, s.Investigated, s.Learned, s.Completed, s.NextSteps}
	return strings.Join(parts, " ")
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "... (truncated)"
}
