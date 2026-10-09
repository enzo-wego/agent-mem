package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// The production seed: ignore staging/noise channels, keep only tax/payment
// problems in task-alerts-production (drop the green-check success heartbeats).
const seedChannelFilters = `{
  "ignore": ["C01T60D80JV", "C0A7D29E5ED", "C0B1BR522F5", "C0AJ3JPRA9L", "C02AD7A21UH", "C029TRHS5HU"],
  "incident_only": {"C08S954G2LX": ["PagerDuty"]},
  "keep_regex": {"CPP5EH3A8": "(?i)pending.?payment|process[- ]?taxes"},
  "drop_regex": {"CPP5EH3A8": "(?i)white_check_mark[\\s\\S]*->\\s*200"}
}`

func TestChannelFiltersContentSkip(t *testing.T) {
	f := compileChannelFilters(seedChannelFilters)

	cases := []struct {
		name     string
		channel  string
		body     string
		wantSkip bool
		outcome  string
	}{
		{"ignored staging channel", "C01T60D80JV", "anything at all", true, "skipped_ignored_channel"},
		{"ignored itops channel", "C0A7D29E5ED", "some AI news", true, "skipped_ignored_channel"},
		{"unfiltered channel passes", "C05RNSE8TBR", "payments-team chatter", false, ""},
		{
			"task-alerts: tax success heartbeat dropped",
			"CPP5EH3A8",
			":white_check_mark: process-taxes triggered 2026-07-23T04:17:02Z [from=2026-07-22 to=2026-07-23] -> 200",
			true, "skipped_off_topic",
		},
		{
			"task-alerts: tax warning kept",
			"CPP5EH3A8",
			":warning: process-taxes hourly cron: VPN tunnel could not be (re)established — run skipped",
			false, "",
		},
		{
			"task-alerts: pending-payment failure kept",
			"CPP5EH3A8",
			":red_circle: Task Failed (v2) (https://scheduler/dags/payments.process-pending-payments/…)",
			false, "",
		},
		{
			"task-alerts: unrelated alert dropped (off-topic)",
			"CPP5EH3A8",
			":red_circle: Task Failed (v2) macherly.shopcash-conversion.import-jumia",
			true, "skipped_off_topic",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			skip, outcome := f.contentSkip(tc.channel, tc.body)
			if skip != tc.wantSkip || outcome != tc.outcome {
				t.Fatalf("contentSkip(%q) = (%v, %q); want (%v, %q)",
					tc.channel, skip, outcome, tc.wantSkip, tc.outcome)
			}
		})
	}
}

func TestChannelFiltersIncidentOnly(t *testing.T) {
	f := compileChannelFilters(seedChannelFilters)
	if authors, ok := f.incidentOnly["C08S954G2LX"]; !ok || len(authors) != 1 || authors[0] != "PagerDuty" {
		t.Fatalf("incident_only[payments-alerts] = %v, %v; want [PagerDuty]", authors, ok)
	}
	if _, ok := f.incidentOnly["CPP5EH3A8"]; ok {
		t.Fatalf("task-alerts should not be incident_only")
	}
}

func TestChannelFiltersBadConfigIsNoOp(t *testing.T) {
	for _, raw := range []string{"", "not json", `{"keep_regex":{"C1":"("}}`} {
		f := compileChannelFilters(raw)
		if skip, _ := f.contentSkip("C1", "anything"); skip {
			t.Fatalf("bad config %q should not skip anything", raw)
		}
	}
}

func seedFilterCache(t *testing.T, raw string) {
	t.Helper()
	cfMu.Lock()
	cfCache = compileChannelFilters(raw)
	cfLoadedAt = time.Now()
	cfMu.Unlock()
	t.Cleanup(invalidateChannelFilters)
}

const dropAuthorsCfg = `{"ignore":["CIGN"],"drop_authors":{"CUV9EAYGY":["B085CSTBTS8"," ","U1","bot:BFULL"],"CIGN":["B1"]},
 "keep_regex":{"CUV9EAYGY":"keepme"}}`

func TestChannelFiltersAuthorDropped(t *testing.T) {
	f := compileChannelFilters(dropAuthorsCfg)
	cases := []struct {
		name   string
		ch     string
		idents []string
		want   bool
	}{
		{"bot id vs ref", "CUV9EAYGY", []string{"bot:B085CSTBTS8"}, true},
		{"full ref entry", "CUV9EAYGY", []string{"bot:BFULL"}, true},
		{"user id vs ref", "CUV9EAYGY", []string{"slack_uid:U1"}, true},
		{"bare id", "CUV9EAYGY", []string{"U1"}, true},
		{"empty ident never matches", "CUV9EAYGY", []string{"", ""}, false},
		{"no idents", "CUV9EAYGY", nil, false},
		{"unlisted author", "CUV9EAYGY", []string{"slack_uid:U2"}, false},
		{"other channel", "COTHER", []string{"bot:B085CSTBTS8"}, false},
		{"partial id no match", "CUV9EAYGY", []string{"bot:XB085CSTBTS8"}, false},
	}
	for _, tc := range cases {
		if got := f.authorDropped(tc.ch, tc.idents...); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
	if len(f.dropAuthors["CUV9EAYGY"]) != 3 {
		t.Errorf("blank entries not stripped: %v", f.dropAuthors["CUV9EAYGY"])
	}
}

func TestChannelFiltersAdaptersParity(t *testing.T) {
	f := compileChannelFilters(dropAuthorsCfg)
	liveDevin := liveAuthorIdents(ingestAuthorRef{Ref: "bot:B085CSTBTS8", DisplayName: "B085CSTBTS8", IsBot: true})
	backDevin := backfillAuthorIdents(slackMessage{BotID: "B085CSTBTS8", User: "U085A93H8UB"})
	if !f.authorDropped("CUV9EAYGY", liveDevin...) || !f.authorDropped("CUV9EAYGY", backDevin...) {
		t.Fatal("devin must be dropped on both paths")
	}
	if !f.authorDropped("CUV9EAYGY", liveAuthorIdents(ingestAuthorRef{Ref: "slack_uid:U1"})...) ||
		!f.authorDropped("CUV9EAYGY", backfillAuthorIdents(slackMessage{User: "U1"})...) {
		t.Fatal("U1 must be dropped on both paths")
	}
	if f.authorDropped("CUV9EAYGY", liveAuthorIdents(ingestAuthorRef{Ref: "slack_uid:U9"})...) ||
		f.authorDropped("CUV9EAYGY", backfillAuthorIdents(slackMessage{User: "U9"})...) {
		t.Fatal("unlisted human must be kept on both paths")
	}
	if got := liveAuthorIdents(ingestAuthorRef{DisplayName: "Devin"}); len(got) != 0 {
		t.Fatalf("display name leaked: %v", got)
	}
	if got := backfillAuthorIdents(slackMessage{}); len(got) != 0 {
		t.Fatalf("empty: %v", got)
	}
}

func TestChannelFiltersPrecedence(t *testing.T) {
	f := compileChannelFilters(dropAuthorsCfg)
	if skip, o := f.contentSkip("CIGN", "x", "bot:B1"); !skip || o != "skipped_ignored_channel" {
		t.Errorf("ignore must win: %v %q", skip, o)
	}
	if skip, o := f.contentSkip("CUV9EAYGY", "keepme please", "bot:B085CSTBTS8"); !skip || o != "skipped_dropped_author" {
		t.Errorf("author beats keep_regex: %v %q", skip, o)
	}
	if skip, _ := f.contentSkip("CUV9EAYGY", "keepme please", "slack_uid:U2"); skip {
		t.Error("unlisted author with matching keep must pass")
	}
}

func TestChannelFiltersBadConfigNoop(t *testing.T) {
	for _, raw := range []string{`{bad`, `{"drop_authors":["x"]}`, `{"drop_authors":{"C1":"B1"}}`, `{"drop_authors":null}`} {
		f := compileChannelFilters(raw)
		if f.authorDropped("C1", "bot:B1") {
			t.Errorf("%s: should be no-op", raw)
		}
	}
}

func TestChannelFiltersStripNames(t *testing.T) {
	out, err := stripNames([]byte(`{"ignore":["C1"],"names":{"C1":"x"},"future":{"a":1}}`))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	_ = json.Unmarshal(out, &m)
	if _, ok := m["names"]; ok {
		t.Error("names kept")
	}
	if _, ok := m["future"]; !ok || m["ignore"] == nil {
		t.Error("unknown keys lost")
	}
	for _, bad := range []string{`null`, `[]`, `"x"`, `1`, `{bad`} {
		if _, err := stripNames([]byte(bad)); err == nil {
			t.Errorf("%s: want error", bad)
		}
	}
}

func TestChannelFiltersChannelFilterIDs(t *testing.T) {
	var m map[string]json.RawMessage
	_ = json.Unmarshal([]byte(`{"ignore":["A","B"],"keep_regex":{"B":"x","C":"y"},"drop_regex":"bad","incident_only":{"D":["p"]},"drop_authors":{"E":["B1"],"A":["x"]}}`), &m)
	got := channelFilterIDs(m)
	want := []string{"A", "B", "C", "D", "E"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestChannelFiltersWithChannelNames(t *testing.T) {
	look := func(ids []string) (map[string]string, error) {
		return map[string]string{"C1": "alpha", "C2": ""}, nil
	}
	var m map[string]json.RawMessage
	_ = json.Unmarshal(withChannelNames(`{"ignore":["C1","C2"],"future":1,"names":{"stale":"x"}}`, look), &m)
	if string(m["names"]) != `{"C1":"alpha"}` {
		t.Errorf("names = %s", m["names"])
	}
	if string(m["future"]) != "1" {
		t.Error("unknown key lost")
	}
	if string(withChannelNames("", look)) != "{}" {
		t.Error("unset must be {}")
	}
	if string(withChannelNames(`[1]`, look)) != `[1]` {
		t.Error("non-object must be unchanged")
	}
	boom := func([]string) (map[string]string, error) { return nil, errors.New("x") }
	m = nil
	_ = json.Unmarshal(withChannelNames(`{"ignore":["C1"],"names":{"s":"x"},"k":2}`, boom), &m)
	if _, ok := m["names"]; ok || string(m["k"]) != "2" {
		t.Errorf("lookup error: %v", m)
	}
}

func TestChannelFiltersBackfillGuard(t *testing.T) {
	seedFilterCache(t, dropAuthorsCfg)
	err := ingestSlackMessage(context.Background(), Deps{Logger: zerolog.Nop()}, "CUV9EAYGY",
		slackMessage{Ts: "1700000000.000001", BotID: "B085CSTBTS8", Text: "hi"})
	if err != nil {
		t.Fatalf("want nil, got %v", err)
	}
}

func TestChannelFiltersLiveHandlerDropsAuthor(t *testing.T) {
	seedFilterCache(t, dropAuthorsCfg)
	h := NewIngestContentHandler(Deps{Logger: zerolog.Nop()})
	body := `{"source":"slack","body":"hello","metadata":{"ts":"1700000000.000001","channel_id":"CUV9EAYGY","author":{"ref":"bot:B085CSTBTS8","is_bot":true,"display_name":"B085CSTBTS8"}}}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/api/graph/ingest/content", strings.NewReader(body)))
	var resp struct{ Outcome string }
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if rec.Code != 200 || resp.Outcome != "skipped_dropped_author" {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
}
