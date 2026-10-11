package fetchers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/agent-mem/agent-mem/internal/graph/extractor"
)

func TestSlackFetcher_Matches(t *testing.T) {
	f := &slackFetcher{}
	cases := []struct {
		input string
		want  bool
	}{
		{"slack:C08S954G2LX:1779710863.216389", true},
		{"https://wego.slack.com/archives/C08S954G2LX/p1779710863216389", true},
		{"jira:PAY-123", false},
		{"gh_pr:wego/payments#1", false},
		{"https://app.datadoghq.com/monitors/123", false},
		{"slack:INVALID", false},
	}
	for _, tc := range cases {
		got := f.Matches(tc.input)
		if got != tc.want {
			t.Errorf("Matches(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

func TestSlackFetcher_HappyPath_Thread(t *testing.T) {
	payload := slackAPIResponse{
		OK: true,
		Messages: []slackMessage{
			{User: "U123", Text: "Hello thread", Ts: "1779710863.216389"},
			{User: "U456", Text: "Reply here", Ts: "1779710864.000001"},
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Expect bearer auth.
		if r.Header.Get("Authorization") != "Bearer test-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(payload)
	}))
	defer srv.Close()

	cfg := Config{
		SlackBotToken: "test-token",
		HTTPClient:    srv.Client(),
	}
	// Point API calls to test server by overriding the URL in the fetcher's doGet.
	// We do this by using a transport that rewrites the host.
	cfg.HTTPClient = newRewriteClient(srv.URL, srv.Client())

	f := newSlackFetcher(cfg, noLogger())
	body, err := f.Fetch(context.Background(), "slack:C08S954G2LX:1779710863.216389")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if body.Author.ExternalID != "U123" {
		t.Errorf("author = %q, want U123", body.Author.ExternalID)
	}
	if body.NodeID != "slack:C08S954G2LX:1779710863.216389" {
		t.Errorf("nodeID = %q", body.NodeID)
	}
	if body.Type != "slack" {
		t.Errorf("type = %q", body.Type)
	}
	// Raw should contain parent text + reply separator.
	raw := string(body.Raw)
	if raw == "" {
		t.Error("raw body is empty")
	}
	if body.Title == "" {
		t.Error("title is empty")
	}
}

func TestSlackFetcher_SharedMessage(t *testing.T) {
	// A "FYI @x" share: the real content (forwarded text + PDF) lives in
	// attachments, not the top-level Text.
	payload := slackAPIResponse{
		OK: true,
		Messages: []slackMessage{{
			User: "U123", Text: "FYI @Surbhi Babbar", Ts: "1779710863.216389",
			Attachments: []slackAttachment{{
				AuthorName: "mohan",
				Text:       "Hi attached flow of Pay at Hotel journey.",
				Files:      []slackFile{{ID: "F999", Name: "Booking.com-Certification.pdf", Mimetype: "application/pdf"}},
			}},
		}},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(payload)
	}))
	defer srv.Close()

	cfg := Config{SlackBotToken: "test-token", HTTPClient: newRewriteClient(srv.URL, srv.Client())}
	f := newSlackFetcher(cfg, noLogger())
	body, err := f.Fetch(context.Background(), "slack:C08S954G2LX:1779710863.216389")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	raw := string(body.Raw)
	if !strings.Contains(raw, "mohan") || !strings.Contains(raw, "Pay at Hotel journey") {
		t.Errorf("shared content not folded into body: %q", raw)
	}
	if len(body.Attachments) != 1 || body.Attachments[0].Filename != "Booking.com-Certification.pdf" {
		t.Errorf("shared file not collected: %+v", body.Attachments)
	}
}

func TestSlackFetcher_404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	cfg := Config{
		SlackBotToken: "test-token",
		HTTPClient:    newRewriteClient(srv.URL, srv.Client()),
	}
	f := newSlackFetcher(cfg, noLogger())
	_, err := f.Fetch(context.Background(), "slack:C08S954G2LX:1779710863.216389")
	if err == nil {
		t.Fatal("expected error for 404, got nil")
	}
}

func TestSlackFetcher_5xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	cfg := Config{
		SlackBotToken: "test-token",
		HTTPClient:    newRewriteClient(srv.URL, srv.Client()),
	}
	f := newSlackFetcher(cfg, noLogger())
	_, err := f.Fetch(context.Background(), "slack:C08S954G2LX:1779710863.216389")
	if err == nil {
		t.Fatal("expected error for 5xx, got nil")
	}
}

func TestSlackFetcher_AuthHeader(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(slackAPIResponse{
			OK:       true,
			Messages: []slackMessage{{User: "U1", Text: "hi", Ts: "1779710863.000001"}},
		})
	}))
	defer srv.Close()

	cfg := Config{
		SlackBotToken: "xoxb-secret",
		HTTPClient:    newRewriteClient(srv.URL, srv.Client()),
	}
	f := newSlackFetcher(cfg, noLogger())
	f.Fetch(context.Background(), "slack:C08S:1779710863.000001") //nolint:errcheck
	if gotAuth != "Bearer xoxb-secret" {
		t.Errorf("auth header = %q, want %q", gotAuth, "Bearer xoxb-secret")
	}
}

func fetchSlackBodyRaw(t *testing.T, payload slackAPIResponse) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(payload)
	}))
	defer srv.Close()
	cfg := Config{SlackBotToken: "test-token", HTTPClient: newRewriteClient(srv.URL, srv.Client())}
	f := newSlackFetcher(cfg, noLogger())
	body, err := f.Fetch(context.Background(), "slack:C08S954G2LX:1779710863.216389")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	return string(body.Raw)
}

func TestSlackFetcher_NonShareBodyExact(t *testing.T) {
	raw := fetchSlackBodyRaw(t, slackAPIResponse{
		OK: true,
		Messages: []slackMessage{{
			User: "U123", Text: "see link", Ts: "1779710863.216389",
			Attachments: []slackAttachment{
				{AuthorName: "bob", Title: "A title", Text: "unfurl text"},
				{Fallback: "fallback only"},
			},
		}},
	})
	want := "see link\n\n--- shared from bob ---\nA title\nunfurl text\n\n--- shared ---\nfallback only"
	if raw != want {
		t.Errorf("body mismatch:\n got %q\nwant %q", raw, want)
	}
}

func TestSlackShareURL(t *testing.T) {
	cases := []struct{ name, from, orig, ch, ts, want string }{
		{"from_url", "https://x/from", "https://x/orig", "C1", "1.2", "https://x/from"},
		{"original_url", "", "https://x/orig", "C1", "1.2", "https://x/orig"},
		{"channel_ts", "", "", "C019B36KGNR", "1791548199.239769", "https://wego.slack.com/archives/C019B36KGNR/p1791548199239769"},
		{"none", "", "", "C1", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := SlackShareURL(c.from, c.orig, c.ch, c.ts); got != c.want {
				t.Errorf("got %q want %q", got, c.want)
			}
		})
	}
}

func TestSlackFetcher_ShareURL(t *testing.T) {
	const fromURL = "https://wego.slack.com/archives/C019B36KGNR/p1791548199239769?thread_ts=1779440396.626379&cid=C019B36KGNR"
	raw := fetchSlackBodyRaw(t, slackAPIResponse{
		OK: true,
		Messages: []slackMessage{{
			User: "U123", Text: "", Ts: "1779710863.216389",
			Attachments: []slackAttachment{{
				IsShare: true, IsMsgUnfurl: true, ChannelID: "C019B36KGNR", Ts: "1791548199.239769",
				AuthorName: "someone", FromURL: fromURL, Text: "forwarded body",
			}},
		}},
	})
	if !strings.Contains(raw, fromURL) {
		t.Fatalf("from_url missing from body: %q", raw)
	}
	res, err := extractor.New(nil, zerolog.Nop()).Extract(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"slack:C019B36KGNR:1791548199.239769", "slack:C019B36KGNR:1779440396.626379"} {
		found := false
		for _, f := range res.Findings {
			if f.NodeID == want {
				found = true
			}
		}
		if !found {
			t.Errorf("no finding for %s: %+v", want, res.Findings)
		}
	}

	// URL-only attachment is not skipped.
	raw = fetchSlackBodyRaw(t, slackAPIResponse{
		OK: true,
		Messages: []slackMessage{{
			User: "U123", Text: "x", Ts: "1779710863.216389",
			Attachments: []slackAttachment{{IsMsgUnfurl: true, FromURL: fromURL}},
		}},
	})
	if want := "x\n\n--- shared ---\n" + fromURL + "\n"; raw != want {
		t.Errorf("url-only body: got %q want %q", raw, want)
	}
}
