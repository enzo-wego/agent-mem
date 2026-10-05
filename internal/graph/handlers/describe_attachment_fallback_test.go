package handlers

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

const downloadUserToken = "xoxc-FAKE-LEAK-CANARY"
const downloadUserCookie = "xoxd-FAKE-LEAK-CANARY"

type downloadTestTransport struct {
	base   http.RoundTripper
	target *url.URL
}

func (tr downloadTestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	u := *req.URL
	u.Scheme, u.Host = tr.target.Scheme, tr.target.Host
	clone.URL = &u
	clone.Host = req.URL.Host
	return tr.base.RoundTrip(clone)
}

func installDownloadServer(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	old := http.DefaultTransport
	http.DefaultTransport = downloadTestTransport{base: server.Client().Transport, target: target}
	t.Cleanup(func() { http.DefaultTransport = old })
}

func downloadTestDeps(logs *bytes.Buffer) Deps {
	return Deps{
		SlackBotToken:  "bot",
		Logger:         zerolog.New(logs),
		SlackUserCreds: func() (string, string) { return downloadUserToken, downloadUserCookie },
	}
}

func assertDownloadNoLeaks(t *testing.T, err error, logs string) {
	t.Helper()
	text := logs
	if err != nil {
		text += err.Error()
	}
	for _, secret := range []string{downloadUserToken, downloadUserCookie} {
		if strings.Contains(text, secret) {
			t.Error("credential leaked in logs or error")
		}
	}
}

func TestDownloadSlackBot403UserSucceeds(t *testing.T) {
	testDownloadRedFallback(t, http.StatusForbidden, "text/plain")
}

func TestDownloadSlackBotHTMLUserSucceeds(t *testing.T) {
	testDownloadRedFallback(t, http.StatusOK, "text/html; charset=utf-8")
}

func testDownloadRedFallback(t *testing.T, botStatus int, botType string) {
	t.Helper()
	requests := 0
	installDownloadServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			if r.Header.Get("Authorization") != "Bearer bot" {
				t.Error("missing bot authorization")
			}
			w.Header().Set("Content-Type", botType)
			w.WriteHeader(botStatus)
			io.WriteString(w, "login")
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+downloadUserToken || r.Header.Get("Cookie") != "d="+downloadUserCookie {
			t.Error("missing user credentials")
		}
		w.Header().Set("Content-Type", "image/png")
		io.WriteString(w, "image bytes")
	})
	var logs bytes.Buffer
	deps := downloadTestDeps(&logs)
	data, err := downloadWithAuth(context.Background(), "https://files.slack.com/files-pri/test.png", "slack", "image/png", deps)
	assertDownloadNoLeaks(t, err, logs.String())
	if err != nil {
		t.Fatalf("fallback download failed: %v", err)
	}
	if string(data) != "image bytes" {
		t.Fatalf("download = %q, want user image bytes", data)
	}
	if requests != 2 {
		t.Fatalf("requests = %d, want 2", requests)
	}
}
