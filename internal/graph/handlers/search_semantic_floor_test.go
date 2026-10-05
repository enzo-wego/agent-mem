package handlers

import (
	"reflect"
	"testing"
)

func TestSemanticFloorBoundaryAndOrder(t *testing.T) {
	hits := []armHit{
		{ID: "below", Score: semanticMinCosine - 0.0001},
		{ID: "boundary", Score: semanticMinCosine},
		{ID: "above", Score: 1},
		{ID: "below-again", Score: semanticMinCosine - 0.02},
	}
	want := []armHit{hits[1], hits[2]}
	if got := aboveSemanticFloor(hits); !reflect.DeepEqual(got, want) {
		t.Fatalf("hits = %v, want %v", got, want)
	}
}

func TestSemanticFloorEmpty(t *testing.T) {
	for _, hits := range [][]armHit{nil, {}} {
		if got := aboveSemanticFloor(hits); len(got) != 0 {
			t.Fatalf("empty input returned %v", got)
		}
	}
}
