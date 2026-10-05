package handlers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/agent-mem/agent-mem/internal/graph/jobs"
)

func TestDownloadSlackFallbackStatuses(t *testing.T) {
	cases := []struct {
		name                        string
		bot, user                   int
		botType, userType, declared string
		noToken, noCookie           bool
		requests                    int
		fatal, fail                 bool
		message                     string
	}{
		{name: "real bytes skip fallback", bot: 200, botType: "image/png", requests: 1},
		{name: "real HTML skip fallback", bot: 200, botType: "text/html; charset=utf-8", declared: "text/html; charset=utf-8", requests: 1},
		{name: "bot401 user succeeds", bot: 401, user: 200, requests: 2},
		{name: "no token preserves bot403", bot: 403, noToken: true, requests: 1, fatal: true, fail: true, message: "fatal: download HTTP 403: "},
		{name: "no token bot login fatal", bot: 200, botType: "text/html; charset=utf-8", noToken: true, requests: 1, fatal: true, fail: true, message: "fatal: download: slack login page (bot): "},
		{name: "both403", bot: 403, user: 403, requests: 2, fatal: true, fail: true, message: "fatal: download HTTP 403 (bot), 403 (user): "},
		{name: "bot401 user403 preserves status", bot: 401, user: 403, requests: 2, fatal: true, fail: true, message: "fatal: download HTTP 401 (bot), 403 (user): "},
		{name: "user404", bot: 403, user: 404, requests: 2, fatal: true, fail: true, message: "fatal: download HTTP 403 (bot), 404 (user): "},
		{name: "user503 transient", bot: 403, user: 503, requests: 2, fail: true, message: "download HTTP 403 (bot), 503 (user): "},
		{name: "user429 transient", bot: 403, user: 429, requests: 2, fail: true, message: "download HTTP 403 (bot), 429 (user): "},
		{name: "bot503 no fallback", bot: 503, requests: 1, fail: true, message: "download HTTP 503: "},
		{name: "bot429 no fallback", bot: 429, requests: 1, fail: true, message: "download HTTP 429: "},
		{name: "bot404 no fallback", bot: 404, requests: 1, fatal: true, fail: true, message: "fatal: download HTTP 404: "},
		{name: "bearer only", bot: 403, user: 200, noCookie: true, requests: 2},
		{name: "bot403 user login", bot: 403, user: 200, userType: "text/html; charset=utf-8", requests: 2, fatal: true, fail: true, message: "fatal: download: slack login page (bot 403, user login): "},
		{name: "bot401 user login", bot: 401, user: 200, userType: "text/html", requests: 2, fatal: true, fail: true, message: "fatal: download: slack login page (bot 401, user login): "},
		{name: "both login", bot: 200, botType: "text/html; charset=utf-8", user: 200, userType: "text/html", requests: 2, fatal: true, fail: true, message: "fatal: download: slack login page (bot login, user login): "},
		{name: "no token redirect", bot: 302, noToken: true, requests: 1, fatal: true, fail: true, message: "fatal: download: slack redirect (bot): "},
		{name: "redirect user403", bot: 302, user: 403, requests: 2, fatal: true, fail: true, message: "fatal: download HTTP redirect (bot), 403 (user): "},
		{name: "both redirect", bot: 302, user: 302, requests: 2, fatal: true, fail: true, message: "fatal: download HTTP redirect (bot), redirect (user): "},
		{name: "401 user redirect", bot: 401, user: 302, requests: 2, fatal: true, fail: true, message: "fatal: download HTTP 401 (bot), redirect (user): "},
		{name: "403 user redirect", bot: 403, user: 302, requests: 2, fatal: true, fail: true, message: "fatal: download HTTP 403 (bot), redirect (user): "},
		{name: "login user redirect", bot: 200, botType: "text/html", user: 302, requests: 2, fatal: true, fail: true, message: "fatal: download HTTP login (bot), redirect (user): "},
		{name: "redirect user login", bot: 302, user: 200, userType: "text/html", requests: 2, fatal: true, fail: true, message: "fatal: download: slack login page (bot redirect, user login): "},
		{name: "redirect user429", bot: 302, user: 429, requests: 2, fail: true, message: "download HTTP redirect (bot), 429 (user): "},
		{name: "redirect user503", bot: 302, user: 503, requests: 2, fail: true, message: "download HTTP redirect (bot), 503 (user): "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			requests, reads := 0, 0
			installDownloadServer(t, func(w http.ResponseWriter, r *http.Request) {
				requests++
				status, contentType := tc.bot, tc.botType
				if requests == 1 {
					if r.Header.Get("Authorization") != "Bearer bot" || r.Header.Get("Cookie") != "" {
						t.Error("unexpected bot headers")
					}
				} else {
					status, contentType = tc.user, tc.userType
					wantCookie := "d=" + downloadUserCookie
					if tc.noCookie {
						wantCookie = ""
					}
					if r.Header.Get("Authorization") != "Bearer "+downloadUserToken || r.Header.Get("Cookie") != wantCookie {
						t.Error("unexpected user headers")
					}
				}
				if contentType == "" {
					contentType = "image/png"
				}
				w.Header().Set("Content-Type", contentType)
				if status == http.StatusFound {
					w.Header().Set("Location", "https://evil.example/"+downloadUserToken)
				}
				w.WriteHeader(status)
				io.WriteString(w, "file bytes")
			})
			var logs bytes.Buffer
			deps := downloadTestDeps(&logs)
			deps.SlackUserCreds = func() (string, string) {
				reads++
				token, cookie := downloadUserToken, downloadUserCookie
				if tc.noToken {
					token = ""
				}
				if tc.noCookie {
					cookie = ""
				}
				return token, cookie
			}
			declared := tc.declared
			if declared == "" {
				declared = "image/png"
			}
			fileURL := "https://files.slack.com/files-pri/test.png"
			data, err := downloadWithAuth(context.Background(), fileURL, "slack", declared, deps)
			assertDownloadNoLeaks(t, err, logs.String())
			if (err != nil) != tc.fail {
				t.Fatalf("error = %v, want failure %v", err, tc.fail)
			}
			if errors.Is(err, jobs.ErrFatal) != tc.fatal {
				t.Errorf("fatal classification = %v", err)
			}
			if tc.message != "" && err != nil {
				want := strings.Replace(tc.message, "fatal: ", jobs.ErrFatal.Error()+": ", 1) + fileURL
				if err.Error() != want {
					t.Errorf("error = %q, want %q", err, want)
				}
			}
			if err == nil && string(data) != "file bytes" {
				t.Errorf("bytes = %q", data)
			}
			if requests != tc.requests {
				t.Errorf("requests = %d, want %d", requests, tc.requests)
			}
			if reads != 1 {
				t.Errorf("credential snapshots = %d, want 1", reads)
			}
			if !tc.fail && tc.requests == 2 {
				if !strings.Contains(logs.String(), "\"level\":\"info\"") || !strings.Contains(logs.String(), "test.png") {
					t.Error("fallback success missing INFO file log")
				}
			} else if logs.Len() != 0 {
				t.Error("unexpected fallback success log")
			}
		})
	}
}

func TestDownloadSlackCredentialAllowlist(t *testing.T) {
	for _, fileURL := range []string{
		"http://files.slack.com/file",
		"https://evil.example/file",
		"https://files.slack.com.evil.example/file",
		"https://files.slack.com:443/file",
		"https://FILES.slack.com/file",
	} {
		t.Run(fileURL, func(t *testing.T) {
			requests := 0
			installDownloadServer(t, func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
					t.Error("credentials sent outside allowlist")
				}
				w.WriteHeader(http.StatusForbidden)
			})
			var logs bytes.Buffer
			_, err := downloadWithAuth(context.Background(), fileURL, "slack", "image/png", downloadTestDeps(&logs))
			assertDownloadNoLeaks(t, err, logs.String())
			if !errors.Is(err, jobs.ErrFatal) || requests != 1 {
				t.Errorf("error = %v, requests = %d", err, requests)
			}
		})
	}
}

func TestDownloadNonSlackNeverUsesUserCredentials(t *testing.T) {
	requests := 0
	installDownloadServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("Authorization") != "Bearer jira" || r.Header.Get("Cookie") != "" {
			t.Error("unexpected Jira credentials")
		}
		w.WriteHeader(http.StatusForbidden)
	})
	var logs bytes.Buffer
	deps := downloadTestDeps(&logs)
	deps.JiraToken = "jira"
	deps.SlackUserCreds = func() (string, string) {
		t.Error("non-Slack read user credentials")
		return downloadUserToken, downloadUserCookie
	}
	_, err := downloadWithAuth(context.Background(), "https://files.slack.com/file", "jira", "image/png", deps)
	assertDownloadNoLeaks(t, err, logs.String())
	if !errors.Is(err, jobs.ErrFatal) || requests != 1 {
		t.Errorf("error = %v, requests = %d", err, requests)
	}
}

func TestDownloadSlackRedirects(t *testing.T) {
	for _, tc := range []struct {
		name, target string
		fallback     bool
		requests     int
		fail         bool
	}{
		{"same host bot", "https://files.slack.com/final", false, 2, false},
		{"same host user", "https://files.slack.com/final", true, 3, false},
		{"foreign bot", "https://evil.example/final", false, 2, true},
		{"foreign user", "https://evil.example/final", true, 2, true},
		{"http downgrade user", "http://files.slack.com/final", true, 2, true},
		{"suffix user", "https://files.slack.com.evil.example/final", true, 2, true},
		{"port user", "https://files.slack.com:443/final", true, 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests, foreign := 0, 0
			installDownloadServer(t, func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.Host != "files.slack.com" {
					foreign++
				}
				if tc.fallback && requests == 1 {
					w.WriteHeader(403)
					return
				}
				wantAuth, wantCookie := "Bearer bot", ""
				if (tc.fallback && requests > 1) || (!tc.fallback && tc.fail && requests > 1) {
					wantAuth, wantCookie = "Bearer "+downloadUserToken, "d="+downloadUserCookie
				}
				if r.Header.Get("Authorization") != wantAuth || r.Header.Get("Cookie") != wantCookie {
					t.Error("redirect lost credentials on allowed host")
				}
				if r.URL.Path != "/final" {
					http.Redirect(w, r, tc.target, http.StatusFound)
					return
				}
				w.Header().Set("Content-Type", "image/png")
				io.WriteString(w, "redirect bytes")
			})
			var logs bytes.Buffer
			data, err := downloadWithAuth(context.Background(), "https://files.slack.com/start", "slack", "image/png", downloadTestDeps(&logs))
			assertDownloadNoLeaks(t, err, logs.String())
			if (err != nil) != tc.fail {
				t.Errorf("error = %v, want fail %v", err, tc.fail)
			}
			if errors.Is(err, jobs.ErrFatal) != tc.fail {
				t.Errorf("fatal classification = %v", err)
			}
			if !tc.fail && string(data) != "redirect bytes" {
				t.Errorf("bytes = %q", data)
			}
			if requests != tc.requests || foreign != 0 {
				t.Errorf("requests = %d, foreign = %d", requests, foreign)
			}
		})
	}
}

func TestDownloadSlackRedirectLimit(t *testing.T) {
	for _, source := range []string{"slack", "jira"} {
		t.Run(source, func(t *testing.T) {
			requests := 0
			installDownloadServer(t, func(w http.ResponseWriter, r *http.Request) {
				requests++
				http.Redirect(w, r, "https://files.slack.com/"+strconv.Itoa(requests), http.StatusFound)
			})
			var logs bytes.Buffer
			_, err := downloadWithAuth(context.Background(), "https://files.slack.com/start", source, "image/png", downloadTestDeps(&logs))
			assertDownloadNoLeaks(t, err, logs.String())
			if err == nil || errors.Is(err, jobs.ErrFatal) || requests != 10 {
				t.Errorf("error = %v, requests = %d; want redirect cap 10", err, requests)
			}
		})
	}
}

type downloadFailTransport struct{ calls *int }

func (tr downloadFailTransport) RoundTrip(*http.Request) (*http.Response, error) {
	(*tr.calls)++
	return nil, fmt.Errorf("network error %s %s", downloadUserToken, downloadUserCookie)
}

func TestDownloadSlackNetworkErrorNoFallback(t *testing.T) {
	calls := 0
	old := http.DefaultTransport
	http.DefaultTransport = downloadFailTransport{calls: &calls}
	t.Cleanup(func() { http.DefaultTransport = old })
	var logs bytes.Buffer
	_, err := downloadWithAuth(context.Background(), "https://files.slack.com/file", "slack", "image/png", downloadTestDeps(&logs))
	assertDownloadNoLeaks(t, err, logs.String())
	if err == nil || errors.Is(err, jobs.ErrFatal) || calls != 1 {
		t.Errorf("error = %v, calls = %d", err, calls)
	}
}

func TestDownloadSlackFreshCredentials(t *testing.T) {
	requests, reads := 0, 0
	token := downloadUserToken
	installDownloadServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests%2 == 1 {
			w.WriteHeader(403)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Error("download used stale credential")
		}
		w.Header().Set("Content-Type", "image/png")
		io.WriteString(w, "fresh bytes")
	})
	var logs bytes.Buffer
	deps := downloadTestDeps(&logs)
	deps.SlackUserCreds = func() (string, string) { reads++; return token, downloadUserCookie }
	for range 2 {
		_, err := downloadWithAuth(context.Background(), "https://files.slack.com/file", "slack", "image/png", deps)
		assertDownloadNoLeaks(t, err, logs.String())
		if err != nil {
			t.Fatal(err)
		}
		token = "updated-user-token"
	}
	if requests != 4 || reads != 2 {
		t.Errorf("requests = %d, snapshots = %d", requests, reads)
	}
}

type downloadRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn downloadRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

type downloadErrorBody struct{ err error }

func (body downloadErrorBody) Read([]byte) (int, error) { return 0, body.err }
func (downloadErrorBody) Close() error                  { return nil }

func TestDownloadSlackFailedUserRequestNoFurtherFallback(t *testing.T) {
	for _, failBody := range []bool{false, true} {
		t.Run(fmt.Sprintf("body=%v", failBody), func(t *testing.T) {
			calls := 0
			old := http.DefaultTransport
			http.DefaultTransport = downloadRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				if calls == 1 {
					return &http.Response{StatusCode: 403, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
				}
				leaky := fmt.Errorf("failure %s %s", downloadUserToken, downloadUserCookie)
				if failBody {
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"image/png"}}, Body: downloadErrorBody{err: leaky}, Request: req}, nil
				}
				return nil, leaky
			})
			t.Cleanup(func() { http.DefaultTransport = old })
			var logs bytes.Buffer
			_, err := downloadWithAuth(context.Background(), "https://files.slack.com/file", "slack", "image/png", downloadTestDeps(&logs))
			assertDownloadNoLeaks(t, err, logs.String())
			if err == nil || errors.Is(err, jobs.ErrFatal) || calls != 2 {
				t.Errorf("error = %v, requests = %d", err, calls)
			}
		})
	}
}

func TestDownloadNonSlackPreservesTransportAndBodyErrors(t *testing.T) {
	for _, failBody := range []bool{false, true} {
		t.Run(fmt.Sprintf("body=%v", failBody), func(t *testing.T) {
			failure := errors.New("original download failure")
			calls := 0
			old := http.DefaultTransport
			http.DefaultTransport = downloadRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls++
				if failBody {
					return &http.Response{StatusCode: 200, Header: http.Header{}, Body: downloadErrorBody{err: failure}, Request: req}, nil
				}
				return nil, failure
			})
			t.Cleanup(func() { http.DefaultTransport = old })
			var logs bytes.Buffer
			_, err := downloadWithAuth(context.Background(), "https://jira.example/file", "jira", "image/png", downloadTestDeps(&logs))
			assertDownloadNoLeaks(t, err, logs.String())
			if !errors.Is(err, failure) || calls != 1 {
				t.Errorf("original error not preserved: %v; requests = %d", err, calls)
			}
		})
	}
}

func TestDownloadNonSlackKeepsCrossHostRedirectBehavior(t *testing.T) {
	requests, foreign := 0, 0
	installDownloadServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Host == "foreign.example" {
			foreign++
		}
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "https://foreign.example/final", http.StatusFound)
			return
		}
		io.WriteString(w, "non-Slack bytes")
	})
	var logs bytes.Buffer
	data, err := downloadWithAuth(context.Background(), "https://jira.example/start", "jira", "image/png", downloadTestDeps(&logs))
	assertDownloadNoLeaks(t, err, logs.String())
	if err != nil || string(data) != "non-Slack bytes" || requests != 2 || foreign != 1 {
		t.Errorf("error = %v, bytes = %q, requests = %d, foreign = %d", err, data, requests, foreign)
	}
}

func TestDownloadSlackUserRedirectLimit(t *testing.T) {
	requests := 0
	installDownloadServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+downloadUserToken || r.Header.Get("Cookie") != "d="+downloadUserCookie {
			t.Error("missing user redirect credentials")
		}
		http.Redirect(w, r, "https://files.slack.com/"+strconv.Itoa(requests), http.StatusFound)
	})
	var logs bytes.Buffer
	_, err := downloadWithAuth(context.Background(), "https://files.slack.com/start", "slack", "image/png", downloadTestDeps(&logs))
	assertDownloadNoLeaks(t, err, logs.String())
	if err == nil || errors.Is(err, jobs.ErrFatal) || requests != 11 {
		t.Errorf("error = %v, requests = %d; want bot plus capped user chain", err, requests)
	}
}

func TestDownloadSlackOffHostRedirectFallsBack(t *testing.T) {
	requests, foreign := 0, 0
	installDownloadServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "files.slack.com" {
			foreign++
			t.Error("foreign host requested")
			return
		}
		requests++
		if requests == 1 {
			if r.Header.Get("Authorization") != "Bearer bot" || r.Header.Get("Cookie") != "" {
				t.Error("unexpected bot credentials")
			}
			http.Redirect(w, r, "https://evil.example/"+downloadUserToken, http.StatusFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+downloadUserToken || r.Header.Get("Cookie") != "d="+downloadUserCookie {
			t.Error("unexpected user credentials")
		}
		io.WriteString(w, "user bytes")
	})
	var logs bytes.Buffer
	data, err := downloadWithAuth(context.Background(), "https://files.slack.com/file", "slack", "image/png", downloadTestDeps(&logs))
	assertDownloadNoLeaks(t, err, logs.String())
	if err != nil || string(data) != "user bytes" || requests != 2 || foreign != 0 {
		t.Fatalf("error = %v, bytes = %q, requests = %d, foreign = %d", err, data, requests, foreign)
	}
}

type downloadUnreadBody struct {
	reads, closes int
}

func (b *downloadUnreadBody) Read([]byte) (int, error) {
	b.reads++
	return 0, fmt.Errorf("refusal body %s %s", downloadUserToken, downloadUserCookie)
}

func (b *downloadUnreadBody) Close() error {
	b.closes++
	return nil
}

func TestDownloadSlackFinalRedirectBodies(t *testing.T) {
	for _, tc := range []struct {
		name, initial, location, message string
		bot, user, requests              int
		fatal                            bool
	}{
		{name: "bot redirect user bytes", bot: 302, user: 200, location: "https://evil.example/file", requests: 2},
		{name: "both redirect", bot: 302, user: 302, location: "https://evil.example/file", requests: 2, fatal: true, message: "download HTTP redirect (bot), redirect (user): "},
		{name: "anonymous redirect", initial: "https://other.example/file", bot: 302, location: "https://evil.example/file", requests: 1, fatal: true, message: "download: slack redirect (no auth): "},
		{name: "302 without location", bot: 302, requests: 1, fatal: true, message: "download HTTP 302: "},
		{name: "304", bot: 304, requests: 1, fatal: true, message: "download HTTP 304: "},
		{name: "reset flag user302", bot: 302, user: 302, location: "https://evil.example/file", requests: 2, fatal: true, message: "download HTTP redirect (bot), 302 (user): "},
		{name: "redirect user network", bot: 302, user: -1, location: "https://evil.example/file", requests: 2, message: "http get: download request failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			initial := tc.initial
			if initial == "" {
				initial = "https://files.slack.com/file"
			}
			calls, foreign := 0, 0
			var bodies []*downloadUnreadBody
			old := http.DefaultTransport
			http.DefaultTransport = downloadRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.String() != initial {
					foreign++
					return nil, errors.New("foreign request")
				}
				for _, body := range bodies {
					if body.closes != 1 {
						t.Error("previous response not closed before fallback")
					}
				}
				calls++
				auth, cookie := "Bearer bot", ""
				if tc.initial != "" {
					auth = ""
				} else if calls > 1 {
					auth, cookie = "Bearer "+downloadUserToken, "d="+downloadUserCookie
				}
				if req.Header.Get("Authorization") != auth || req.Header.Get("Cookie") != cookie {
					t.Error("unexpected credentials")
				}
				status := tc.bot
				if calls > 1 {
					status = tc.user
				}
				if status == -1 {
					return nil, fmt.Errorf("network %s", downloadUserToken)
				}
				header := http.Header{}
				if tc.location != "" && !(tc.name == "reset flag user302" && calls > 1) {
					header.Set("Location", tc.location+"/"+downloadUserCookie)
				}
				var body io.ReadCloser = io.NopCloser(strings.NewReader("user bytes"))
				if status >= 300 && status < 400 {
					unread := &downloadUnreadBody{}
					bodies = append(bodies, unread)
					body = unread
				}
				return &http.Response{StatusCode: status, Header: header, Body: body, Request: req}, nil
			})
			t.Cleanup(func() { http.DefaultTransport = old })
			var logs bytes.Buffer
			data, err := downloadWithAuth(context.Background(), initial, "slack", "image/png", downloadTestDeps(&logs))
			assertDownloadNoLeaks(t, err, logs.String())
			if errors.Is(err, jobs.ErrFatal) != tc.fatal || calls != tc.requests || foreign != 0 {
				t.Errorf("error = %v, calls = %d, foreign = %d", err, calls, foreign)
			}
			if tc.message == "" {
				if err != nil || string(data) != "user bytes" {
					t.Errorf("error = %v, data = %q", err, data)
				}
			} else {
				want := tc.message
				if tc.fatal {
					want = jobs.ErrFatal.Error() + ": " + want + initial
				}
				if err == nil || err.Error() != want {
					t.Errorf("error = %v, want %q", err, want)
				}
			}
			for _, body := range bodies {
				if body.reads != 0 || body.closes != 1 {
					t.Errorf("body reads = %d, closes = %d", body.reads, body.closes)
				}
			}
		})
	}
}

func TestDownloadSlackRedirectCapBeforeRefusal(t *testing.T) {
	requests := 0
	installDownloadServer(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Header.Get("Authorization") != "Bearer bot" || r.Header.Get("Cookie") != "" {
			t.Error("cap triggered fallback")
		}
		target := "https://files.slack.com/" + strconv.Itoa(requests)
		if requests == 10 {
			target = "https://evil.example/" + downloadUserToken
		}
		http.Redirect(w, r, target, http.StatusFound)
	})
	var logs bytes.Buffer
	_, err := downloadWithAuth(context.Background(), "https://files.slack.com/start", "slack", "image/png", downloadTestDeps(&logs))
	assertDownloadNoLeaks(t, err, logs.String())
	if err == nil || errors.Is(err, jobs.ErrFatal) || requests != 10 {
		t.Errorf("error = %v, requests = %d", err, requests)
	}
}
