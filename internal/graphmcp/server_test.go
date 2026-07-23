package graphmcp

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type recordedCall struct {
	name    string
	query   string
	types   []string
	limit   int
	id      string
	rawURL  string
	depth   int
	kinds   []string
	node    string
	resolve ResolveRequest
}

type recordingGraphClient struct {
	calls []recordedCall
	err   error
}

func (c *recordingGraphClient) record(call recordedCall) (map[string]any, error) {
	c.calls = append(c.calls, call)
	if c.err != nil {
		return nil, c.err
	}
	return map[string]any{"tool": call.name, "ok": true}, nil
}

func (c *recordingGraphClient) Search(_ context.Context, query string, types []string, limit int) (map[string]any, error) {
	return c.record(recordedCall{name: "graph_search", query: query, types: types, limit: limit})
}

func (c *recordingGraphClient) Node(_ context.Context, id, rawURL string) (map[string]any, error) {
	return c.record(recordedCall{name: "graph_node", id: id, rawURL: rawURL})
}

func (c *recordingGraphClient) Neighbors(_ context.Context, id string, depth int, kinds []string) (map[string]any, error) {
	return c.record(recordedCall{name: "graph_neighbors", id: id, depth: depth, kinds: kinds})
}

func (c *recordingGraphClient) ClusterSummary(_ context.Context, node string, depth int) (map[string]any, error) {
	return c.record(recordedCall{name: "graph_cluster_summary", node: node, depth: depth})
}

func (c *recordingGraphClient) Resolve(_ context.Context, request ResolveRequest) (map[string]any, error) {
	return c.record(recordedCall{name: "graph_resolve", resolve: request})
}

func connectTestClient(t *testing.T, graphClient GraphClient) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := NewServer(graphClient, "test").Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("connect server: %v", err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "test"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("connect client: %v", err)
	}
	t.Cleanup(func() { _ = clientSession.Close() })
	return clientSession
}

func TestServer_ListsExactlyFiveGraphToolsWithSchemas(t *testing.T) {
	session := connectTestClient(t, &recordingGraphClient{})
	result, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}

	names := make([]string, 0, len(result.Tools))
	for _, tool := range result.Tools {
		names = append(names, tool.Name)
		schema, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatalf("%s schema: %v", tool.Name, err)
		}
		if len(schema) == 0 || string(schema) == "null" {
			t.Errorf("%s has no input schema", tool.Name)
		}
	}
	sort.Strings(names)
	want := []string{
		"graph_cluster_summary",
		"graph_neighbors",
		"graph_node",
		"graph_resolve",
		"graph_search",
	}
	if stringSlice(names) != stringSlice(want) {
		t.Fatalf("tools = %v, want %v", names, want)
	}
}

func TestServer_AppliesDefaultsAndForwardsEveryTool(t *testing.T) {
	graphClient := &recordingGraphClient{}
	session := connectTestClient(t, graphClient)
	ctx := context.Background()

	calls := []*mcp.CallToolParams{
		{Name: "graph_search", Arguments: map[string]any{"q": "TRY currency"}},
		{Name: "graph_node", Arguments: map[string]any{"id": "jira:PAY-2223"}},
		{Name: "graph_neighbors", Arguments: map[string]any{"id": "jira:PAY-2223"}},
		{Name: "graph_cluster_summary", Arguments: map[string]any{"node": "jira:PAY-2223"}},
		{Name: "graph_resolve", Arguments: map[string]any{
			"seeds": []string{"https://github.com/wego/payments/pull/2198"},
			"query": "is WithRebateRepo safe to remove?",
		}},
	}
	for _, call := range calls {
		result, err := session.CallTool(ctx, call)
		if err != nil {
			t.Fatalf("%s: %v", call.Name, err)
		}
		if result.IsError {
			t.Fatalf("%s returned tool error: %#v", call.Name, result.Content)
		}
		if result.StructuredContent == nil {
			t.Errorf("%s returned no structured content", call.Name)
		}
	}

	if len(graphClient.calls) != 5 {
		t.Fatalf("worker calls = %d, want 5", len(graphClient.calls))
	}
	if got := graphClient.calls[0].limit; got != 10 {
		t.Errorf("search limit = %d, want 10", got)
	}
	if got := graphClient.calls[2].depth; got != 1 {
		t.Errorf("neighbors depth = %d, want 1", got)
	}
	if got := graphClient.calls[3].depth; got != 2 {
		t.Errorf("cluster depth = %d, want 2", got)
	}
	resolve := graphClient.calls[4].resolve
	if resolve.Depth != 2 || resolve.BudgetTokens != 4000 || !resolve.IncludeBodies {
		t.Errorf("resolve defaults = %#v", resolve)
	}
}

func TestServer_ValidationAndWorkerErrorsBecomeToolErrors(t *testing.T) {
	graphClient := &recordingGraphClient{}
	session := connectTestClient(t, graphClient)

	invalid := []struct {
		name string
		args map[string]any
	}{
		{"graph_search", map[string]any{"q": " ", "limit": 51}},
		{"graph_node", map[string]any{"id": "jira:X", "url": "https://example.com"}},
		{"graph_neighbors", map[string]any{"id": "jira:X", "depth": 4}},
		{"graph_cluster_summary", map[string]any{"node": "jira:X", "depth": 4}},
		{"graph_resolve", map[string]any{"seeds": []string{}, "query": "x"}},
	}
	for _, test := range invalid {
		result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: test.name, Arguments: test.args})
		if err != nil {
			t.Fatalf("%s protocol error: %v", test.name, err)
		}
		if !result.IsError {
			t.Errorf("%s accepted invalid input %#v", test.name, test.args)
		}
	}

	graphClient.err = errors.New("worker unavailable")
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name:      "graph_search",
		Arguments: map[string]any{"q": "valid query"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Fatal("worker failure was not returned as a tool error")
	}
}

type cancellationGraphClient struct {
	*recordingGraphClient
	started chan struct{}
	done    chan struct{}
}

func (c *cancellationGraphClient) Search(ctx context.Context, _ string, _ []string, _ int) (map[string]any, error) {
	close(c.started)
	<-ctx.Done()
	close(c.done)
	return nil, ctx.Err()
}

func TestServer_CancellationReachesWorker(t *testing.T) {
	graphClient := &cancellationGraphClient{
		recordingGraphClient: &recordingGraphClient{},
		started:              make(chan struct{}),
		done:                 make(chan struct{}),
	}
	session := connectTestClient(t, graphClient)
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := session.CallTool(ctx, &mcp.CallToolParams{
			Name:      "graph_search",
			Arguments: map[string]any{"q": "cancel me"},
		})
		result <- err
	}()
	<-graphClient.started
	cancel()
	<-graphClient.done
	<-result
}

func stringSlice(values []string) string {
	encoded, _ := json.Marshal(values)
	return string(encoded)
}
