package handlers

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-mem/agent-mem/internal/graph/jobs"
	"github.com/agent-mem/agent-mem/internal/llmgateway"
)

// F-1: one failing confirm must not cancel siblings that already hold a gateway slot.
func TestRunTopicLinkConfirmsDoesNotCancelRunningSiblings(t *testing.T) {
	cands := []topicLinkCandidate{
		{topicLinkNode: topicLinkNode{NodeID: "a", Summary: "a"}},
		{topicLinkNode: topicLinkNode{NodeID: "b", Summary: "b"}},
		{topicLinkNode: topicLinkNode{NodeID: "c", Summary: "c"}},
		{topicLinkNode: topicLinkNode{NodeID: "d", Summary: "d"}},
	}
	boom := errors.New("boom")
	release := make(chan struct{})
	started := make(chan string, len(cands))
	var finished, cancelled, launchedD int32
	done := make(chan error, 1)

	go func() {
		done <- runTopicLinkConfirms(context.Background(), cands, func(ctx context.Context, c topicLinkCandidate) error {
			switch c.NodeID {
			case "a":
				// fail only once both siblings are in flight
				<-started
				<-started
				return boom
			case "d":
				atomic.AddInt32(&launchedD, 1)
				return nil
			}
			started <- c.NodeID
			<-release
			if ctx.Err() != nil {
				atomic.AddInt32(&cancelled, 1)
			}
			atomic.AddInt32(&finished, 1)
			return nil
		})
	}()
	// give the failure time to land, then release the blocked siblings
	time.Sleep(100 * time.Millisecond)
	close(release)
	select {
	case err := <-done:
		if !errors.Is(err, boom) {
			t.Fatalf("err = %v, want boom", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runTopicLinkConfirms did not return")
	}
	if atomic.LoadInt32(&cancelled) != 0 {
		t.Fatal("running sibling ctx was cancelled by the failure")
	}
	if atomic.LoadInt32(&finished) != 2 {
		t.Fatalf("finished siblings = %d, want 2", atomic.LoadInt32(&finished))
	}
	if atomic.LoadInt32(&launchedD) != 0 {
		t.Fatal("new candidate launched after the first error")
	}
}

// F-2: a capped generate keeps ErrCapped in the chain and stays transient.
func TestConfirmTopicLinkCappedIsCappedAndTransient(t *testing.T) {
	capped := fmt.Errorf("%w: %w: no token", llmgateway.ErrUnreachable, llmgateway.ErrCapped)
	gem := &mockGemini{generateResult: func() (string, error) { return "", capped }}
	_, err := confirmTopicLink(context.Background(), Deps{Gemini: gem},
		topicLinkNode{NodeID: "slack:C:1", Type: "slack", Summary: "s"},
		topicLinkCandidate{topicLinkNode: topicLinkNode{NodeID: "jira:P-1", Type: "jira", Summary: "c"}},
		topicLinkContext{})
	if !errors.Is(err, llmgateway.ErrCapped) || !errors.Is(err, jobs.ErrTransient) {
		t.Fatalf("err = %v, want ErrCapped and ErrTransient", err)
	}
}

// F-3: a permit wait that hit the deadline reads as a timeout, not a cap.
func TestSubjectQueriesLLMErrorMessage(t *testing.T) {
	both := fmt.Errorf("%w: %w: %w: x", llmgateway.ErrUnreachable, llmgateway.ErrCapped, context.DeadlineExceeded)
	if got := subjectQueriesLLMError(both); got != "llm timed out" {
		t.Fatalf("got %q, want llm timed out", got)
	}
	capOnly := fmt.Errorf("%w: %w: x", llmgateway.ErrUnreachable, llmgateway.ErrCapped)
	if got := subjectQueriesLLMError(capOnly); got != "llm hourly cap reached" {
		t.Fatalf("got %q", got)
	}
	if got := subjectQueriesLLMError(errors.New("x")); got != "llm generation failed" {
		t.Fatalf("got %q", got)
	}
}
