package handlers

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// topic_rules.json is the single source of truth for "same topic" decisions:
// the LLM judge applies it at runtime, /live/rules renders it for humans, and
// agents read the same file in the repo. Mirrors the importance.json pattern.
//
//go:embed topic_rules.json
var topicRulesJSON []byte

type topicRuleTag struct {
	Tag              string `json:"tag"`
	ClassifyWhen     string `json:"classify_when"`
	SameWhen         string `json:"same_when"`
	DifferentWhen    string `json:"different_when"`
	ExampleSame      string `json:"example_same"`
	ExampleDifferent string `json:"example_different"`
}

type topicRules struct {
	Version     int            `json:"version"`
	Tags        []topicRuleTag `json:"tags"`
	TieBreakers []string       `json:"tie_breakers"`
	Domains     map[string]any `json:"domains"`
}

var loadedTopicRules = func() topicRules {
	var r topicRules
	if err := json.Unmarshal(topicRulesJSON, &r); err != nil {
		panic(fmt.Sprintf("topic_rules.json invalid: %v", err)) // embed-time content; fail fast at startup
	}
	return r
}()

// topicRulesPromptDigest renders the rules compactly for the judge's system
// prompt. Kept terse: the confirm gate is high-volume on a cheap model.
func topicRulesPromptDigest() string {
	var b strings.Builder
	b.WriteString("TAGS — classify each artifact into exactly one, then apply that tag's criteria:\n")
	for _, t := range loadedTopicRules.Tags {
		fmt.Fprintf(&b, "- %s (%s)\n  SAME when: %s\n  DIFFERENT when: %s\n", t.Tag, t.ClassifyWhen, t.SameWhen, t.DifferentWhen)
	}
	b.WriteString("TIE-BREAKERS:\n")
	for _, tb := range loadedTopicRules.TieBreakers {
		b.WriteString("- " + tb + "\n")
	}
	if pay, ok := loadedTopicRules.Domains["payments"].(map[string]any); ok {
		if partners, ok := pay["partners"].([]any); ok {
			parts := make([]string, 0, len(partners))
			for _, p := range partners {
				parts = append(parts, fmt.Sprint(p))
			}
			b.WriteString("PAYMENTS PARTNERS (partner alone is never enough): " + strings.Join(parts, "; ") + "\n")
		}
	}
	return b.String()
}

// NewTopicRulesHandler serves the raw rules JSON for the /live/rules page.
func NewTopicRulesHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(topicRulesJSON)
	})
}
