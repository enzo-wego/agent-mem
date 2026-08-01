// Package llmjson contains response parsing helpers shared by LLM clients and callers.
package llmjson

import (
	"encoding/json"
	"strings"
)

// ExtractJSON returns the JSON body of an LLM response.
//
// The OpenRouter json_object contract is gone, and the Agent SDK does not
// replace it without a schema; agent-mem passes no schema. This lives in a leaf
// package because llmgateway already imports gemini, so putting it there would
// create an import cycle.
func ExtractJSON(response string) []byte {
	trimmed := strings.TrimSpace(response)
	original := []byte(trimmed)
	if json.Valid(original) {
		return original
	}

	markers, fenceCount := fenceMarkers(trimmed)
	if fenceCount >= len(markers) {
		return original
	}
	if fenceCount == 2 {
		start := markers[0] + len("```")
		end := markers[1]
		inside := strings.TrimSpace(trimmed[start:end])
		if body := []byte(inside); json.Valid(body) {
			return body
		}
		if newline := strings.IndexByte(inside, '\n'); newline >= 0 {
			body := []byte(strings.TrimSpace(inside[newline+1:]))
			if json.Valid(body) {
				return body
			}
		}
	}

	if body := delimitedJSON(trimmed, '{', '}'); body != nil {
		return body
	}
	if body := delimitedJSON(trimmed, '[', ']'); body != nil {
		return body
	}
	return original
}

func fenceMarkers(response string) ([4]int, int) {
	var markers [4]int
	count := 0
	for lineStart := 0; ; {
		lineEnd := strings.IndexByte(response[lineStart:], '\n')
		if lineEnd < 0 {
			lineEnd = len(response)
		} else {
			lineEnd += lineStart
		}

		marker := lineStart
		for marker < lineEnd && (response[marker] == ' ' || response[marker] == '\t') {
			marker++
		}
		if strings.HasPrefix(response[marker:lineEnd], "```") {
			markers[count] = marker
			count++
			if count == len(markers) {
				return markers, count
			}
		}
		if lineEnd == len(response) {
			return markers, count
		}
		lineStart = lineEnd + 1
	}
}

func delimitedJSON(response string, open, close byte) []byte {
	start := strings.IndexByte(response, open)
	end := strings.LastIndexByte(response, close)
	if start < 0 || end <= start {
		return nil
	}
	body := []byte(response[start : end+1])
	if !json.Valid(body) {
		return nil
	}
	return body
}
