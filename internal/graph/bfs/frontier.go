// Package bfs implements cost-bounded priority-queue BFS over graph.edges.
package bfs

import (
	"container/heap"
)

// Candidate is one node under consideration during traversal.
type Candidate struct {
	NodeID  string
	Hop     int     // BFS depth from the nearest seed
	Cost    float64 // accumulated cost from seed (1 - edge_score per hop)
	Score   float64 // current best score (overall, not per-component)
	ViaEdge string  // last edge kind that reached this node
}

// Frontier is a max-heap of candidates with per-NodeID dedup that keeps
// the highest-score entry.
type Frontier struct {
	h       *candHeap
	index   map[string]int // NodeID → position in heap
	maxSize int
}

// NewFrontier creates a new Frontier with a maximum size.
func NewFrontier(maxSize int) *Frontier {
	h := &candHeap{}
	heap.Init(h)
	return &Frontier{
		h:       h,
		index:   make(map[string]int),
		maxSize: maxSize,
	}
}

// Push adds or updates a candidate. If the NodeID is already present
// and the new score is higher, the existing entry is updated.
func (f *Frontier) Push(c Candidate) {
	if pos, ok := f.index[c.NodeID]; ok {
		if c.Score > (*f.h)[pos].Score {
			(*f.h)[pos] = c
			heap.Fix(f.h, pos)
			// Rebuild the index after Fix since positions shift.
			f.rebuildIndex()
		}
		return
	}
	if f.maxSize > 0 && len(*f.h) >= f.maxSize {
		// Drop the lowest-score entry to make room (if new entry beats it).
		lowest, lowIdx := f.findLowest()
		if c.Score <= lowest.Score {
			return
		}
		delete(f.index, lowest.NodeID)
		heap.Remove(f.h, lowIdx)
		f.rebuildIndex()
	}
	heap.Push(f.h, c)
	f.rebuildIndex()
}

// Pop removes and returns the highest-score candidate.
func (f *Frontier) Pop() Candidate {
	c := heap.Pop(f.h).(Candidate)
	delete(f.index, c.NodeID)
	f.rebuildIndex()
	return c
}

// Len returns the number of candidates in the frontier.
func (f *Frontier) Len() int { return len(*f.h) }

func (f *Frontier) rebuildIndex() {
	for k := range f.index {
		delete(f.index, k)
	}
	for i, c := range *f.h {
		f.index[c.NodeID] = i
	}
}

func (f *Frontier) findLowest() (Candidate, int) {
	low := (*f.h)[0]
	lowIdx := 0
	for i, c := range *f.h {
		if c.Score < low.Score {
			low, lowIdx = c, i
		}
	}
	return low, lowIdx
}

// candHeap is a max-heap by Score.
type candHeap []Candidate

func (h candHeap) Len() int           { return len(h) }
func (h candHeap) Less(i, j int) bool { return h[i].Score > h[j].Score }
func (h candHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *candHeap) Push(x any)        { *h = append(*h, x.(Candidate)) }
func (h *candHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}
