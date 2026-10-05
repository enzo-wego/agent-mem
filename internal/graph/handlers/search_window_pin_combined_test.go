package handlers

import (
	"net/url"
	"testing"
)

func TestSearchWindowPinCombined(t *testing.T) {
	for _, mode := range []string{"default", "hybrid"} {
		for _, arm := range []string{"semantic", "keyword"} {
			t.Run(mode+"/"+arm, func(t *testing.T) {
				db := winReset(t)
				semantic := arm == "semantic"
				query := "what happened on PAY-1 rollout"
				title := "unrelated"
				if !semantic {
					title = query
				}
				// The owner is unindexed and does not match the sentence's
				// keywords: sentence pinning must hydrate it after filtering.
				whfNode(t, db, "jira:PAY-1", whfOutside, "PAY-1", false)
				// No fixture has epic membership, so only the pin is exempt.
				whfNode(t, db, "ordinary:outside", whfOutside, title, semantic)
				whfNode(t, db, "ordinary:inside", whfInside, title, semantic)
				s := whfSearch(t, db, semantic)
				params := "q=" + url.QueryEscape(query) + "&limit=10"
				if mode == "hybrid" {
					params += "&match=hybrid"
				} else {
					params += "&arms=" + arm
				}

				control := whfRequest(t, s, params)
				whfExpect(t, control, "ordinary:outside", true)
				whfExpect(t, control, "ordinary:inside", true)
				for _, row := range control.Results {
					if row.NodeID == "ordinary:outside" || row.ID == "ordinary:outside" {
						if row.ScoreBreakdown.Ranks[arm] < 1 {
							t.Fatalf("outside control lacks %s arm rank: %v", arm, row.ScoreBreakdown.Ranks)
						}
					}
				}

				filtered := whfRequest(t, s, params+whfBounds)
				whfExpect(t, filtered, "jira:PAY-1", true)
				if len(filtered.Results) == 0 || (filtered.Results[0].NodeID != "jira:PAY-1" && filtered.Results[0].ID != "jira:PAY-1") {
					t.Fatal("out-of-window sentence pin is not rank 1")
				}
				whfExpect(t, filtered, "ordinary:outside", false)
				whfExpect(t, filtered, "ordinary:inside", true)
				whfHard(t, filtered, true)
			})
		}
	}
}
