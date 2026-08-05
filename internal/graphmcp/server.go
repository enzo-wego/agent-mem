package graphmcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// GraphClient is the worker API surface exposed through MCP.
type GraphClient interface {
	Search(context.Context, string, []string, int) (map[string]any, error)
	Node(context.Context, string, string) (map[string]any, error)
	Neighbors(context.Context, string, int, []string) (map[string]any, error)
	ClusterSummary(context.Context, string, int) (map[string]any, error)
	Resolve(context.Context, ResolveRequest) (map[string]any, error)
}

type SearchInput struct {
	Q     string   `json:"q" jsonschema:"Natural-language or keyword query to search for"`
	Types []string `json:"types,omitempty" jsonschema:"Optional graph node types to include"`
	Limit int      `json:"limit,omitempty" jsonschema:"Maximum results, from 1 to 50; defaults to 10"`
}

type NodeInput struct {
	ID  string `json:"id,omitempty" jsonschema:"Canonical graph node ID"`
	URL string `json:"url,omitempty" jsonschema:"Exact source URL stored on a graph node"`
}

type NeighborsInput struct {
	ID    string   `json:"id" jsonschema:"Canonical graph node ID to traverse from"`
	Depth int      `json:"depth,omitempty" jsonschema:"Traversal depth from 1 to 3; defaults to 1"`
	Kinds []string `json:"kinds,omitempty" jsonschema:"Optional edge kinds to follow"`
}

type ClusterSummaryInput struct {
	Node  string `json:"node" jsonschema:"Canonical graph node ID at the center of the cluster"`
	Depth int    `json:"depth,omitempty" jsonschema:"Summary depth from 1 to 3; defaults to 1"`
}

type ResolveInput struct {
	Seeds         []string `json:"seeds" jsonschema:"One to twenty graph node IDs or exact source URLs"`
	Query         string   `json:"query" jsonschema:"Question the resolved context should answer"`
	Depth         int      `json:"depth,omitempty" jsonschema:"Traversal depth from 1 to 3; defaults to 2"`
	BudgetTokens  int      `json:"budget_tokens,omitempty" jsonschema:"Approximate context budget from 500 to 16000 tokens; defaults to 4000"`
	IncludeBodies *bool    `json:"include_bodies,omitempty" jsonschema:"Include artifact bodies; defaults to true"`
}

// NewServer creates an MCP server exposing the worker's graph read APIs.
func NewServer(client GraphClient, version string) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{
		Name:    "agent-mem-graph",
		Version: version,
	}, nil)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "graph_search",
		Description: "Search graph artifacts such as Slack threads, Jira issues, pull requests, and documents.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input SearchInput) (*mcp.CallToolResult, map[string]any, error) {
		input.Q = strings.TrimSpace(input.Q)
		if input.Q == "" {
			return nil, nil, fmt.Errorf("q must not be empty")
		}
		if input.Limit == 0 {
			input.Limit = 10
		}
		if input.Limit < 1 || input.Limit > 50 {
			return nil, nil, fmt.Errorf("limit must be between 1 and 50")
		}
		output, err := client.Search(ctx, input.Q, input.Types, input.Limit)
		return nil, output, err
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "graph_node",
		Description: "Fetch one graph artifact by its canonical node ID or exact source URL.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input NodeInput) (*mcp.CallToolResult, map[string]any, error) {
		input.ID = strings.TrimSpace(input.ID)
		input.URL = strings.TrimSpace(input.URL)
		if (input.ID == "") == (input.URL == "") {
			return nil, nil, fmt.Errorf("provide exactly one of id or url")
		}
		output, err := client.Node(ctx, input.ID, input.URL)
		return nil, output, err
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "graph_neighbors",
		Description: "Traverse related graph artifacts from a canonical node ID, optionally filtering edge kinds. Attached files and Jira attachments are always included as leaves, regardless of depth.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input NeighborsInput) (*mcp.CallToolResult, map[string]any, error) {
		input.ID = strings.TrimSpace(input.ID)
		if input.ID == "" {
			return nil, nil, fmt.Errorf("id must not be empty")
		}
		depth, err := validatedDepth(input.Depth, 1)
		if err != nil {
			return nil, nil, err
		}
		output, err := client.Neighbors(ctx, input.ID, depth, input.Kinds)
		return nil, output, err
	})

	mcp.AddTool(server, &mcp.Tool{
		Name: "graph_cluster_summary",
		Description: "Generate a concise summary of the artifact cluster around a graph node. " +
			"This invokes or reads LLM synthesis, can take roughly 15 seconds, and can return tens of kilobytes.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input ClusterSummaryInput) (*mcp.CallToolResult, map[string]any, error) {
		input.Node = strings.TrimSpace(input.Node)
		if input.Node == "" {
			return nil, nil, fmt.Errorf("node must not be empty")
		}
		depth, err := validatedDepth(input.Depth, 2)
		if err != nil {
			return nil, nil, err
		}
		output, err := client.ClusterSummary(ctx, input.Node, depth)
		return nil, output, err
	})

	mcp.AddTool(server, &mcp.Tool{
		Name:        "graph_resolve",
		Description: "Resolve graph seeds into a bounded, question-focused context bundle with trace information.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, input ResolveInput) (*mcp.CallToolResult, map[string]any, error) {
		if len(input.Seeds) < 1 || len(input.Seeds) > 20 {
			return nil, nil, fmt.Errorf("seeds must contain between 1 and 20 items")
		}
		for index, seed := range input.Seeds {
			input.Seeds[index] = strings.TrimSpace(seed)
			if input.Seeds[index] == "" {
				return nil, nil, fmt.Errorf("seeds must not contain empty values")
			}
		}
		input.Query = strings.TrimSpace(input.Query)
		if input.Query == "" {
			return nil, nil, fmt.Errorf("query must not be empty")
		}
		depth, err := validatedDepth(input.Depth, 2)
		if err != nil {
			return nil, nil, err
		}
		if input.BudgetTokens == 0 {
			input.BudgetTokens = 4000
		}
		if input.BudgetTokens < 500 || input.BudgetTokens > 16000 {
			return nil, nil, fmt.Errorf("budget_tokens must be between 500 and 16000")
		}
		includeBodies := true
		if input.IncludeBodies != nil {
			includeBodies = *input.IncludeBodies
		}
		output, err := client.Resolve(ctx, ResolveRequest{
			Seeds:         input.Seeds,
			Query:         input.Query,
			Depth:         depth,
			BudgetTokens:  input.BudgetTokens,
			IncludeBodies: includeBodies,
		})
		return nil, output, err
	})

	return server
}

func validatedDepth(value, defaultValue int) (int, error) {
	if value == 0 {
		return defaultValue, nil
	}
	if value < 1 || value > 3 {
		return 0, fmt.Errorf("depth must be between 1 and 3")
	}
	return value, nil
}
