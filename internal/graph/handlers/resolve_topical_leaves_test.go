package handlers_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/agent-mem/agent-mem/internal/graph/handlers"
)

func TestResolveLeaves_Acceptance(t *testing.T) {
	for _, row := range []string{"H1", "H2", "H3", "H4", "H5", "H6", "H7", "H8", "H9", "H10", "H11", "H12", "H13", "H14", "H15", "H16", "H17", "H18", "H19", "H20", "H21", "H22", "H23"} {
		t.Run(row, func(t *testing.T) {
			pool := testDB(t)
			s := "slack:SEED:1"
			seeds := []string{s}
			depth := 2
			want := map[string]int{s: 0}
			var absent []string
			node := func(id, typ string) { seedNode(t, pool, id, typ, id); seedBody(t, pool, id, "short fixture body") }
			edge := func(a, b, k string) { seedEdge(t, pool, a, b, k) }
			node(s, "slack_thread")
			a, x := "jira:A", "gh_pr:X"
			chain := func(kind, id, typ string) {
				node(id, typ)
				node(x, "gh_pr")
				edge(s, id, kind)
				edge(id, x, "REFERENCES")
				want[id] = 1
			}
			popular := func(id, typ string, others int) {
				node(id, typ)
				edge(s, id, "REFERENCES")
				for i := range others {
					n := fmt.Sprintf("slack:OTHER:%03d", i)
					node(n, "slack_thread")
					edge(n, id, "REFERENCES")
				}
			}
			switch row {
			case "H1":
				node("cf:A", "cf")
				node(x, "gh_pr")
				edge(s, "cf:A", "SAME_TOPIC")
				edge("cf:A", x, "SAME_TOPIC")
				want["cf:A"] = 1
				absent = append(absent, x)
			case "H2", "H3":
				k := "REFERENCES"
				if row == "H3" {
					k = "REFERS_TO"
				}
				chain(k, a, "jira")
				want[x] = 2
			case "H4", "H15":
				s = "slack:C9:100.000001"
				reply := "slack:C9:100.000002"
				seeds = []string{s}
				want = map[string]int{s: 0, reply: 1, a: 2}
				node(s, "slack")
				node(reply, "slack")
				node(a, "jira")
				if _, err := pool.Exec(context.Background(), `UPDATE graph.nodes SET scope='slack:C9', metadata=jsonb_build_object('thread_ts','100.000001') WHERE id=ANY($1)`, []string{s, reply}); err != nil {
					t.Fatal(err)
				}
				edge(reply, a, "REFERENCES")
				if row == "H15" {
					edge(s, reply, "SAME_TOPIC")
				}
			case "H5":
				chain("PART_OF", "jira:EPIC", "jira")
				absent = append(absent, x)
			case "H6", "H7":
				id, typ, target := "feature:t", "feature", "slack:C2:2"
				if row == "H7" {
					id, typ, target = "person:p", "person", "slack:C3:3"
				}
				node(id, typ)
				node(target, "slack_thread")
				edge(s, id, "REFERENCES")
				edge(id, target, "REFERENCES")
				want[id] = 1
				absent = append(absent, target)
			case "H8", "H9", "H11":
				others := 11
				if row != "H8" {
					others = 12
				}
				popular(a, "jira", others)
				node(x, "gh_pr")
				edge(a, x, "REFERENCES")
				if row == "H11" {
					seeds = []string{a}
					depth = 1
					want = map[string]int{a: 0, s: 1, x: 1}
					for i := range others {
						want[fmt.Sprintf("slack:OTHER:%03d", i)] = 1
					}
				} else {
					want[a] = 1
					if row == "H8" {
						want[x] = 2
					} else {
						absent = append(absent, x)
						for i := range others {
							absent = append(absent, fmt.Sprintf("slack:OTHER:%03d", i))
						}
					}
				}
			case "H10":
				s = "business:payments"
				node(s, "business")
				node(a, "jira")
				edge(s, a, "REFERENCES")
				seeds = []string{s}
				want = map[string]int{s: 0, a: 1}
			case "H12", "H13":
				chain("SAME_TOPIC", a, "jira")
				node("jira:B", "jira")
				edge(s, "jira:B", "REFERENCES")
				edge("jira:B", a, "REFERENCES")
				want["jira:B"] = 1
				if row == "H13" {
					depth = 3
					want[x] = 2
				} else {
					absent = append(absent, x)
				}
			case "H14":
				depth = 1
				node("slack:C4:4", "slack_thread")
				edge(s, "slack:C4:4", "SAME_TOPIC")
				want["slack:C4:4"] = 1
			case "H16":
				node("jira:M", "jira")
				node(x, "gh_pr")
				edge(s, "jira:M", "REFERENCES")
				edge("jira:M", x, "SAME_TOPIC")
				want["jira:M"] = 1
				want[x] = 2
			case "H17":
				popular("slack:C5:5", "slack_thread", 12)
				node("jira:Y", "jira")
				edge("slack:C5:5", "jira:Y", "REFERENCES")
				want["slack:C5:5"] = 1
				want["jira:Y"] = 2
			case "H18":
				depth = 3
				node(a, "jira")
				node("business:payments", "business")
				node("jira:Z", "jira")
				edge(s, a, "REFERENCES")
				edge(a, "business:payments", "REFERENCES")
				edge("business:payments", "jira:Z", "REFERENCES")
				want[a] = 1
				want["business:payments"] = 2
				absent = append(absent, "jira:Z")
			case "H19", "H22":
				for i := range 201 {
					id := fmt.Sprintf("jira:L%03d", i)
					node(id, "jira")
					edge(s, id, "REFERENCES")
					want[id] = 1
					if row == "H19" {
						target := fmt.Sprintf("gh_pr:wego/l#%03d", i)
						node(target, "gh_pr")
						edge(id, target, "REFERENCES")
						if i < 200 {
							want[target] = 2
						} else {
							absent = append(absent, target)
						}
					}
				}
				if row == "H22" {
					depth = 3
					edge("jira:L000", "jira:L200", "REFERENCES")
					node("gh_pr:wego/x#9", "gh_pr")
					edge("jira:L200", "gh_pr:wego/x#9", "REFERENCES")
					absent = append(absent, "gh_pr:wego/x#9")
				}
			case "H20":
				depth = 1
				seeds = nil
				want = map[string]int{}
				for i := range 201 {
					id := fmt.Sprintf("slack:C6:%03d", i)
					node(id, "slack_thread")
					seeds = append(seeds, id)
					want[id] = 0
				}
			case "H21":
				depth = 1
				for i := range 550 {
					id := fmt.Sprintf("jira:F%03d", i)
					node(id, "jira")
					edge(s, id, "REFERENCES")
					if i < 500 {
						want[id] = 1
					} else {
						absent = append(absent, id)
					}
				}
			case "H23":
				depth = 1000000
				s = "slack:C7:7"
				node(s, "slack_thread")
				seeds = []string{s}
				want = map[string]int{s: 0}
			}
			h, err := handlers.NewResolve(pool)
			if err != nil {
				t.Fatal(err)
			}
			body, err := json.Marshal(map[string]any{"seeds": seeds, "depth": depth, "asker_eeid": 0, "include_bodies": false, "budget_tokens": 100000})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			req := httptest.NewRequest("POST", "/api/graph/resolve", strings.NewReader(string(body))).WithContext(ctx)
			w := httptest.NewRecorder()
			start := time.Now()
			h.ServeHTTP(w, req)
			if row == "H23" && time.Since(start) >= 5*time.Second {
				t.Fatal("request took at least 5 seconds")
			}
			if w.Code != 200 {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			var result struct {
				Artifacts []struct {
					NodeID string `json:"node_id"`
					Hop    int    `json:"hop"`
				} `json:"artifacts"`
				CacheMisses []string `json:"cache_misses"`
				Trace       struct {
					Count int `json:"after_score_threshold"`
				} `json:"graph_trace"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if len(result.CacheMisses) > 0 {
				t.Fatalf("hydration misses: %v", result.CacheMisses)
			}
			got := map[string]int{}
			for _, artifact := range result.Artifacts {
				got[artifact.NodeID] = artifact.Hop
			}
			for id, hop := range want {
				if actual, ok := got[id]; !ok || actual != hop {
					t.Errorf("%s: want present hop %d, got hop %d present %v", id, hop, actual, ok)
				}
			}
			for _, id := range absent {
				if hop, ok := got[id]; ok {
					t.Errorf("%s: want absent, got hop %d", id, hop)
				}
			}
			if row == "H20" || row == "H21" || row == "H23" {
				if len(got) != len(want) {
					t.Errorf("artifact count: got %d want %d (after_score_threshold=%d)", len(got), len(want), result.Trace.Count)
				}
			}
		})
	}
}
