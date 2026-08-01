package llmjson

import "testing"

func TestExtractJSON(t *testing.T) {
	tests := []struct {
		name     string
		response string
		want     string
	}{
		{
			name:     "bare object",
			response: "  {\"ok\":true}\n",
			want:     `{"ok":true}`,
		},
		{
			name:     "fenced whole response",
			response: "```json\n{\"ok\":true}\n```",
			want:     `{"ok":true}`,
		},
		{
			name:     "preamble and fenced block",
			response: "Here is the observation:\n```json\n{\"ok\":true}\n```",
			want:     `{"ok":true}`,
		},
		{
			name:     "fenced block and trailing prose",
			response: "```\n{\"ok\":true}\n```\nHope that helps.",
			want:     `{"ok":true}`,
		},
		{
			name:     "prose around object",
			response: "Result: {\"ok\":true} done.",
			want:     `{"ok":true}`,
		},
		{
			name:     "prose around array",
			response: "Result: [1,2,3] done.",
			want:     `[1,2,3]`,
		},
		{
			name:     "unmatched fence falls back to object",
			response: "Here is the result:\n```\n{\"ok\":true}",
			want:     `{"ok":true}`,
		},
		{
			name:     "stray marker and fenced block fall back to object",
			response: "Note ```\nResult:\n```json\n{\"ok\":true}\n```",
			want:     `{"ok":true}`,
		},
		{
			name:     "backticks inside fenced JSON string",
			response: "```json\n{\"narrative\":\"used ```go``` snippets\"}\n```",
			want:     "{\"narrative\":\"used ```go``` snippets\"}",
		},
		{
			name:     "two fenced blocks are ambiguous",
			response: "First:\n```json\n{\"first\":true}\n```\nSecond:\n```json\n{\"second\":true}\n```",
			want:     "First:\n```json\n{\"first\":true}\n```\nSecond:\n```json\n{\"second\":true}\n```",
		},
		{
			name:     "not JSON",
			response: "  no structured response  ",
			want:     "no structured response",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := string(ExtractJSON(tt.response)); got != tt.want {
				t.Errorf("ExtractJSON() = %q, want %q", got, tt.want)
			}
		})
	}
}
