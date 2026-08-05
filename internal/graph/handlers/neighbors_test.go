package handlers_test

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/agent-mem/agent-mem/internal/graph/handlers"
)

func TestNeighbors_Depth1(t *testing.T) {
	pool := testDB(t)
	seedNode(t, pool, "a", "slack_thread", "A")
	seedNode(t, pool, "b", "jira", "B")
	seedNode(t, pool, "c", "gh_pr", "C")
	seedEdge(t, pool, "a", "b", "REFERENCES")
	seedEdge(t, pool, "a", "c", "REFERENCES")

	r := chi.NewRouter()
	r.Mount("/api/graph", handlers.NewNeighbors(pool))

	req := httptest.NewRequest("GET", "/api/graph/node/a/neighbors?depth=1", nil)
	req.Header.Set("X-Asker-User", "U07UAC0J7T3")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	var resp struct {
		Neighbors []struct {
			Node struct {
				NodeID string `json:"node_id"`
			} `json:"node"`
			Edge struct {
				Kind string `json:"kind"`
			} `json:"edge"`
			Hop int `json:"hop"`
		} `json:"neighbors"`
	}
	json.NewDecoder(w.Body).Decode(&resp)
	if len(resp.Neighbors) != 2 {
		t.Errorf("want 2 neighbours, got %d", len(resp.Neighbors))
	}
}

// A referenced node we never fetched (no title, no url) is an un-enriched stub —
// e.g. "RFC-53" mis-typed as a Jira key, or a "feature:" entity. It must not
// surface in the panel as a raw, un-openable id row.
func TestNeighbors_DropsUnenrichedStubs(t *testing.T) {
	pool := testDB(t)
	seedNode(t, pool, "root", "slack_thread", "Root")
	seedNode(t, pool, "real", "jira", "Real ticket")
	seedNodeURL(t, pool, "real", "https://wegomushi.atlassian.net/browse/PAY-1")
	seedNode(t, pool, "jira:RFC-53", "jira", "") // stub: no title, no url
	seedEdge(t, pool, "root", "real", "REFERENCES")
	seedEdge(t, pool, "root", "jira:RFC-53", "REFERENCES")

	r := chi.NewRouter()
	r.Mount("/api/graph", handlers.NewNeighbors(pool))

	req := httptest.NewRequest("GET", "/api/graph/node/root/neighbors?depth=1", nil)
	req.Header.Set("X-Asker-User", "U07UAC0J7T3")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != 200 {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	var resp struct {
		Neighbors []struct {
			Node struct {
				NodeID string `json:"node_id"`
			} `json:"node"`
		} `json:"neighbors"`
	}
	json.NewDecoder(w.Body).Decode(&resp)
	if len(resp.Neighbors) != 1 {
		t.Fatalf("want 1 neighbour (stub dropped), got %d", len(resp.Neighbors))
	}
	if resp.Neighbors[0].Node.NodeID != "real" {
		t.Errorf("want enriched node 'real', got %q", resp.Neighbors[0].Node.NodeID)
	}
}

// neighborRow mirrors the fields the file-leaf pass populates.
type neighborRow struct {
	Node struct {
		NodeID string `json:"node_id"`
		Type   string `json:"type"`
		URL    string `json:"url"`
		Via    string `json:"via"`
	} `json:"node"`
	Edge struct {
		Kind string `json:"kind"`
	} `json:"edge"`
	Hop int `json:"hop"`
}

func decodeNeighbors(t *testing.T, w *httptest.ResponseRecorder) []neighborRow {
	t.Helper()
	if w.Code != 200 {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	var resp struct {
		Neighbors []neighborRow `json:"neighbors"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp.Neighbors
}

// A file the neighbour thread posted hangs off it at hop 2 (thread is hop 1),
// so a raw depth=1 BFS never returns it. The attachment-leaf pass must pull it
// in as a leaf with the neighbour's title as its Via.
func TestNeighbors_FileLeafFromNeighborThread(t *testing.T) {
	pool := testDB(t)
	seedNode(t, pool, "slack:C1:1.1", "slack", "Root thread")
	seedNode(t, pool, "slack:C2:2.2", "slack", "Saudi Rail taxation thread")
	seedNode(t, pool, "slack_file:F1", "slack_file", "Saudi Rail - Tax Analysis")
	seedNodeURL(t, pool, "slack_file:F1", "https://docs.google.com/spreadsheets/d/abc")
	seedEdge(t, pool, "slack:C1:1.1", "slack:C2:2.2", "REFERENCES")
	seedEdge(t, pool, "slack:C2:2.2", "slack_file:F1", "REFERENCES")

	r := chi.NewRouter()
	r.Mount("/api/graph", handlers.NewNeighbors(pool))
	req := httptest.NewRequest("GET", "/api/graph/node/slack:C1:1.1/neighbors?depth=1", nil)
	req.Header.Set("X-Asker-User", "U07UAC0J7T3")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	rows := decodeNeighbors(t, w)
	var file *neighborRow
	for i := range rows {
		if rows[i].Node.Type == "slack_file" {
			file = &rows[i]
		}
	}
	if file == nil {
		t.Fatalf("want a slack_file leaf at depth 1, got %+v", rows)
	}
	if file.Node.NodeID != "slack_file:F1" {
		t.Errorf("file node_id = %q, want slack_file:F1", file.Node.NodeID)
	}
	if file.Node.URL == "" {
		t.Errorf("file url is empty, want the Google-Sheet link")
	}
	if file.Node.Via != "Saudi Rail taxation thread" {
		t.Errorf("file via = %q, want the neighbour thread title", file.Node.Via)
	}
	if file.Hop != 2 {
		t.Errorf("file hop = %d, want 2 (neighbour hop 1 + 1)", file.Hop)
	}
}

// The same fixture with an explicit edge-kind filter stays literal: the pass is
// skipped, so the neighbour (reachable via REFERENCES) surfaces but its file
// does not.
func TestNeighbors_FileLeafSkippedWhenKindFiltered(t *testing.T) {
	pool := testDB(t)
	seedNode(t, pool, "slack:C1:1.1", "slack", "Root thread")
	seedNode(t, pool, "slack:C2:2.2", "slack", "Saudi Rail taxation thread")
	seedNode(t, pool, "slack_file:F1", "slack_file", "Saudi Rail - Tax Analysis")
	seedNodeURL(t, pool, "slack_file:F1", "https://docs.google.com/spreadsheets/d/abc")
	seedEdge(t, pool, "slack:C1:1.1", "slack:C2:2.2", "REFERENCES")
	seedEdge(t, pool, "slack:C2:2.2", "slack_file:F1", "REFERENCES")

	r := chi.NewRouter()
	r.Mount("/api/graph", handlers.NewNeighbors(pool))
	req := httptest.NewRequest("GET", "/api/graph/node/slack:C1:1.1/neighbors?depth=1&kind=REFERENCES", nil)
	req.Header.Set("X-Asker-User", "U07UAC0J7T3")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	rows := decodeNeighbors(t, w)
	files, sawNeighbor := 0, false
	for _, n := range rows {
		if n.Node.Type == "slack_file" {
			files++
		}
		if n.Node.NodeID == "slack:C2:2.2" {
			sawNeighbor = true
		}
	}
	if files != 0 {
		t.Errorf("kind-filtered query returned %d file rows, want 0", files)
	}
	if !sawNeighbor {
		t.Errorf("kind-filtered query dropped the REFERENCES neighbour; got %+v", rows)
	}
}

// A thread with a photo dump must not flood the payload: the pass caps at 20
// file rows even when 25 files hang off a surfaced neighbour.
func TestNeighbors_FileLeafCappedAt20(t *testing.T) {
	pool := testDB(t)
	seedNode(t, pool, "slack:C1:1.1", "slack", "Root thread")
	seedNode(t, pool, "slack:C2:2.2", "slack", "Photo dump thread")
	seedEdge(t, pool, "slack:C1:1.1", "slack:C2:2.2", "REFERENCES")
	for i := range 25 {
		fid := fmt.Sprintf("slack_file:F%02d", i)
		seedNode(t, pool, fid, "slack_file", fmt.Sprintf("photo %02d", i))
		seedNodeURL(t, pool, fid, fmt.Sprintf("https://files.slack.com/%02d.png", i))
		seedEdge(t, pool, "slack:C2:2.2", fid, "REFERENCES")
	}

	r := chi.NewRouter()
	r.Mount("/api/graph", handlers.NewNeighbors(pool))
	req := httptest.NewRequest("GET", "/api/graph/node/slack:C1:1.1/neighbors?depth=1", nil)
	req.Header.Set("X-Asker-User", "U07UAC0J7T3")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	rows := decodeNeighbors(t, w)
	files := 0
	for _, n := range rows {
		if n.Node.Type == "slack_file" {
			files++
		}
	}
	if files != 20 {
		t.Errorf("want exactly 20 file rows (cap), got %d", files)
	}
}
