// Package mcp provides a Model Context Protocol server that auto-generates
// tools from Kigae's doctype registry. Supports stdio (for Claude Desktop) and
// HTTP (embedded in kora serve) transports.
package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/asenawritescode/kora/api/ai"
	"github.com/asenawritescode/kora/doctype"
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
func NewWithExecutor(reg *doctype.Registry, siteName string, mode Mode, executor VerifiedToolExecutor) *Server {
	if mode == "" {
		mode = ModeValidationOnly
	}
	srv := mcp.NewServer(&mcp.Implementation{
		Name:    "kora-" + siteName,
		Version: "1.0.0",
	}, nil)

	ks := &Server{srv: srv, registry: reg, mode: mode, executor: executor}
	ks.registerTools()
	return ks
}

// NewReadOnlyWithExecutor registers only generated DocType find/list/get
// tools. The executor must authenticate every call and enforce current tenant
// grants. Mutating and system tools are deliberately absent from this mode.
func NewReadOnlyWithExecutor(reg *doctype.Registry, siteName string, executor VerifiedToolExecutor) *Server {
	return NewWithExecutor(reg, siteName, ModeReadOnly, executor)
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

	if s.mode != ModeExecutable && s.mode != ModeReadOnly {
		return
	}
	if s.executor == nil {
		return
	}

	catalog := ai.BuildToolCatalog(s.registry)
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

func isReadOperation(operation string) bool {
	switch operation {
	case "find", "list", "get":
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

func sanitizeName(name string) string {
	s := strings.ToLower(name)
	s = strings.ReplaceAll(s, " ", "_")
	s = strings.ReplaceAll(s, "-", "_")
	return s
}
