// Package mcp provides a Model Context Protocol server that auto-generates
// tools from Kora's doctype registry. Supports stdio (for Claude Desktop) and
// HTTP (embedded in kora serve) transports.
package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/asenawritescode/kora/doctype"
)

// Server wraps the MCP server with Kora registry awareness.
type Server struct {
	srv      *mcp.Server
	registry *doctype.Registry
}

// New creates a new MCP server populated with tools for all doctypes in the registry.
func New(reg *doctype.Registry, siteName string) *Server {
	srv := mcp.NewServer(&mcp.Implementation{
		Name:    "kora-" + siteName,
		Version: "1.0.0",
	}, nil)

	ks := &Server{srv: srv, registry: reg}
	ks.registerTools()
	return ks
}

// Run starts the MCP server on stdio transport (for Claude Desktop).
func (s *Server) Run(ctx context.Context) error {
	return s.srv.Run(ctx, &mcp.StdioTransport{})
}


func (s *Server) registerTools() {
	// Config generation tools.
	s.addConfigTools()

	// Per-doctype CRUD tools.
	for _, dt := range s.registry.All() {
		if dt.IsChildTable {
			continue
		}
		s.addDoctypeTools(dt)
	}
}

func (s *Server) addConfigTools() {
	mcp.AddTool(s.srv, &mcp.Tool{
		Name:        "validate_yaml",
		Description: "Validate a Kora YAML configuration. Returns syntax errors with line numbers and 'did you mean?' suggestions.",
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

func (s *Server) addDoctypeTools(dt *doctype.DocType) {
	lower := sanitizeName(dt.Name)
	props := buildFieldSchema(dt)

	// List tool.
	mcp.AddTool(s.srv, &mcp.Tool{
		Name:        lower + "_list",
		Description: "List all " + dt.Name + " documents",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"limit":   map[string]any{"type": "integer", "description": "Maximum results"},
				"offset":  map[string]any{"type": "integer", "description": "Pagination offset"},
				"order_by": map[string]any{"type": "string", "description": "Sort field and direction"},
				"filters":  map[string]any{"type": "string", "description": "JSON filter array"},
			},
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, args struct {
		Limit   int    `json:"limit"`
		Offset  int    `json:"offset"`
		OrderBy string `json:"order_by"`
		Filters string `json:"filters"`
	}) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{
				Text: fmt.Sprintf("Call GET /api/resource/%s with limit=%d offset=%d", dt.Name, args.Limit, args.Offset),
			}},
		}, nil, nil
	})

	// Create tool.
	mcp.AddTool(s.srv, &mcp.Tool{
		Name:        lower + "_create",
		Description: "Create a new " + dt.Name + " document",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": props,
			"required":   requiredFields(dt),
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{
				Text: fmt.Sprintf("Would call POST /api/resource/%s with fields: %v", dt.Name, args),
			}},
		}, nil, nil
	})

	// Get tool.
	mcp.AddTool(s.srv, &mcp.Tool{
		Name:        lower + "_get",
		Description: "Get a single " + dt.Name + " document by name",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{"type": "string", "description": "Document name"},
			},
			"required": []string{"name"},
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, args struct {
		Name string `json:"name"`
	}) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{
				Text: fmt.Sprintf("Would call GET /api/resource/%s/%s", dt.Name, args.Name),
			}},
		}, nil, nil
	})

	// Update tool.
	mcp.AddTool(s.srv, &mcp.Tool{
		Name:        lower + "_update",
		Description: "Update an existing " + dt.Name + " document",
		InputSchema: map[string]any{
			"type": "object",
			"properties": mergeMaps(map[string]any{
				"name": map[string]any{"type": "string", "description": "Document name to update"},
			}, props),
			"required": []string{"name"},
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{
				Text: fmt.Sprintf("Would call PUT /api/resource/%s/%s", dt.Name, args["name"]),
			}},
		}, nil, nil
	})

	// Delete tool.
	mcp.AddTool(s.srv, &mcp.Tool{
		Name:        lower + "_delete",
		Description: "Delete a " + dt.Name + " document by name",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": map[string]any{"type": "string", "description": "Document name to delete"},
			},
			"required": []string{"name"},
		},
	}, func(ctx context.Context, req *mcp.CallToolRequest, args struct {
		Name string `json:"name"`
	}) (*mcp.CallToolResult, any, error) {
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{
				Text: fmt.Sprintf("Would call DELETE /api/resource/%s/%s", dt.Name, args.Name),
			}},
		}, nil, nil
	})
}

func buildFieldSchema(dt *doctype.DocType) map[string]any {
	props := make(map[string]any)
	for _, f := range dt.DataFields() {
		if f.Fieldtype == "Table" {
			continue
		}
		s := map[string]any{}
		switch f.Fieldtype {
		case "Int":
			s["type"] = "integer"
		case "Float", "Currency", "Percent":
			s["type"] = "number"
		case "Check":
			s["type"] = "boolean"
		default:
			s["type"] = "string"
		}
		if f.Description != "" {
			s["description"] = f.Description
		}
		props[f.Fieldname] = s
	}
	return props
}

func requiredFields(dt *doctype.DocType) []string {
	var req []string
	for _, f := range dt.DataFields() {
		if f.Reqd && f.Fieldtype != "Table" {
			req = append(req, f.Fieldname)
		}
	}
	return req
}

func mergeMaps(a, b map[string]any) map[string]any {
	for k, v := range b {
		a[k] = v
	}
	return a
}

func sanitizeName(name string) string {
	s := strings.ToLower(name)
	s = strings.ReplaceAll(s, " ", "_")
	s = strings.ReplaceAll(s, "-", "_")
	return s
}
