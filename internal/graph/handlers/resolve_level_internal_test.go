package handlers

import (
	"reflect"
	"slices"
	"testing"

	"github.com/agent-mem/agent-mem/internal/graph/bfs"
)

func TestResolveLevel_NextStructural(t *testing.T) {
	topic := bfs.Neighbor{NodeID: "A", EdgeKind: "SAME_TOPIC"}
	ref := bfs.Neighbor{NodeID: "A", EdgeKind: "REFERENCES"}
	for _, tc := range []struct {
		name      string
		neighbors []bfs.Neighbor
		siblings  map[string]map[string]bool
		decided   map[string]bool
		want      []string
	}{
		{name: "topic then structural", neighbors: []bfs.Neighbor{topic, ref}, want: []string{"A"}},
		{name: "structural then topic", neighbors: []bfs.Neighbor{ref, topic}, want: []string{"A"}},
		{name: "topic leaf", neighbors: []bfs.Neighbor{topic}},
		{name: "thread sibling", neighbors: []bfs.Neighbor{topic}, siblings: map[string]map[string]bool{"S": {"A": true}}, want: []string{"A"}},
		{name: "already decided", neighbors: []bfs.Neighbor{ref}, decided: map[string]bool{"A": true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := nextStructural([]string{"S"}, map[string][]bfs.Neighbor{"S": tc.neighbors}, tc.siblings, tc.decided)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestResolveLevel_DisplayHops(t *testing.T) {
	for _, expanded := range []bool{false, true} {
		t.Run(map[bool]string{false: "A leaf", true: "A expanded"}[expanded], func(t *testing.T) {
			nbrs := map[string][]bfs.Neighbor{
				"S": {{NodeID: "A", EdgeKind: "SAME_TOPIC"}, {NodeID: "B", EdgeKind: "REFERENCES"}},
				"B": {{NodeID: "A", EdgeKind: "REFERENCES"}},
			}
			if expanded {
				nbrs["A"] = []bfs.Neighbor{{NodeID: "X", EdgeKind: "REFERENCES"}}
			}
			want := map[string]bfs.Candidate{"S": {NodeID: "S", Hop: 0, Score: 1}, "T": {NodeID: "T", Hop: 0, Score: 1}, "A": {NodeID: "A", Hop: 1, Score: 0.5}, "B": {NodeID: "B", Hop: 1, Score: 0.5}}
			if expanded {
				want["X"] = bfs.Candidate{NodeID: "X", Hop: 2, Score: 0.25}
			}
			for _, reverseSeeds := range []bool{false, true} {
				for _, reverseEdges := range []bool{false, true} {
					seeds := []string{"S", "T"}
					if reverseSeeds {
						slices.Reverse(seeds)
					}
					ordered := make(map[string][]bfs.Neighbor, len(nbrs))
					for id, ns := range nbrs {
						ordered[id] = slices.Clone(ns)
						if reverseEdges {
							slices.Reverse(ordered[id])
						}
					}
					got := displayHops(seeds, ordered, 3)
					for id, c := range got {
						c.ViaEdge = ""
						got[id] = c
					}
					if !reflect.DeepEqual(got, want) {
						t.Fatalf("reverseSeeds=%v reverseEdges=%v: got %#v want %#v", reverseSeeds, reverseEdges, got, want)
					}
				}
			}
		})
	}
}
