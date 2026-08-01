package handlers

import (
	"context"
	"testing"

	"github.com/rs/zerolog"
)

func TestConfirmTopicLinkPreambleWrappedJSON(t *testing.T) {
	gem := &mockGemini{cheapGenerateResult: func() (string, error) {
		return "Here is the judgment:\n```json\n" +
			`{"same_topic":true,"confidence":0.91,"topic":"Checkout blacklist","why":"same issue"}` + "\n```", nil
	}}

	got, err := confirmTopicLink(context.Background(), Deps{Gemini: gem},
		topicLinkNode{NodeID: "slack:C:1", Type: "slack", Summary: "Checkout blacklist"},
		topicLinkCandidate{topicLinkNode: topicLinkNode{NodeID: "jira:PAY-1", Type: "jira", Summary: "Checkout blacklist"}},
		topicLinkContext{},
	)
	if err != nil {
		t.Fatalf("confirmTopicLink() error = %v", err)
	}
	if !got.SameTopic || got.Confidence != 0.91 {
		t.Errorf("confirmTopicLink() = %+v", got)
	}
}

func TestGenScopePreambleWrappedJSON(t *testing.T) {
	gem := &mockGemini{generateResult: func() (string, error) {
		return "Here is the scope:\n```json\n" +
			`{"scope_definition":"Payments only","summary":"Payment systems"}` + "\n```", nil
	}}

	scope, summary := genScope(context.Background(), Deps{Gemini: gem}, "payments", []string{"Payments"}, nil)
	if scope != "Payments only" || summary != "Payment systems" {
		t.Errorf("genScope() = %q, %q", scope, summary)
	}
}

func TestGenClusterSummaryPreambleWrappedJSON(t *testing.T) {
	gem := &mockGemini{generateResult: func() (string, error) {
		return "Here is the summary:\n```json\n" +
			`{"overview":"Checkout failed. [T1]","highlights":["Failure reported. [T1]"]}` + "\n```", nil
	}}

	overview, highlights := genClusterSummary(context.Background(), gem, "[T1] transcript")
	if overview != "Checkout failed. [T1]" || len(highlights) != 1 || highlights[0] != "Failure reported. [T1]" {
		t.Errorf("genClusterSummary() = %q, %v", overview, highlights)
	}
}

func TestGenThreadDeepSummaryPreambleWrappedJSON(t *testing.T) {
	gem := &mockGemini{generateResult: func() (string, error) {
		return "Here is the summary:\n```json\n" +
			`{"topic":"Checkout failure","overview":"Checkout failed.","highlights":["Failure reported."],"kind":"substantive"}` + "\n```", nil
	}}

	topic, overview, highlights, kind := genThreadDeepSummary(context.Background(), gem, "transcript")
	if topic != "Checkout failure" || overview != "Checkout failed." || len(highlights) != 1 || kind != "substantive" {
		t.Errorf("genThreadDeepSummary() = %q, %q, %v, %q", topic, overview, highlights, kind)
	}
}

func TestJudgeTopicPreambleWrappedJSON(t *testing.T) {
	gem := &mockGemini{generateResult: func() (string, error) {
		return "Here is the judgment:\n```json\n{\"relevant\":true}\n```", nil
	}}

	relevant, ok := judgeTopic(context.Background(), Deps{Gemini: gem, Logger: zerolog.Nop()}, "payments", hotThread{Blob: "payment failed"})
	if !ok || !relevant {
		t.Errorf("judgeTopic() = %v, %v", relevant, ok)
	}
}
