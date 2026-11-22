// Package mcp provides a Model Context Protocol server that auto-generates
// tools from Kigae's doctype registry. Supports stdio (for Claude Desktop) and
// HTTP (embedded in kora serve) transports.
package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/asenawritescode/kora/internal/api/ai"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type Mode string

const (
	ModeValidationOnly Mode = "validation_only"
	ModeExecutable     Mode = "executable"
	ModeReadOnly       Mode = "read_only"
)

// Server wraps the MCP server with Kigae registry awareness.
type Server struct {
	srv      *mcp.Server
	registry *doctype.Registry
	mode     Mode
	executor VerifiedToolExecutor
	metrics  MetricsFactory
	insights InsightsGate
}

// MetricsFactory returns the runner of the metrics tools for one call. The runner must act as the caller the call
// authenticated as, so the reads apply the same grants, scope and business clock as the API.
type MetricsFactory func(ctx context.Context) (ai.MetricsRunner, error)

// InsightsGate reports whether the authenticated identity of the call may use Insights, by the rule the HTTP routes
// apply. An error means the identity could not be established, which denies it.
type InsightsGate func(ctx context.Context) (bool, error)

// ErrInsightsForbidden refuses a metrics call from an identity whose roles do not grant Insights.
var ErrInsightsForbidden = errors.New("mcp: Insights is not available for this identity")

// Option configures a Server at construction.
type Option func(*Server)

// WithMetrics adds the metrics read tools (list_metrics, query_metrics, drill_metrics). Each call takes its runner from
// the factory and is admitted by the gate first. The tools are listed only while the gate admits the identity, and a
// call by name is refused with ErrInsightsForbidden when it does not.
func WithMetrics(factory MetricsFactory, gate InsightsGate) Option {
	return func(s *Server) {
		s.metrics = factory
		s.insights = gate
	}
}

// VerifiedToolExecutor is supplied only by an adapter that authenticates the
// MCP caller, constructs policy.Execution, and delegates every tool operation
// to the same authorized domain operation used by other transports.
type VerifiedToolExecutor func(context.Context, ai.ToolDescriptor, map[string]any) (string, error)

// New creates an MCP server. Executable tenant tools require NewWithExecutor.
func New(reg *doctype.Registry, siteName string, mode Mode) *Server {
	return NewWithExecutor(reg, siteName, mode, nil)
}

// NewWithExecutor enables tenant tools only when the caller provides the
// authenticated execution adapter. Without it, executable mode cannot start.
func NewWithExecutor(reg *doctype.Registry, siteName string, mode Mode, executor VerifiedToolExecutor, opts ...Option) *Server {
	if mode == "" {
		mode = ModeValidationOnly
	}
	srv := mcp.NewServer(&mcp.Implementation{
		Name:    "kora-" + siteName,
		Version: "1.0.0",
	}, nil)

	ks := &Server{srv: srv, registry: reg, mode: mode, executor: executor}
	for _, opt := range opts {
		opt(ks)
	}
	ks.registerTools()
	return ks
}

// NewReadOnlyWithExecutor registers only generated DocType find/list/get
// tools. The executor must authenticate every call and enforce current tenant
// grants. Mutating and system tools are deliberately absent from this mode.
func NewReadOnlyWithExecutor(reg *doctype.Registry, siteName string, executor VerifiedToolExecutor, opts ...Option) *Server {
	return NewWithExecutor(reg, siteName, ModeReadOnly, executor, opts...)
}

// Run starts the MCP server on stdio transport (for Claude Desktop).
func (s *Server) Run(ctx context.Context) error {
	if (s.mode == ModeExecutable || s.mode == ModeReadOnly) && s.executor == nil {
		return fmt.Errorf("executable MCP mode requires an authenticated verified tool executor")
	}
	return s.srv.Run(ctx, &mcp.StdioTransport{})
}

func (s *Server) registerTools() {
	// Config generation tools.
	s.addConfigTools()
	if s.metrics != nil && s.listsMetrics() {
		for _, tool := range ai.MetricsToolDescriptors() {
			s.addMetricsTool(tool)
		}
	}

	if s.mode != ModeExecutable && s.mode != ModeReadOnly {
		return
	}
	if s.executor == nil {
		return
	}

	catalog := ai.BuildToolCatalog(s.registry, ai.CatalogOptions{})
	for _, tool := range catalog.Tools {
		if tool.Source != "tenant" {
			continue
		}
		if s.mode == ModeReadOnly && !isReadOperation(tool.Operation) {
			continue
		}
		s.addProjectedTool(tool)
	}
}

// listsMetrics reports whether the identity may see the metrics tools. A missing gate or a gate that errs hides them.
func (s *Server) listsMetrics() bool {
	if s.insights == nil {
		return false
	}
	ok, err := s.insights(context.Background())
	return err == nil && ok
}

// metricsRunner admits one metrics call through the gate, then takes the runner of the caller. A denied identity gets
// an error even when it calls the tool by name.
func (s *Server) metricsRunner(ctx context.Context) (ai.MetricsRunner, error) {
	if s.insights == nil {
		return nil, ErrInsightsForbidden
	}
	ok, err := s.insights(ctx)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrInsightsForbidden
	}
	return s.metrics(ctx)
}

func isReadOperation(operation string) bool {
	switch operation {
	case "find", "list", "get", "describe":
		return true
	default:
		return false
	}
}

func (s *Server) addConfigTools() {
	mcp.AddTool(s.srv, &mcp.Tool{
		Name:        "validate_yaml",
		Description: "Validate a Kigae YAML configuration. Returns syntax errors with line numbers and 'did you mean?' suggestions.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"yaml": map[string]any{"type": "string", "description": "YAML content to validate"},
			},
			"required": []string{"yaml"},
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, args struct {
		Yaml string `json:"yaml"`
	}) (*mcp.CallToolResult, any, error) {
		errs, _, err := doctype.ValidateYAML([]byte(args.Yaml))
		if err != nil {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "Validation error: " + err.Error()}},
			}, nil, nil
		}
		if len(errs) == 0 {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "✓ YAML is valid"}},
			}, nil, nil
		}
		var lines []string
		for _, e := range errs {
			line := fmt.Sprintf("Line %d: %s", e.Line, e.Message)
			if e.Detail != "" {
				line += " (" + e.Detail + ")"
			}
			lines = append(lines, line)
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: strings.Join(lines, "\n")}},
		}, nil, nil
	})
}

func (s *Server) addProjectedTool(tool ai.ToolDescriptor) {
	mcpTool := &mcp.Tool{
		Name:        tool.Name,
		Description: tool.Description,
		InputSchema: tool.InputSchema,
	}
	mcp.AddTool(s.srv, mcpTool, func(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
		result, err := s.executor(ctx, tool, args)
		if err != nil {
			return nil, nil, err
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: result}}}, nil, nil
	})
}

// addMetricsTool publishes one metrics read. The call runs through the same runner the chat uses, so a refusal is
// the same "Error:" text.
func (s *Server) addMetricsTool(tool ai.ToolDescriptor) {
	mcp.AddTool(s.srv, &mcp.Tool{Name: tool.Name, Description: tool.Description, InputSchema: tool.InputSchema},
		func(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
			run, err := s.metricsRunner(ctx)
			if err != nil {
				return nil, nil, err
			}
			text := ai.RunMetricsTool(run, tool.Name, args)
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil, nil
		})
}
