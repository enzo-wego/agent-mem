package graphmcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync/atomic"
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
	resolve ResolveRequest
	search  SearchRequest
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

func (c *recordingGraphClient) Search(_ context.Context, req SearchRequest) (map[string]any, error) {
	return c.record(recordedCall{name: "graph_search", query: req.Q, types: req.Types, limit: req.Limit, search: req})
}

func (c *recordingGraphClient) Epic(_ context.Context, key string) (map[string]any, error) {
	return c.record(recordedCall{name: "graph_epic", id: key})
}

func (c *recordingGraphClient) Node(_ context.Context, id, rawURL string) (map[string]any, error) {
	return c.record(recordedCall{name: "graph_node", id: id, rawURL: rawURL})
}

func (c *recordingGraphClient) Neighbors(_ context.Context, id string, depth int, kinds []string) (map[string]any, error) {
	return c.record(recordedCall{name: "graph_neighbors", id: id, depth: depth, kinds: kinds})
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
		"graph_epic",
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
		{Name: "graph_resolve", Arguments: map[string]any{
			"seeds":    []string{"https://github.com/wego/payments/pull/2198"},
			"query":    "is WithRebateRepo safe to remove?",
			"epic":     []string{"PAY-2307"},
			"business": "payments",
		}},
		{Name: "graph_epic", Arguments: map[string]any{"key": "PAY-2307"}},
		{Name: "graph_search", Arguments: map[string]any{"q": "GST", "epic": []string{"PAY-2307"}, "business": "payments"}},
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

	if len(graphClient.calls) != 6 {
		t.Fatalf("worker calls = %d, want 6", len(graphClient.calls))
	}
	if got := graphClient.calls[0].search.Limit; got != 10 {
		t.Errorf("search limit = %d, want 10", got)
	}
	if got := graphClient.calls[2].depth; got != 1 {
		t.Errorf("neighbors depth = %d, want 1", got)
	}
	resolve := graphClient.calls[3].resolve
	if resolve.Depth != 2 || resolve.BudgetTokens != 4000 || !resolve.IncludeBodies {
		t.Errorf("resolve defaults = %#v", resolve)
	}
	if stringSlice(resolve.Epic) != stringSlice([]string{"PAY-2307"}) || resolve.Business != "payments" {
		t.Errorf("resolve scope not forwarded: %#v", resolve)
	}
	if epic := graphClient.calls[4]; epic.name != "graph_epic" || epic.id != "PAY-2307" {
		t.Errorf("epic call = %#v", epic)
	}
	if s := graphClient.calls[5].search; stringSlice(s.Epic) != stringSlice([]string{"PAY-2307"}) || s.Business != "payments" {
		t.Errorf("search scope not forwarded: %#v", s)
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
		{"graph_resolve", map[string]any{"seeds": []string{}, "query": "x"}},
		{"graph_epic", map[string]any{"key": " "}},
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

func (c *cancellationGraphClient) Search(ctx context.Context, _ SearchRequest) (map[string]any, error) {
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

func TestServer_RemovedToolsAreGone(t *testing.T) {
	session := connectTestClient(t, &recordingGraphClient{})
	for _, name := range []string{"graph_cluster_summary", "graph_person"} {
		result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: map[string]any{"q": "x", "node": "y"}})
		if err == nil && (result == nil || !result.IsError) {
			t.Errorf("removed tool %s still callable", name)
		}
	}
}

// Without AGENT_MEM_ASKER every graph tool returns a tool error and no HTTP
// request reaches the worker.
func TestServer_MissingAskerIsToolErrorWithoutHTTP(t *testing.T) {
	var hits atomic.Int32
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer worker.Close()
	client, err := NewClient(worker.URL, "secret", worker.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Probe(context.Background()); err != nil {
		t.Fatalf("Probe must work without an asker: %v", err)
	}
	hits.Store(0)
	session := connectTestClient(t, client)
	for _, call := range []*mcp.CallToolParams{
		{Name: "graph_search", Arguments: map[string]any{"q": "x"}},
		{Name: "graph_node", Arguments: map[string]any{"id": "jira:X-1"}},
		{Name: "graph_neighbors", Arguments: map[string]any{"id": "jira:X-1"}},
		{Name: "graph_resolve", Arguments: map[string]any{"seeds": []string{"jira:X-1"}, "query": "x"}},
		{Name: "graph_epic", Arguments: map[string]any{"key": "PAY-1"}},
	} {
		result, err := session.CallTool(context.Background(), call)
		if err != nil {
			t.Fatalf("%s protocol error: %v", call.Name, err)
		}
		text := ""
		if len(result.Content) > 0 {
			if tc, ok := result.Content[0].(*mcp.TextContent); ok {
				text = tc.Text
			}
		}
		if !result.IsError || !strings.Contains(text, "AGENT_MEM_ASKER") {
			t.Errorf("%s: want AGENT_MEM_ASKER tool error, got IsError=%v text=%q", call.Name, result.IsError, text)
		}
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("worker saw %d requests, want 0", got)
	}
}
