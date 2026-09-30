package fetchers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/agent-mem/agent-mem/internal/graph/normalizer"
)

func TestJiraFetcher_Matches(t *testing.T) {
	f := &jiraFetcher{}
	cases := []struct {
		input string
		want  bool
	}{
		{"jira:PAY-2128", true},
		{"jira:PROJ-1", true},
		{"https://wegomushi.atlassian.net/browse/PAY-2128", true},
		{"slack:C08:1.2", false},
		{"gh_pr:wego/payments#1", false},
		{"jira:lowercase-123", false},
	}
	for _, tc := range cases {
		got := f.Matches(tc.input)
		if got != tc.want {
			t.Errorf("Matches(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

func TestJiraFetcher_HappyPath(t *testing.T) {
	adf := map[string]any{
		"version": 1,
		"type":    "doc",
		"content": []map[string]any{
			{
				"type": "paragraph",
				"content": []map[string]any{
					{"type": "text", "text": "Issue body"},
				},
			},
		},
	}
	resp := jiraIssueResponse{
		Key: "PAY-2128",
		Fields: jiraFields{
			Summary:  "Test issue",
			Updated:  "2024-01-15T10:30:00Z",
			Reporter: &jiraUser{AccountID: "acc-123", DisplayName: "Alice", EmailAddress: "alice@example.com"},
		},
	}
	adfBytes, _ := json.Marshal(adf)
	resp.Fields.Description = adfBytes

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkJiraAuth(t, r, "user@example.com", "token-abc")
		if strings.HasSuffix(r.URL.Path, "/comment") {
			fmt.Fprint(w, `{"total":0,"comments":[]}`)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/remotelink") {
			fmt.Fprint(w, `[]`)
			return
		}
		if r.Header.Get("Accept") != "application/json" {
			http.Error(w, "bad accept", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	cfg := Config{
		JiraEmail:   "user@example.com",
		JiraToken:   "token-abc",
		JiraBaseURL: srv.URL,
		HTTPClient:  srv.Client(),
	}
	f := newJiraFetcher(cfg, noLogger())
	body, err := f.Fetch(context.Background(), "jira:PAY-2128")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if body.NodeID != "jira:PAY-2128" {
		t.Errorf("nodeID = %q", body.NodeID)
	}
	if body.Title != "Test issue" {
		t.Errorf("title = %q", body.Title)
	}
	if body.Author.ExternalID != "acc-123" {
		t.Errorf("author ID = %q", body.Author.ExternalID)
	}
	if body.ContentType != "application/json" {
		t.Errorf("content type = %q", body.ContentType)
	}
	if len(body.Raw) == 0 {
		t.Error("raw body is empty")
	}
}

func TestJiraFetcher_AuthHeader(t *testing.T) {
	var routes []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkJiraAuth(t, r, "user@eg.com", "secret")
		routes = append(routes, r.URL.Path)
		if strings.HasSuffix(r.URL.Path, "/comment") {
			fmt.Fprint(w, `{"total":0,"comments":[]}`)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/remotelink") {
			fmt.Fprint(w, `[]`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(jiraIssueResponse{
			Key:    "PAY-1",
			Fields: jiraFields{Summary: "s"},
		})
	}))
	defer srv.Close()

	cfg := Config{
		JiraEmail:   "user@eg.com",
		JiraToken:   "secret",
		JiraBaseURL: srv.URL,
		HTTPClient:  srv.Client(),
	}
	f := newJiraFetcher(cfg, noLogger())
	if _, err := f.Fetch(context.Background(), "jira:PAY-1"); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if len(routes) != 3 {
		t.Errorf("routes = %v, want issue, comments and remote links", routes)
	}
}

func TestJiraFetcher_404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkJiraAuth(t, r, "u", "t")
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	cfg := Config{JiraEmail: "u", JiraToken: "t", JiraBaseURL: srv.URL, HTTPClient: srv.Client()}
	f := newJiraFetcher(cfg, noLogger())
	_, err := f.Fetch(context.Background(), "jira:PAY-1")
	if err == nil {
		t.Fatal("expected error for 404")
	}
}

func TestJiraFetcher_5xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkJiraAuth(t, r, "u", "t")
		http.Error(w, "server error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	cfg := Config{JiraEmail: "u", JiraToken: "t", JiraBaseURL: srv.URL, HTTPClient: srv.Client()}
	f := newJiraFetcher(cfg, noLogger())
	_, err := f.Fetch(context.Background(), "jira:PAY-1")
	if err == nil {
		t.Fatal("expected error for 5xx")
	}
}

func checkJiraAuth(t *testing.T, r *http.Request, email, token string) {
	t.Helper()
	user, pass, ok := r.BasicAuth()
	if !ok || user != email || pass != token {
		t.Errorf("%s: Basic auth = %q:%q", r.URL.Path, user, pass)
	}
	if r.Header.Get("Accept") != "application/json" {
		t.Errorf("%s: Accept = %q", r.URL.Path, r.Header.Get("Accept"))
	}
}

func jiraTestDoc(text string) map[string]any {
	return map[string]any{"type": "doc", "version": 1, "content": []any{
		map[string]any{"type": "paragraph", "content": []any{map[string]any{"type": "text", "text": text}}},
	}}
}

func jiraTestComment(i int) map[string]any {
	return map[string]any{"id": strconv.Itoa(i), "author": map[string]any{"displayName": "Alice"},
		"created": "2026-09-30T10:00:00Z", "body": jiraTestDoc(fmt.Sprintf("comment %03d", i))}
}

// Each fixture authenticates every route, including fault-injected requests.
func jiraTestServer(t *testing.T, description any, links any, comments, remote http.HandlerFunc) *jiraFetcher {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkJiraAuth(t, r, "u", "t")
		switch r.URL.Path {
		case "/rest/api/3/issue/TEST-1":
			if !strings.Contains(r.URL.Query().Get("fields"), "issuelinks") {
				t.Error("issue fields omit issuelinks")
			}
			for _, field := range strings.Split(r.URL.Query().Get("fields"), ",") {
				if field == "comment" {
					t.Error("issue fields must not include comment")
				}
			}
			json.NewEncoder(w).Encode(map[string]any{"key": "TEST-1", "fields": map[string]any{
				"description": description, "issuelinks": links, "updated": "2026-09-30T10:00:00Z",
			}})
		case "/rest/api/3/issue/TEST-1/comment":
			if r.URL.Query().Get("orderBy") != "created" || r.URL.Query().Get("maxResults") != "100" {
				t.Errorf("comment query = %s", r.URL.RawQuery)
			}
			if comments != nil {
				comments(w, r)
			} else {
				fmt.Fprint(w, `{"total":0,"comments":[]}`)
			}
		case "/rest/api/3/issue/TEST-1/remotelink":
			if remote != nil {
				remote(w, r)
			} else {
				fmt.Fprint(w, `[]`)
			}
		default:
			t.Errorf("unexpected route %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return newJiraFetcher(Config{JiraEmail: "u", JiraToken: "t", JiraBaseURL: srv.URL, HTTPClient: srv.Client()}, noLogger())
}

func jiraTestText(t *testing.T, f *jiraFetcher) string {
	t.Helper()
	body, err := f.Fetch(context.Background(), "jira:TEST-1")
	if err != nil {
		t.Fatal(err)
	}
	if !body.BodyTS.Equal(ParseJiraTime("2026-09-30T10:00:00Z")) {
		t.Errorf("BodyTS = %v", body.BodyTS)
	}
	result, err := normalizer.NewJiraNormalizer().Normalize(context.Background(), body.Raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	return result.Text
}

func TestJiraFetch_CommentsInBody(t *testing.T) {
	url := "https://wego.slack.com/archives/C01234567/p1790668180910269"
	first, second := jiraTestComment(1), jiraTestComment(2)
	second["body"] = jiraTestDoc(url)
	f := jiraTestServer(t, jiraTestDoc("description"), nil, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"total": 2, "comments": []any{first, second}})
	}, nil)
	text := jiraTestText(t, f)
	want := "description\n\n--- comment by Alice @ 2026-09-30T10:00:00Z ---\n\ncomment 001\n\n--- comment by Alice @ 2026-09-30T10:00:00Z ---\n\n" + url
	if strings.TrimSpace(text) != want {
		t.Errorf("text = %q, want %q", text, want)
	}
}

func jiraTestPagedComments(t *testing.T, total int, starts *[]int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start, err := strconv.Atoi(r.URL.Query().Get("startAt"))
		if err != nil {
			t.Errorf("startAt: %v", err)
		}
		*starts = append(*starts, start)
		end := start + 100
		if end > total {
			end = total
		}
		comments := make([]any, 0, end-start)
		for i := start; i < end; i++ {
			comments = append(comments, jiraTestComment(i))
		}
		json.NewEncoder(w).Encode(map[string]any{"startAt": start, "maxResults": 100, "total": total, "comments": comments})
	}
}

func TestJiraFetch_CommentPaging(t *testing.T) {
	var starts []int
	f := jiraTestServer(t, nil, nil, jiraTestPagedComments(t, 150, &starts), nil)
	text := jiraTestText(t, f)
	if !reflect.DeepEqual(starts, []int{0, 100}) {
		t.Errorf("starts = %v", starts)
	}
	for i := range 150 {
		if strings.Count(text+"\n", fmt.Sprintf("comment %03d\n", i)) != 1 {
			t.Errorf("comment %d missing or duplicated", i)
		}
	}
}

func TestJiraFetch_CommentEmptyPageFails(t *testing.T) {
	var starts []string
	f := jiraTestServer(t, nil, nil, func(w http.ResponseWriter, r *http.Request) {
		starts = append(starts, r.URL.Query().Get("startAt"))
		comments := []any{}
		if len(starts) == 1 {
			comments = []any{jiraTestComment(1), jiraTestComment(2)}
		}
		json.NewEncoder(w).Encode(map[string]any{"total": 5, "comments": comments})
	}, func(w http.ResponseWriter, r *http.Request) { t.Error("remote links reached after empty page") })
	body, err := f.Fetch(context.Background(), "jira:TEST-1")
	if err == nil || !strings.Contains(err.Error(), "jira comments: empty page at startAt=2 of total=5") {
		t.Errorf("error = %v", err)
	}
	if !reflect.DeepEqual(body, FetchedBody{}) || !reflect.DeepEqual(starts, []string{"0", "2"}) {
		t.Errorf("body = %+v, starts = %v", body, starts)
	}
}

func TestJiraFetch_CommentCap(t *testing.T) {
	var starts []int
	f := jiraTestServer(t, nil, nil, jiraTestPagedComments(t, 600, &starts), nil)
	text := jiraTestText(t, f)
	if !reflect.DeepEqual(starts, []int{0, 100, 200, 300, 400}) {
		t.Errorf("starts = %v", starts)
	}
	if strings.Count(text, "--- comment by ") != 500 || strings.Contains(text, "comment 500") {
		t.Error("did not retain exactly first 500 comments")
	}
	for i := range 500 {
		if strings.Count(text+"\n", fmt.Sprintf("comment %03d\n", i)) != 1 {
			t.Errorf("comment %d missing or duplicated", i)
		}
	}
}

func TestJiraFetch_CommentOddBodies(t *testing.T) {
	f := jiraTestServer(t, nil, nil, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"total": 3, "comments": []any{
			map[string]any{"created": "one", "body": nil},
			map[string]any{"author": map[string]any{"displayName": "Bob"}, "created": "two", "body": map[string]any{"type": "doc"}},
			map[string]any{"created": "three", "body": "not an object"},
		}})
	}, nil)
	text := jiraTestText(t, f)
	want := "--- comment by unknown @ one ---\n\n--- comment by Bob @ two ---\n\n--- comment by unknown @ three ---"
	if strings.TrimSpace(text) != want {
		t.Errorf("text = %q", text)
	}
}

func TestJiraFetch_IssueLinks(t *testing.T) {
	links := []any{
		map[string]any{"outwardIssue": map[string]any{"key": "PAY-12"}},
		map[string]any{"inwardIssue": map[string]any{"key": "FBT-34"}},
		map[string]any{"outwardIssue": map[string]any{"key": "PAY-12"}},
	}
	if text := jiraTestText(t, jiraTestServer(t, nil, links, nil, nil)); strings.TrimSpace(text) != "Linked issues: PAY-12, FBT-34" {
		t.Errorf("text = %q", text)
	}
}

func TestJiraFetch_RemoteLinks(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(fmt.Sprint(empty), func(t *testing.T) {
			f := jiraTestServer(t, nil, nil, nil, func(w http.ResponseWriter, r *http.Request) {
				if empty {
					fmt.Fprint(w, `[]`)
				} else {
					fmt.Fprint(w, `[{"object":{"title":"Slack","url":"https://wego.slack.com/archives/C01234567/p1790668180910269"}},{"object":{"title":"PR","url":"https://github.com/wego/payments/pull/42"}}]`)
				}
			})
			want := "Link: Slack (https://wego.slack.com/archives/C01234567/p1790668180910269)\n\nLink: PR (https://github.com/wego/payments/pull/42)"
			if empty {
				want = ""
			}
			if text := jiraTestText(t, f); strings.TrimSpace(text) != want {
				t.Errorf("text = %q, want %q", text, want)
			}
		})
	}
}

func TestJiraFetch_EmptyDescriptionWithComments(t *testing.T) {
	f := jiraTestServer(t, nil, nil, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"total": 1, "comments": []any{jiraTestComment(1)}})
	}, nil)
	if text := jiraTestText(t, f); strings.TrimSpace(text) != "--- comment by Alice @ 2026-09-30T10:00:00Z ---\n\ncomment 001" {
		t.Errorf("text = %q", text)
	}
}

func jiraTestDisconnect(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	conn, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		t.Error(err)
		return
	}
	conn.Close()
}

func TestJiraFetch_CommentErrorsFail(t *testing.T) {
	for _, fault := range []string{"second page 500", "invalid JSON", "transport", "cancel"} {
		t.Run(fault, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var mu sync.Mutex
			var routes []string
			calls := 0
			f := jiraTestServer(t, jiraTestDoc("description"), nil, func(w http.ResponseWriter, r *http.Request) {
				calls++
				switch fault {
				case "second page 500":
					if calls == 1 {
						fmt.Fprint(w, `{"total":2,"comments":[{"body":null}]}`)
					} else {
						http.Error(w, "fault", 500)
					}
				case "invalid JSON":
					fmt.Fprint(w, `{`)
				case "transport":
					jiraTestDisconnect(t, w)
				case "cancel":
					cancel()
					<-r.Context().Done()
				}
			}, nil)
			original := f.cfg.HTTPClient.Transport
			f.cfg.HTTPClient.Transport = jiraTestRoundTripper(func(r *http.Request) (*http.Response, error) {
				mu.Lock()
				routes = append(routes, r.URL.Path)
				mu.Unlock()
				return original.RoundTrip(r)
			})
			body, err := f.Fetch(ctx, "jira:TEST-1")
			if err == nil || !reflect.DeepEqual(body, FetchedBody{}) {
				t.Errorf("body = %+v, error = %v", body, err)
			}
			want := []string{"/rest/api/3/issue/TEST-1", "/rest/api/3/issue/TEST-1/comment"}
			if fault == "second page 500" {
				want = append(want, "/rest/api/3/issue/TEST-1/comment")
			}
			if !reflect.DeepEqual(routes, want) {
				t.Errorf("routes = %v, want %v", routes, want)
			}
		})
	}
}

type jiraTestRoundTripper func(*http.Request) (*http.Response, error)

func (f jiraTestRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestJiraFetch_RemoteLinkErrorFails(t *testing.T) {
	for _, fault := range []string{"404", "500", "invalid JSON", "transport"} {
		t.Run(fault, func(t *testing.T) {
			var routes []string
			f := jiraTestServer(t, jiraTestDoc("description"), nil, nil, func(w http.ResponseWriter, r *http.Request) {
				switch fault {
				case "404":
					http.Error(w, "fault", 404)
				case "500":
					http.Error(w, "fault", 500)
				case "invalid JSON":
					fmt.Fprint(w, `{`)
				case "transport":
					jiraTestDisconnect(t, w)
				}
			})
			original := f.cfg.HTTPClient.Transport
			f.cfg.HTTPClient.Transport = jiraTestRoundTripper(func(r *http.Request) (*http.Response, error) {
				routes = append(routes, r.URL.Path)
				return original.RoundTrip(r)
			})
			body, err := f.Fetch(context.Background(), "jira:TEST-1")
			if err == nil || !reflect.DeepEqual(body, FetchedBody{}) {
				t.Errorf("body = %+v, error = %v", body, err)
			}
			want := []string{"/rest/api/3/issue/TEST-1", "/rest/api/3/issue/TEST-1/comment", "/rest/api/3/issue/TEST-1/remotelink"}
			if !reflect.DeepEqual(routes, want) {
				t.Errorf("routes = %v, want %v", routes, want)
			}
		})
	}
}
