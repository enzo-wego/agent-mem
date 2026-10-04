package normalizer

import (
	"context"
	"strings"
	"testing"

	"github.com/agent-mem/agent-mem/internal/graph/extractor"
	"github.com/rs/zerolog"
)

func TestJiraNormalizer(t *testing.T) {
	ctx := context.Background()
	n := NewJiraNormalizer()

	if n.Source() != "jira" {
		t.Fatalf("Source() = %q, want \"jira\"", n.Source())
	}

	tests := []struct {
		name         string
		input        string
		wantContains []string
		wantMentions []Mention
	}{
		{
			name:         "empty",
			input:        "",
			wantContains: []string{},
		},
		{
			name:         "PAY-2128 real fixture",
			input:        `{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"Tabby authorizations failing due to missing installments_count in API response"}]}]}`,
			wantContains: []string{"Tabby authorizations failing"},
		},
		{
			name: "simple paragraph with bold and link",
			input: `{"type":"doc","content":[{"type":"paragraph","content":[
				{"type":"text","text":"Hello ","marks":[]},
				{"type":"text","text":"world","marks":[{"type":"strong"}]},
				{"type":"text","text":" see ","marks":[]},
				{"type":"text","text":"docs","marks":[{"type":"link","attrs":{"href":"https://example.com"}}]}
			]}]}`,
			wantContains: []string{"Hello world", "docs (https://example.com)"},
		},
		{
			name:         "codeBlock",
			input:        `{"type":"doc","content":[{"type":"codeBlock","content":[{"type":"text","text":"fmt.Println(\"hi\")"}]}]}`,
			wantContains: []string{"```", "fmt.Println"},
		},
		{
			name: "mention",
			input: `{"type":"doc","content":[{"type":"paragraph","content":[
				{"type":"mention","attrs":{"id":"5e7b8c1a2d3f4e5","text":"Jane Doe"}}
			]}]}`,
			wantContains: []string{"@Jane Doe"},
			wantMentions: []Mention{
				{Source: "jira", ExternalID: "5e7b8c1a2d3f4e5", DisplayName: "Jane Doe"},
			},
		},
		{
			name:         "malformed JSON falls back to raw",
			input:        "not json at all",
			wantContains: []string{"not json at all"},
		},
		{
			name: "bullet list",
			input: `{"type":"doc","content":[{"type":"bulletList","content":[
				{"type":"listItem","content":[{"type":"paragraph","content":[{"type":"text","text":"item one"}]}]},
				{"type":"listItem","content":[{"type":"paragraph","content":[{"type":"text","text":"item two"}]}]}
			]}]}`,
			wantContains: []string{"- item one", "- item two"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, err := n.Normalize(ctx, []byte(tc.input), nil)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			for _, want := range tc.wantContains {
				if !strings.Contains(res.Text, want) {
					t.Errorf("Text missing %q\n  got: %q", want, res.Text)
				}
			}
			if len(res.Mentions) != len(tc.wantMentions) {
				t.Errorf("Mentions count: got %d, want %d", len(res.Mentions), len(tc.wantMentions))
				return
			}
			for i, m := range res.Mentions {
				wm := tc.wantMentions[i]
				if m.Source != wm.Source || m.ExternalID != wm.ExternalID || m.DisplayName != wm.DisplayName {
					t.Errorf("Mention[%d]: got %+v, want %+v", i, m, wm)
				}
			}
		})
	}
}

func TestJiraNormalizer_IssuePayloadLiftsMetadata(t *testing.T) {
	n := NewJiraNormalizer()
	input := `{"key":"PAY-2307","fields":{
		"summary":"India GST",
		"description":{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"GST invoice body"}]}]},
		"status":{"name":"In Progress"},
		"issuetype":{"name":"Epic"},
		"created":"2026-01-02T03:04:05.000+0000",
		"updated":"2026-02-03T04:05:06.000+0000",
		"resolutiondate":null,
		"labels":["gst","india"],
		"assignee":{"accountId":"acc-9"},
		"parent":null
	}}`
	res, err := n.Normalize(context.Background(), []byte(input), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Text != "GST invoice body" {
		t.Errorf("Text = %q", res.Text)
	}
	want := map[string]any{
		"status":              "In Progress",
		"issuetype":           "Epic",
		"created":             "2026-01-02T03:04:05.000+0000",
		"updated":             "2026-02-03T04:05:06.000+0000",
		"resolutiondate":      nil,
		"assignee_account_id": "acc-9",
		"parent_key":          nil,
	}
	for k, v := range want {
		got, ok := res.Metadata[k]
		if !ok {
			t.Errorf("Metadata missing key %q (absent values must be present as nil)", k)
			continue
		}
		if got != v {
			t.Errorf("Metadata[%q] = %#v, want %#v", k, got, v)
		}
	}
	if labels, _ := res.Metadata["labels"].([]string); strings.Join(labels, ",") != "gst,india" {
		t.Errorf("labels = %#v", res.Metadata["labels"])
	}

	// Issue with a null description still yields metadata and no text.
	res, err = n.Normalize(context.Background(), []byte(`{"key":"PAY-1","fields":{"description":null,"status":{"name":"Done"},"parent":{"key":"PAY-2307"}}}`), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Text != "" || res.Metadata["status"] != "Done" || res.Metadata["parent_key"] != "PAY-2307" {
		t.Errorf("null description: text=%q meta=%#v", res.Text, res.Metadata)
	}

	// Bare ADF (legacy shape) yields no metadata.
	res, _ = n.Normalize(context.Background(), []byte(`{"type":"doc","content":[]}`), nil)
	if res.Metadata != nil {
		t.Errorf("bare ADF metadata = %#v, want nil", res.Metadata)
	}
}

func TestJiraNormalize_CodeBlockThenParagraph(t *testing.T) {
	raw := `{"type":"doc","content":[{"type":"codeBlock","content":[{"type":"text","text":"code"}]},{"type":"paragraph","content":[{"type":"text","text":"--- comment by Jane @ now ---"}]}]}`
	res, err := NewJiraNormalizer().Normalize(context.Background(), []byte(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "```code```\n\n--- comment by Jane @ now ---" {
		t.Fatalf("unexpected text: %q", res.Text)
	}
}

func TestJiraNormalize_BlockAndEmbedCard(t *testing.T) {
	raw := `{"type":"doc","content":[{"type":"blockCard","attrs":{"url":"https://wego.slack.com/archives/C04U4KATYUV/p1787303769925489"}},{"type":"embedCard","attrs":{"url":"https://github.com/wego/payments/pull/123"}}]}`
	ctx := context.Background()
	res, err := NewJiraNormalizer().Normalize(ctx, []byte(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	found, err := extractor.New(nil, zerolog.Nop()).Extract(ctx, res.Text)
	if err != nil {
		t.Fatal(err)
	}
	nodes := make(map[string]bool)
	for _, finding := range found.Findings {
		nodes[finding.NodeID] = true
	}
	for _, id := range []string{"slack:C04U4KATYUV:1787303769.925489", "gh_pr:wego/payments#123"} {
		if !nodes[id] {
			t.Errorf("missing %s in findings %+v from %q", id, found.Findings, res.Text)
		}
	}
}
