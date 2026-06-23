package bfs_test

import (
	"testing"

	"github.com/agent-mem/agent-mem/internal/graph/bfs"
)

func TestFrontier_PopsHighestScore(t *testing.T) {
	f := bfs.NewFrontier(10)
	f.Push(bfs.Candidate{NodeID: "a", Hop: 0, Cost: 0.0, Score: 0.5})
	f.Push(bfs.Candidate{NodeID: "b", Hop: 1, Cost: 1.0, Score: 0.8})
	f.Push(bfs.Candidate{NodeID: "c", Hop: 1, Cost: 1.0, Score: 0.3})
	got := f.Pop()
	if got.NodeID != "b" {
		t.Errorf("got %s want b", got.NodeID)
	}
}

func TestFrontier_RespectsMaxSize(t *testing.T) {
	f := bfs.NewFrontier(2)
	f.Push(bfs.Candidate{NodeID: "a", Score: 0.5})
	f.Push(bfs.Candidate{NodeID: "b", Score: 0.8})
	f.Push(bfs.Candidate{NodeID: "c", Score: 0.3}) // dropped — lowest score
	if f.Len() != 2 {
		t.Errorf("len %d want 2", f.Len())
	}
}

func TestFrontier_DedupsByNodeID(t *testing.T) {
	f := bfs.NewFrontier(10)
	f.Push(bfs.Candidate{NodeID: "a", Score: 0.5})
	f.Push(bfs.Candidate{NodeID: "a", Score: 0.8})
	if f.Len() != 1 {
		t.Errorf("dedup failed; len %d", f.Len())
	}
	got := f.Pop()
	if got.Score != 0.8 {
		t.Errorf("expected higher-score retained; got %v", got.Score)
	}
}
