package handlers_test

import (
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/agent-mem/agent-mem/internal/graph/handlers"
)

func TestSearch_HybridSemanticFloodAtLimit(t *testing.T) {
	const limit = 20
	pool := testDB(t)
	spChannel(t, pool, "CSFL", "semflood-chat")
	author := spPerson(t, pool, "Alice", "USFL1", false)

	near := spUnitVec() // cosine 1 with the query
	far := make([]float32, len(near))
	far[0], far[1] = 0.9, 0.43589 // cosine ~0.9

	want := map[string]bool{}
	for i := 1; i <= 5; i++ {
		ts := fmt.Sprintf("%d.000001", i+1000)
		root := spMsg(t, pool, "CSFL", ts, ts, "other thread", author, "", false)
		spEmbed(t, pool, root, far)
		want[root] = true
	}
	const th = "100.000001"
	floodRoot := spMsg(t, pool, "CSFL", th, th, "flood root", author, "", false)
	spEmbed(t, pool, floodRoot, near)
	want[floodRoot] = true
	// More than 3 x limit replies of one thread, all with the best cosines.
	for r := 1; r <= 3*limit+1; r++ {
		ts := fmt.Sprintf("%d.000001", 200+r)
		id := spMsg(t, pool, "CSFL", ts, th, "flood reply", author, "", false)
		spEmbed(t, pool, id, near)
	}

	h, err := handlers.NewSearchWithEmbedder(pool, fixedSearchEmbedder{vector: near})
	if err != nil {
		t.Fatal(err)
	}
	code, top := shRaw(t, h, fmt.Sprintf("q=%s&match=hybrid&limit=%d", "zzqqnomatch", limit), "")
	if code != http.StatusOK {
		t.Fatalf("status %d: %v", code, top)
	}
	got := map[string]bool{}
	for _, r := range shResults(top) {
		got[r["id"].(string)] = true
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("threads = %v, want %v", keys(got), keys(want))
	}
}

func keys(m map[string]bool) string {
	var s []string
	for k := range m {
		s = append(s, k)
	}
	return strings.Join(s, ",")
}
