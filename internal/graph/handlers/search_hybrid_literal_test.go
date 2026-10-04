package handlers_test

import (
	"reflect"
	"testing"

	"github.com/agent-mem/agent-mem/internal/graph/handlers"
)

func TestSearch_HybridSearchesLiteralQ(t *testing.T) {
	pool := testDB(t)
	spNode(t, pool, "jira:PAY-L1", "jira", "L1", "payment may fail on retry", "", "", "{}", 0, false)
	spNode(t, pool, "jira:PAY-L2", "jira", "L2", "yesterday we shipped", "", "", "{}", 0, false)
	h, err := handlers.NewSearch(pool)
	if err != nil {
		t.Fatal(err)
	}

	_, top := shRaw(t, h, "q=payment+may+fail&match=hybrid", "")
	if top["query"] != "payment may fail" || top["window"] != nil {
		t.Errorf("query=%v window=%v, want literal q and no window", top["query"], top["window"])
	}
	if ids := shOrdered(shResults(top)); !reflect.DeepEqual(ids, []string{"jira:PAY-L1"}) {
		t.Errorf("results = %v", ids)
	}

	_, top = shRaw(t, h, "q=yesterday&match=hybrid", "")
	if top["query"] != "yesterday" || top["window"] != nil {
		t.Errorf("query=%v window=%v, want literal word and no window", top["query"], top["window"])
	}
	if ids := shOrdered(shResults(top)); !reflect.DeepEqual(ids, []string{"jira:PAY-L2"}) {
		t.Errorf("results = %v", ids)
	}

	_, top = shRaw(t, h, "q=yesterday&match=hybrid&since=2020-01-01&until=2020-01-31", "")
	if top["window"] == nil || top["query"] != "yesterday" {
		t.Errorf("query=%v window=%v, want explicit window honoured", top["query"], top["window"])
	}
}
