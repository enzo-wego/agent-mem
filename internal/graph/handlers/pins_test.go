package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Validation must reject bad input before touching the DB (h.db is nil here —
// a panic means validation ran too late).
func TestPinsCreateValidation(t *testing.T) {
	h := NewPins(nil)
	cases := []struct {
		name string
		body string
	}{
		{"bad json", `{not json`},
		{"missing thread_ts", `{"channel_id":"C1"}`},
		{"missing channel_id", `{"thread_ts":"100.000001"}`},
		{"whitespace only", `{"channel_id":"  ","thread_ts":"100.000001"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, "/api/graph/pins", strings.NewReader(tc.body))
			h.create(w, r)
			if w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", w.Code)
			}
		})
	}
}

func TestPinsDeleteValidation(t *testing.T) {
	h := NewPins(nil)
	for _, qs := range []string{"", "channel=C1", "thread=100.000001"} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodDelete, "/api/graph/pins?"+qs, nil)
		h.delete(w, r)
		if w.Code != http.StatusBadRequest {
			t.Errorf("qs=%q status = %d, want 400", qs, w.Code)
		}
	}
}

// Round-trip: pin a seeded 2-message thread, list it (expect live latest-msg
// enrichment), unpin, list again (expect empty). Requires DATABASE_URL.
func TestPinsRoundTrip(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)
	ctx := t.Context()

	// Seed a 2-message thread in channel CPIN, thread_ts 100.000001, plus the
	// channel name row the list query joins for channel_name.
	for _, n := range []struct{ id, ts, author, body string }{
		{"slack:CPIN:100.000001", "100.000001", "Ross", "refund stuck on TripleA"},
		{"slack:CPIN:100.000002", "100.000002", "Enzo", "checking the ledger now"},
	} {
		meta := `{"ts":"` + n.ts + `","thread_ts":"100.000001","author":{"display_name":"` + n.author + `"}}`
		if _, err := pool.Exec(ctx, `
INSERT INTO graph.nodes (id, type, natural_key, body, scope, metadata, machine_id)
VALUES ($1,'slack',$1,$2,'slack:CPIN',$3::jsonb,'test')
ON CONFLICT (id) DO NOTHING`, n.id, n.body, meta); err != nil {
			t.Fatalf("seed %s: %v", n.id, err)
		}
	}
	if _, err := pool.Exec(ctx, `
INSERT INTO graph.slack_channels (slack_channel_id, name, machine_id)
VALUES ('CPIN','payments-dev','test')
ON CONFLICT (slack_channel_id) DO UPDATE SET name='payments-dev'`); err != nil {
		t.Fatalf("seed channel: %v", err)
	}

	h := NewPins(pool)

	// Pin it.
	w := httptest.NewRecorder()
	h.create(w, httptest.NewRequest(http.MethodPost, "/api/graph/pins",
		strings.NewReader(`{"channel_id":"CPIN","thread_ts":"100.000001"}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("create status = %d, body %s", w.Code, w.Body.String())
	}

	// List: one enriched row.
	w = httptest.NewRecorder()
	h.list(w, httptest.NewRequest(http.MethodGet, "/api/graph/pins", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("list status = %d, body %s", w.Code, w.Body.String())
	}
	var pins []pinnedThread
	if err := json.Unmarshal(w.Body.Bytes(), &pins); err != nil {
		t.Fatalf("list json: %v", err)
	}
	if len(pins) != 1 {
		t.Fatalf("len(pins) = %d, want 1", len(pins))
	}
	p := pins[0]
	if p.ChannelName != "payments-dev" {
		t.Errorf("channel_name = %q, want payments-dev", p.ChannelName)
	}
	if p.NodeID != "slack:CPIN:100.000001" {
		t.Errorf("node_id = %q", p.NodeID)
	}
	if p.MsgCount != 2 {
		t.Errorf("msg_count = %d, want 2", p.MsgCount)
	}
	if p.LastAuthor != "Enzo" || p.LastBody != "checking the ledger now" {
		t.Errorf("latest = %q / %q, want Enzo / checking the ledger now", p.LastAuthor, p.LastBody)
	}
	if p.LastMs != 100000 { // ts 100.000002 → epoch 100s → 100000 ms
		t.Errorf("last_ms = %d, want 100000", p.LastMs)
	}

	// Re-pin is idempotent.
	w = httptest.NewRecorder()
	h.create(w, httptest.NewRequest(http.MethodPost, "/api/graph/pins",
		strings.NewReader(`{"channel_id":"CPIN","thread_ts":"100.000001"}`)))
	if w.Code != http.StatusOK {
		t.Fatalf("re-pin status = %d", w.Code)
	}

	// Unpin, list is empty.
	w = httptest.NewRecorder()
	h.delete(w, httptest.NewRequest(http.MethodDelete,
		"/api/graph/pins?channel=CPIN&thread=100.000001", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("delete status = %d", w.Code)
	}
	w = httptest.NewRecorder()
	h.list(w, httptest.NewRequest(http.MethodGet, "/api/graph/pins", nil))
	pins = nil
	if err := json.Unmarshal(w.Body.Bytes(), &pins); err != nil {
		t.Fatalf("list json: %v", err)
	}
	if len(pins) != 0 {
		t.Errorf("len(pins) after unpin = %d, want 0", len(pins))
	}
}

// Board grouping: a thread REFERENCES a jira node; the epic map groups it under
// its epic; chatter threads are excluded. Requires DATABASE_URL (scratch DB!).
func TestPinsBoard(t *testing.T) {
	pool := openTestDB(t)
	truncateGraphHandlerTables(t, pool)
	ctx := t.Context()

	seed := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v (%s)", err, q)
		}
	}
	// Thread (2 msgs) in channel CB, thread_ts 200.000001.
	for _, n := range []struct{ id, ts, body string }{
		{"slack:CB:200.000001", "200.000001", "scan card gate broken on olympias"},
		{"slack:CB:200.000002", "200.000002", "fix rolling out"},
	} {
		seed(`INSERT INTO graph.nodes (id, type, natural_key, body, scope, metadata, machine_id)
		      VALUES ($1,'slack',$1,$2,'slack:CB',$3::jsonb,'test') ON CONFLICT (id) DO NOTHING`,
			n.id, n.body, `{"ts":"`+n.ts+`","thread_ts":"200.000001"}`)
	}
	// The jira node the thread references, and the REFERENCES edge.
	seed(`INSERT INTO graph.nodes (id, type, natural_key, title, machine_id)
	      VALUES ('jira:PAY-2227','jira','PAY-2227','olympias capability gate','test') ON CONFLICT (id) DO NOTHING`)
	seed(`INSERT INTO graph.edges (from_node_id, to_node_id, kind, machine_id)
	      VALUES ('slack:CB:200.000001','jira:PAY-2227','REFERENCES','test') ON CONFLICT DO NOTHING`)
	// Epic map row.
	seed(`INSERT INTO graph.jira_epic_map (issue_key, issue_summary, issue_status, epic_key, epic_summary, epic_status, machine_id)
	      VALUES ('PAY-2227','olympias capability gate','In Progress','PAY-2197','Scan Card','To Do','test')
	      ON CONFLICT (issue_key) DO NOTHING`)

	h := NewPins(pool)
	w := httptest.NewRecorder()
	h.board(w, httptest.NewRequest(http.MethodGet, "/api/graph/pins/board", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("board status = %d, body %s", w.Code, w.Body.String())
	}
	var resp struct {
		Groups []boardEpicGroup `json:"groups"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("board json: %v", err)
	}
	if len(resp.Groups) != 1 {
		t.Fatalf("len(groups) = %d, want 1: %s", len(resp.Groups), w.Body.String())
	}
	g := resp.Groups[0]
	if g.EpicKey != "PAY-2197" || g.EpicSummary != "Scan Card" {
		t.Errorf("epic = %q %q", g.EpicKey, g.EpicSummary)
	}
	if len(g.Threads) != 1 || g.Threads[0].ThreadTS != "200.000001" || g.Threads[0].MsgCount != 2 {
		t.Errorf("threads = %+v", g.Threads)
	}
	if len(g.Issues) != 1 || g.Issues[0].Key != "PAY-2227" {
		t.Errorf("issues = %+v", g.Issues)
	}
}
