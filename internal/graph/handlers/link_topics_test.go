package handlers

import (
	"context"
	"errors"
	"testing"

	"github.com/agent-mem/agent-mem/internal/graph/jobs"
)

func TestConfirmTopicLinkReturnsTransientOnGenerateError(t *testing.T) {
	gem := &mockGemini{}
	gem.cheapGenerateResult = func() (string, error) {
		return "", errors.New("rate limited")
	}
	deps := Deps{Gemini: gem}

	if _, err := confirmTopicLink(context.Background(), deps,
		topicLinkNode{NodeID: "slack:C:1", Type: "slack", Summary: "Checkout email blacklist"},
		topicLinkCandidate{topicLinkNode: topicLinkNode{NodeID: "jira:PAY-1", Type: "jira", Summary: "Checkout email blacklist"}, Cosine: 0.92},
	); err == nil {
		t.Fatal("expected transient error")
	} else if !errors.Is(err, jobs.ErrTransient) {
		t.Fatalf("error = %v, want jobs.ErrTransient", err)
	}
}

func TestConfirmTopicLinkReturnsTransientOnUnparseableJSON(t *testing.T) {
	gem := &mockGemini{}
	gem.cheapGenerateResult = func() (string, error) {
		return "not json", nil
	}
	deps := Deps{Gemini: gem}

	if _, err := confirmTopicLink(context.Background(), deps,
		topicLinkNode{NodeID: "slack:C:1", Type: "slack", Summary: "Checkout email blacklist"},
		topicLinkCandidate{topicLinkNode: topicLinkNode{NodeID: "jira:PAY-1", Type: "jira", Summary: "Checkout email blacklist"}, Cosine: 0.92},
	); err == nil {
		t.Fatal("expected transient error")
	} else if !errors.Is(err, jobs.ErrTransient) {
		t.Fatalf("error = %v, want jobs.ErrTransient", err)
	}
}

func TestConfirmTopicLinkUsesCheapGeneratePath(t *testing.T) {
	gem := &mockGemini{}
	gem.cheapGenerateResult = func() (string, error) {
		return `{"same_topic":true,"confidence":0.91,"topic":"Checkout blacklist","why":"Both describe blocking checkout emails"}`, nil
	}
	deps := Deps{Gemini: gem}

	j, err := confirmTopicLink(context.Background(), deps,
		topicLinkNode{NodeID: "slack:C:1", Type: "slack", Summary: "Checkout email blacklist"},
		topicLinkCandidate{topicLinkNode: topicLinkNode{NodeID: "jira:PAY-1", Type: "jira", Summary: "Checkout email blacklist"}, Cosine: 0.92},
	)
	if err != nil {
		t.Fatalf("confirmTopicLink: %v", err)
	}
	if !j.SameTopic || j.Confidence != 0.91 {
		t.Fatalf("judgment = %+v", j)
	}
	if gem.cheapGenerateCalls.Load() != 1 {
		t.Fatalf("cheap generate calls = %d, want 1", gem.cheapGenerateCalls.Load())
	}
	if gem.generateUser != "" {
		t.Fatalf("expensive Generate path was used: %q", gem.generateUser)
	}
}

func TestIndexArtifactNeverForcesTopicRelink(t *testing.T) {
	if got := linkTopicsForceFromIndexArtifact(true); got {
		t.Fatal("index_artifact force should not force link_topics rejudgment")
	}
	if got := linkTopicsForceFromIndexArtifact(false); got {
		t.Fatal("non-forced index_artifact should not force link_topics rejudgment")
	}
}

func TestTopicLinkSourceIsSkippedForSlackDM(t *testing.T) {
	if !skipTopicLinkSource(topicLinkNode{Type: "slack", Scope: "slack:D123", Summary: "private thread"}) {
		t.Fatal("Slack DM source should be skipped")
	}
	if skipTopicLinkSource(topicLinkNode{Type: "slack", Scope: "slack:C123", Summary: "public channel thread"}) {
		t.Fatal("public Slack channel source should not be skipped")
	}
}
