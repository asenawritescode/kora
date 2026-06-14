package api

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/yourorg/kora/doctype"
	"github.com/yourorg/kora/orm"
	"github.com/yourorg/kora/secret"
)

// ChatRequest is the JSON body for POST /api/chat.
type ChatRequest struct {
	Message string        `json:"message"`
	History []ChatMessage `json:"history,omitempty"`
	Model   string        `json:"model,omitempty"` // override default model
}

// ChatMessage is a single turn in the conversation.
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatResponse is the JSON response from POST /api/chat.
type ChatResponse struct {
	Reply  string `json:"reply"`
	Action string `json:"action,omitempty"` // what the AI did (e.g., "listed 3 customers")
}

// HandleChat processes a chat message, calls the AI provider with function definitions,
// executes any tool calls via the ORM, and returns the AI's response.
// POST /api/chat
func (h *Handler) HandleChat(c *gin.Context) {
	var req ChatRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error: map[string]string{"message": "Invalid request format"},
		})
		return
	}

	reg := h.siteRegistry(c)
	siteName, _ := c.Get("site_name")
	tx := h.siteTx(c)

	// Read the configured AI provider key.
	_, apiKey, baseURL, model := resolveProvider(tx.DB, siteName.(string), req.Model)
	if apiKey == "" {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error: map[string]string{"message": "No AI provider configured. Run: ./kora secret set --site " + siteName.(string) + " --key openai_api_key --value sk-..."},
		})
		return
	}

	// Build function definitions from the registry.
	functions := buildFunctions(reg)

	// Build messages array.
	messages := make([]map[string]any, 0, len(req.History)+2)
	for _, h := range req.History {
		messages = append(messages, map[string]any{"role": h.Role, "content": h.Content})
	}
	messages = append(messages, map[string]any{"role": "user", "content": req.Message})

	// Call AI provider.
	aiBody := map[string]any{
		"model":    model,
		"messages": messages,
	}
	if len(functions) > 0 {
		aiBody["tools"] = functions
		aiBody["tool_choice"] = "auto"
	}

	aiResp, err := callAI(baseURL, apiKey, aiBody)
	if err != nil {
		slog.Error("AI provider call failed", "error", err)
		c.JSON(http.StatusInternalServerError, ErrorResponse{
			Error: map[string]string{"message": "AI provider error: " + err.Error()},
		})
		return
	}

	// Process the AI response.
	choice := aiResp["choices"].([]any)[0].(map[string]any)
	msg := choice["message"].(map[string]any)

	// Check for tool calls.
	if toolCalls, ok := msg["tool_calls"]; ok {
		action, reply := executeToolCalls(tx, reg, toolCalls.([]any))
		c.JSON(http.StatusOK, ChatResponse{Reply: reply, Action: action})
		return
	}

	// Plain text response.
	c.JSON(http.StatusOK, ChatResponse{
		Reply: msg["content"].(string),
	})
}

func resolveProvider(db *sql.DB, siteName, modelOverride string) (provider, apiKey, baseURL, model string) {
	store := secret.NewStore(db)

	// Try each provider in order.
	providers := []struct{ key, base, defaultModel string }{
		{"openai_api_key", "https://api.openai.com/v1", "gpt-4o"},
		{"deepseek_api_key", "https://api.deepseek.com", "deepseek-v4-pro"},
		{"anthropic_api_key", "https://api.anthropic.com/v1", "claude-sonnet-4-6"},
	}
	for _, p := range providers {
		if k, err := store.Get(siteName, p.key); err == nil && k != "" {
			m := p.defaultModel
			if modelOverride != "" {
				m = modelOverride
			}
			return p.key, k, p.base, m
		}
	}
	return "", "", "", ""
}

func buildFunctions(reg *doctype.Registry) []map[string]any {
	var funcs []map[string]any
	for _, dt := range reg.All() {
		if dt.IsChildTable {
			continue
		}
		lower := sanitizeName(dt.Name)
		props := make(map[string]any)
		required := []string{}
		for _, f := range dt.DataFields() {
			if f.Fieldtype == "Table" {
				continue
			}
			s := map[string]any{"description": f.Label}
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
			props[f.Fieldname] = s
			if f.Reqd {
				required = append(required, f.Fieldname)
			}
		}

		funcs = append(funcs, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        lower + "_list",
				"description": "List " + dt.Name + " documents",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"limit":  map[string]any{"type": "integer"},
						"offset": map[string]any{"type": "integer"},
					},
				},
			},
		}, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        lower + "_get",
				"description": "Get a single " + dt.Name + " by name",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"name": map[string]any{"type": "string", "description": "Document name"},
					},
					"required": []string{"name"},
				},
			},
		}, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        lower + "_create",
				"description": "Create a new " + dt.Name,
				"parameters": map[string]any{
					"type":       "object",
					"properties": props,
					"required":   required,
				},
			},
		})
	}
	return funcs
}

func callAI(baseURL, apiKey string, body map[string]any) (map[string]any, error) {
	jsonBody, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", baseURL+"/chat/completions", bytes.NewReader(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("AI provider returned %d: %s", resp.StatusCode, string(respBody))
	}

	var result map[string]any
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("parsing AI response: %w", err)
	}
	return result, nil
}

func executeToolCalls(db *orm.TxManager, reg *doctype.Registry, toolCalls []any) (action string, reply string) {
	parts := make([]string, len(toolCalls))
	for i, tc := range toolCalls {
		call := tc.(map[string]any)
		fn := call["function"].(map[string]any)
		name := fn["name"].(string)
		argsJSON := fn["arguments"].(string)

		var args map[string]any
		json.Unmarshal([]byte(argsJSON), &args)

		parts[i] = executeSingleTool(db, reg, name, args)
	}
	return strings.Join(parts, "; "), strings.Join(parts, "\n")
}

func executeSingleTool(tx *orm.TxManager, reg *doctype.Registry, toolName string, args map[string]any) string {
	parts := strings.SplitN(toolName, "_", 2)
	if len(parts) != 2 {
		return fmt.Sprintf("Unknown tool: %s", toolName)
	}
	doctypeName := parts[0]
	operation := parts[1]

	// Find the doctype (case-insensitive).
	var dt *doctype.DocType
	for _, d := range reg.All() {
		if strings.EqualFold(d.Name, doctypeName) {
			dt = d
			break
		}
	}
	if dt == nil {
		return fmt.Sprintf("DocType %q not found", doctypeName)
	}

	switch operation {
	case "list":
		limit := 20
		if v, ok := args["limit"].(float64); ok {
			limit = int(v)
		}
		docs, total, err := tx.GetList(dt, "", "", limit, 0, "")
		if err != nil {
			return fmt.Sprintf("Error listing %s: %v", dt.Name, err)
		}
		if total == 0 {
			return fmt.Sprintf("No %s found.", dt.Name)
		}
		// Build a markdown table for clean presentation.
		var cols []string
		var colLabels []string
		for _, f := range dt.DataFields() {
			if f.Fieldtype == "Table" || f.Fieldtype == "Section Break" || f.Fieldtype == "Column Break" || f.Fieldtype == "Heading" {
				continue
			}
			cols = append(cols, f.Fieldname)
			colLabels = append(colLabels, f.Label)
		}
		var lines []string
		lines = append(lines, fmt.Sprintf("**%d %s found:**", total, dt.Name))
		lines = append(lines, "")
		// Header row.
		lines = append(lines, "| "+strings.Join(colLabels, " | ")+" |")
		// Separator.
		var seps []string
		for range cols {
			seps = append(seps, "---")
		}
		lines = append(lines, "| "+strings.Join(seps, " | ")+" |")
		// Data rows.
		for _, doc := range docs {
			var vals []string
			for _, col := range cols {
				v := doc.Get(col)
				vals = append(vals, formatCell(col, v))
			}
			lines = append(lines, "| "+strings.Join(vals, " | ")+" |")
		}
		lines = append(lines, "")
		return strings.Join(lines, "\n")
	case "get":
		name, _ := args["name"].(string)
		doc, err := tx.GetDoc(dt, name, "")
		if err != nil {
			return fmt.Sprintf("%s %q not found.", dt.Name, name)
		}
		return fmt.Sprintf("%s %q: %v", dt.Name, name, doc.Fields)
	case "create":
		doc := doctype.NewDocument(dt.Name)
		for k, v := range args {
			doc.Set(k, v)
		}
		if err := tx.Insert(dt, doc, "mcp-agent"); err != nil {
			return fmt.Sprintf("Error creating %s: %v", dt.Name, err)
		}
		return fmt.Sprintf("Created %s %q.", dt.Name, doc.Name)
	default:
		return fmt.Sprintf("Unknown operation: %s", operation)
	}
}

func sanitizeName(name string) string {
	s := strings.ToLower(name)
	s = strings.ReplaceAll(s, " ", "_")
	s = strings.ReplaceAll(s, "-", "_")
	return s
}

func formatCell(fieldname string, v any) string {
	if v == nil {
		return ""
	}
	s := fmt.Sprintf("%v", v)
	// Format Known decimal fields to 2 places.
	if strings.Contains(s, ".") && len(s) > 4 {
		if n, err := strconv.ParseFloat(s, 64); err == nil {
			return fmt.Sprintf("%.2f", n)
		}
	}
	// Boolean → emoji checkmark.
	if s == "1" && (strings.Contains(fieldname, "is_") || strings.Contains(fieldname, "available") ||
		fieldname == "completed") {
		return "✅"
	}
	if s == "0" && (strings.Contains(fieldname, "is_") || strings.Contains(fieldname, "available") ||
		fieldname == "completed") {
		return "❌"
	}
	return s
}
