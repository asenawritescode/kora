package mcp

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/internal/api/ai"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// connectMetrics serves a server built with options and returns a client connected to it.
func connectMetrics(t *testing.T, opts ...Option) *sdkmcp.ClientSession {
	t.Helper()
	server := NewWithExecutor(registryWithTask(), "tenant.example", ModeValidationOnly, nil, opts...)
	clientTransport, serverTransport := sdkmcp.NewInMemoryTransports()
	serverSession, err := server.srv.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatal("connect MCP server:", err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	client := sdkmcp.NewClient(&sdkmcp.Implementation{Name: "test-client", Version: "1"}, nil)
	clientSession, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatal("connect MCP client:", err)
	}
	t.Cleanup(func() { _ = clientSession.Close() })
	return clientSession
}

// recordingRunner answers each metrics route from its own body and records the routes it was asked for. A route
// without an answer is refused.
type recordingRunner struct {
	routes  []ai.MetricsRoute
	answers map[ai.MetricsRoute]ai.MetricsResponse
}

func (r *recordingRunner) run(route ai.MetricsRoute, _ []byte) ai.MetricsResponse {
	r.routes = append(r.routes, route)
	if resp, ok := r.answers[route]; ok {
		return resp
	}
	return ai.MetricsResponse{Status: 404, Body: []byte(`{"error":{"message":"no answer"}}`)}
}

func TestMetricsToolsArePublishedOnlyWithTheOption(t *testing.T) {
	cases := []struct {
		name string
		opts []Option
		want bool
	}{
		{"without the option", nil, false},
		{"with the option", []Option{WithMetrics(func(context.Context) (ai.MetricsRunner, error) { return nil, nil })}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := connectMetrics(t, tc.opts...).ListTools(context.Background(), nil)
			if err != nil {
				t.Fatal("list tools:", err)
			}
			got := map[string]bool{}
			for _, tool := range result.Tools {
				got[tool.Name] = true
			}
			for _, name := range []string{"list_metrics", "query_metrics", "drill_metrics"} {
				if got[name] != tc.want {
					t.Errorf("tool %q published = %v, want %v", name, got[name], tc.want)
				}
			}
			if got["ask_choice"] {
				t.Error("ask_choice is a chat-only tool and must not be published")
			}
		})
	}
}

func TestMetricsToolCallsRunThroughTheCallersRunner(t *testing.T) {
	body := `{"data":{"metrics":["sales"],"rows":[]},"meta":{}}`
	ok := ai.MetricsResponse{Status: 200, Body: []byte(body)}
	runner := &recordingRunner{answers: map[ai.MetricsRoute]ai.MetricsResponse{
		ai.MetricsRouteQuery: ok, ai.MetricsRouteMetrics: {Status: 200, Body: []byte(`{"data":[]}`)}, ai.MetricsRouteExplore: {Status: 200, Body: []byte(`{"data":[]}`)}}}
	var factoryCalls int
	session := connectMetrics(t, WithMetrics(func(context.Context) (ai.MetricsRunner, error) {
		factoryCalls++
		return runner.run, nil
	}))
	result, err := session.CallTool(context.Background(), &sdkmcp.CallToolParams{Name: "query_metrics",
		Arguments: map[string]any{"metrics": []any{"sales"}, "grain": "range"}})
	if err != nil {
		t.Fatal("call query_metrics:", err)
	}
	if text := resultText(t, result); text != body {
		t.Fatalf("query_metrics text = %q, want the runner's answer", text)
	}
	if factoryCalls != 1 || !slicesContainsRoute(runner.routes, ai.MetricsRouteQuery) {
		t.Fatalf("factory calls %d, routes %v; want one query through the runner", factoryCalls, runner.routes)
	}
}

func TestMetricsToolRefusalIsTheChatsErrorText(t *testing.T) {
	runner := &recordingRunner{answers: map[ai.MetricsRoute]ai.MetricsResponse{ai.MetricsRouteDrill: {Status: 403, Body: []byte(`{"error":{"code":"metrics.forbidden","message":"metrics are not available to field-assigned grants"}}`)}}}
	session := connectMetrics(t, WithMetrics(func(context.Context) (ai.MetricsRunner, error) { return runner.run, nil }))
	result, err := session.CallTool(context.Background(), &sdkmcp.CallToolParams{Name: "drill_metrics",
		Arguments: map[string]any{"metric": "sales", "from": "2026-11-09", "to": "2026-11-15"}})
	if err != nil {
		t.Fatal("call drill_metrics:", err)
	}
	if text := resultText(t, result); text != "Error: metrics are not available to field-assigned grants" {
		t.Fatalf("refusal = %q", text)
	}
}

func TestMetricsToolFailsWhenTheCallerHasNoRunner(t *testing.T) {
	session := connectMetrics(t, WithMetrics(func(context.Context) (ai.MetricsRunner, error) {
		return nil, errors.New("no verified caller")
	}))
	result, err := session.CallTool(context.Background(), &sdkmcp.CallToolParams{Name: "list_metrics", Arguments: map[string]any{}})
	if err == nil && (result == nil || !result.IsError) {
		t.Fatal("list_metrics without a caller succeeded")
	}
	if err != nil && !strings.Contains(err.Error(), "no verified caller") {
		t.Fatalf("error = %v, want the factory's refusal", err)
	}
}

// resultText is the text of a tool result's first content item.
func resultText(t *testing.T, result *sdkmcp.CallToolResult) string {
	t.Helper()
	if len(result.Content) == 0 {
		t.Fatal("tool result has no content")
	}
	text, ok := result.Content[0].(*sdkmcp.TextContent)
	if !ok {
		t.Fatalf("content = %T, want text", result.Content[0])
	}
	return text.Text
}

func slicesContainsRoute(routes []ai.MetricsRoute, want ai.MetricsRoute) bool {
	for _, r := range routes {
		if r == want {
			return true
		}
	}
	return false
}
