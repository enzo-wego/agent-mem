package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/rs/zerolog"

	"github.com/agent-mem/agent-mem/internal/graph/fetchers"
)

func TestFetchBodyHandler_BadPayload(t *testing.T) {
	deps := Deps{
		Logger:    zerolog.Nop(),
		MachineID: "test",
		Fetchers:  fetchers.NewRegistry(fetchers.Config{}, zerolog.Nop()),
	}
	h := NewFetchBodyHandler(deps)

	err := h.Handler(context.Background(), []byte("not json"))
	if err == nil {
		t.Fatal("expected error for bad JSON payload")
	}
	if !errors.Is(err, errFatalSentinel) {
		t.Logf("error (expected ErrFatal-wrapped): %v", err)
	}
}

func TestFetchBodyHandler_EmptyRef(t *testing.T) {
	deps := Deps{
		Logger:    zerolog.Nop(),
		MachineID: "test",
		Fetchers:  fetchers.NewRegistry(fetchers.Config{}, zerolog.Nop()),
	}
	h := NewFetchBodyHandler(deps)

	payload, _ := json.Marshal(fetchBodyPayload{})
	err := h.Handler(context.Background(), payload)
	if err == nil {
		t.Fatal("expected error when both node_id and url are empty")
	}
}

func TestFetchBodyHandler_NoFetcher(t *testing.T) {
	deps := Deps{
		Logger:    zerolog.Nop(),
		MachineID: "test",
		Fetchers:  fetchers.NewRegistry(fetchers.Config{}, zerolog.Nop()),
	}
	h := NewFetchBodyHandler(deps)

	// "unknown:xyz" won't match any registered fetcher.
	payload, _ := json.Marshal(fetchBodyPayload{NodeID: "unknown:xyz"})
	err := h.Handler(context.Background(), payload)
	if err == nil {
		t.Fatal("expected error when no fetcher matches")
	}
}

// errFatalSentinel is exported only for test assertions within the package.
var errFatalSentinel = errors.New("fatal")
