package fetchers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestGWSFetcher_Matches(t *testing.T) {
	f := &gwsFetcher{}
	cases := []struct {
		input string
		want  bool
	}{
		{"gws_doc:1BxiMVs0XRA5nFMdKvBdBZjgmUUqptlbs74OgVE2upms", true},
		{"https://docs.google.com/document/d/1BxiMVs0XRA5nFMdKvBdBZjgmUUqptlbs74OgVE2upms/edit", true},
		{"https://drive.google.com/file/d/1BxiMVs0XRA5nFMdKvBdBZjgmUUqptlbs74OgVE2upms/view", true},
		{"jira:PAY-123", false},
		{"slack:C:1.2", false},
	}
	for _, tc := range cases {
		got := f.Matches(tc.input)
		if got != tc.want {
			t.Errorf("Matches(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

func TestGWSFetcher_NotConfigured(t *testing.T) {
	// Neither GWSServiceKeyPath nor GWS_BEARER_TOKEN set.
	t.Setenv("GWS_BEARER_TOKEN", "")
	cfg := Config{GWSServiceKeyPath: ""}
	f := newGWSFetcher(cfg, noLogger())
	_, err := f.Fetch(context.Background(), "gws_doc:someDocID")
	if err == nil {
		t.Fatal("expected error when not configured")
	}
}

func TestGWSFetcher_HappyPath_DocsAPI(t *testing.T) {
	docID := "1BxiMVs0XRA5nFMdKvBdBZjgmUUqptlbs74OgVE2upms"
	docResp := gwsDocResponse{
		DocumentID: docID,
		Title:      "Architecture Doc",
		RevisionID: "rev1",
	}

	var gotAuth string
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/documents/"+docID, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(docResp)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	t.Setenv("GWS_BEARER_TOKEN", "gws-token")
	cfg := Config{
		GWSServiceKeyPath: "/fake/path",
		HTTPClient:        newRewriteClientForHost("docs.googleapis.com", srv.URL, srv.Client()),
	}
	f := newGWSFetcher(cfg, noLogger())

	body, err := f.Fetch(context.Background(), "gws_doc:"+docID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if body.NodeID != "gws_doc:"+docID {
		t.Errorf("nodeID = %q", body.NodeID)
	}
	if body.Title != "Architecture Doc" {
		t.Errorf("title = %q", body.Title)
	}
	if gotAuth != "Bearer gws-token" {
		t.Errorf("auth = %q", gotAuth)
	}
	if body.ContentType != "application/json" {
		t.Errorf("content type = %q", body.ContentType)
	}
}

func TestGWSFetcher_FallbackDriveExport(t *testing.T) {
	fileID := "driveFileID123"
	fileMeta := gwsFileResponse{
		ID:           fileID,
		Name:         "My Spreadsheet",
		MimeType:     "application/vnd.google-apps.spreadsheet",
		ModifiedTime: time.Now(),
		Owners:       []gwsOwner{{DisplayName: "Carol", EmailAddress: "carol@example.com"}},
	}

	mux := http.NewServeMux()
	// Docs API returns 404 (not a doc).
	mux.HandleFunc("/v1/documents/"+fileID, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not a document", http.StatusNotFound)
	})
	// Drive metadata.
	mux.HandleFunc("/drive/v3/files/"+fileID, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(fileMeta)
	})
	// Drive export.
	mux.HandleFunc("/drive/v3/files/"+fileID+"/export", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("exported plain text"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	t.Setenv("GWS_BEARER_TOKEN", "gws-token")
	cfg := Config{
		GWSServiceKeyPath: "/fake/path",
		HTTPClient:        newRewriteClientForHosts(map[string]string{
			"docs.googleapis.com":        srv.URL,
			"www.googleapis.com":         srv.URL,
		}, srv.Client()),
	}
	f := newGWSFetcher(cfg, noLogger())
	body, err := f.Fetch(context.Background(), "gws_doc:"+fileID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if body.NodeID != "gws_doc:"+fileID {
		t.Errorf("nodeID = %q", body.NodeID)
	}
	if body.ContentType != "text/plain" {
		t.Errorf("content type = %q", body.ContentType)
	}
}
