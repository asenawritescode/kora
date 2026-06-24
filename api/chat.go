package api

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gopkg.in/yaml.v3"

	"github.com/asenawritescode/kora/configstore"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/orm"
	"github.com/asenawritescode/kora/secret"
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
	siteNameRaw, _ := c.Get("site_name")
	siteName, _ := siteNameRaw.(string)
	tx := h.siteTx(c)

	// Read the configured AI provider key.
	_, apiKey, baseURL, model := resolveProvider(tx.DB, siteName, req.Model)
	if apiKey == "" {
		c.JSON(http.StatusBadRequest, ErrorResponse{
			Error: map[string]string{"message": "No AI provider configured. Go to /workspace/admin/secrets to add your API key (OpenAI, DeepSeek, or Anthropic)."},
		})
		return
	}

	// Load AI configuration (per-model defaults + site overrides).
	store := secret.NewStore(tx.DB)
	cfg := LoadAIConfig(store, siteName, model)

	// Get the authenticated user for record ownership.
	currentUser := "mcp-agent"
	if u, ok := c.Get("user"); ok {
		if s, ok := u.(string); ok && s != "" {
			currentUser = s
		}
	}

	// Build function definitions from the registry.
	functions := buildFunctions(reg)
	// Add system-level tools (doctype creation, validation, dry run).
	functions = append(functions, buildSystemFunctions()...)

	// Validate and cap incoming history.
	sanitizedHistory := sanitizeHistory(req.History, cfg.HistoryLimit)

	// Build messages array with system instructions.
	messages := []map[string]any{{
		"role": "system",
		"content": `You are a helpful AI assistant for a business application called Kora. Help users manage their data — create, find, update, and analyze business records.

RULES (follow strictly):
- Be CONCISE. One sentence when possible. No markdown tables unless showing actual data results.
- Before creating a record, ALWAYS call _find first to check duplicates. If none found, call _create immediately.
- ALL fields in function parameters ARE available. Never claim a field is "not exposed."
- If create fails due to missing required fields, ask for ALL missing fields at ONCE in ONE sentence.
- When user gives comma-separated data, map it to fields in order. The first value goes to the first required field. Just try it — don't ask permission.
- NEVER mention internal details: database schemas, table names, SQL, tool/function names, error tracebacks.
- Format booleans as ✅/❌. Use proper currency formatting.

DESTRUCTIVE ACTIONS (safety gate):
- NEVER call _delete on multiple records without asking "Delete N [records]?" first. For single deletes, confirm: "Delete [name]?"
- NEVER call _update to change workflow states, amounts, or linked documents without summarizing what will change and asking.
- If the user says "delete all" or "clean up" or "remove old" — STOP. Ask: "How many records should I delete? Which ones specifically?"
- These rules prevent accidental data loss. The user should always confirm destructive actions.

DOCTYPE CREATION (special rules):
- When asked to create a new form/doctype: gather requirements, call validate_doctype_yaml, then SUMMARIZE what you understood in 2-3 lines max. Ask "Create this as draft?" Do NOT show the YAML.
- WAIT for user confirmation before calling create_doctype_draft or update_doctype_draft.
- If user says "yes" or "go ahead" or "create it": call create_doctype_draft immediately.
- If user says "add X" or "change Y": adjust and validate again, then ask again.
- The summary must be scannable. Example: "Invoice form: link to Customer, date fields, Draft→Paid status, line items table with auto-calculated totals, tax at 16%. Create as draft?"
- Never show the YAML to the user unless they explicitly ask "show me the YAML."`,
	}}
	for _, h := range sanitizedHistory {
		messages = append(messages, map[string]any{"role": h.Role, "content": h.Content})
	}
	messages = append(messages, map[string]any{"role": "user", "content": req.Message})

	// Build the initial AI request body. Tools and tool_choice persist across all rounds.
	aiBody := map[string]any{
		"model":                model,
		"messages":             messages,
		"max_tokens":           cfg.MaxTokensPerCall,
		"parallel_tool_calls":  false, // Force sequential — avoids ordering issues.
	}
	if len(functions) > 0 {
		aiBody["tools"] = functions
		aiBody["tool_choice"] = "auto"
	}

	// --- Multi-Round Tool Execution Loop ---
	var (
		totalTokens int      // approximate running token count
		stallCount  int      // consecutive identical tool calls
		toolErrors  int      // cumulative tool errors for circuit breaker
		lastSigs    []string // tool call signatures from previous round
	)

	for round := 0; round < cfg.MaxRounds; round++ {
		aiResp, err := callAIWithRetry(baseURL, apiKey, aiBody, cfg)
		if err != nil {
			slog.Error("AI provider call failed", "error", err, "round", round)
			// On first-round failure, return an error.
			// On later rounds, return whatever tool results we've accumulated.
			if round == 0 {
				c.JSON(http.StatusInternalServerError, ErrorResponse{
					Error: map[string]string{"message": "AI provider error: " + err.Error()},
				})
				return
			}
			// Fallback: return the last assistant message content if available.
			fallbackReply := "I encountered an error while processing your request. Please try again."
			if lastContent := findLastAssistantContent(messages); lastContent != "" {
				fallbackReply = lastContent
			}
			c.JSON(http.StatusOK, ChatResponse{Reply: fallbackReply, Action: "partial"})
			return
		}

		// --- Safe extraction of the AI response ---
		choices := safeGetSlice(aiResp, "choices")
		if len(choices) == 0 {
			slog.Error("AI provider returned empty or missing choices", "response", aiResp)
			c.JSON(http.StatusInternalServerError, ErrorResponse{
				Error: map[string]string{"message": "AI provider returned an unexpected response format."},
			})
			return
		}
		choice, ok := choices[0].(map[string]any)
		if !ok {
			c.JSON(http.StatusInternalServerError, ErrorResponse{
				Error: map[string]string{"message": "AI provider returned an unexpected response format."},
			})
			return
		}

		msg := safeGetMap(choice, "message")
		if msg == nil {
			c.JSON(http.StatusInternalServerError, ErrorResponse{
				Error: map[string]string{"message": "AI provider response missing message."},
			})
			return
		}

		finishReason := safeGetString(choice, "finish_reason")
		content := safeGetString(msg, "content")

		// Track token usage if available.
		if usage := safeGetMap(aiResp, "usage"); usage != nil {
			if tt, ok := usage["total_tokens"].(float64); ok {
				totalTokens += int(tt)
			}
		} else {
			// Rough estimate: 4 chars ≈ 1 token.
			totalTokens += len(content) / 4
		}

		// --- Primary dispatch on finish_reason ---
		switch finishReason {

		case "stop":
			// Check for textified tool calls before accepting as genuine stop.
			if hasTextifiedToolCall(content) {
				slog.Warn("Detected textified tool call in AI response, retrying", "model", model)
				messages = append(messages, map[string]any{
					"role":    "user",
					"content": "You appear to have output a tool call as text instead of using the function calling format. Please use the function calling mechanism to call the appropriate tool.",
				})
				aiBody["tool_choice"] = "required"
				aiBody["messages"] = messages
				continue
			}

			// Check for narrate-then-act false finish.
			if isNarrativePromise(content) && !containsActualData(content) {
				slog.Warn("Detected narrate-then-act false finish, nudging model")
				messages = append(messages, map[string]any{
					"role":    "user",
					"content": "You mentioned taking an action. Please use the available tools to do so, or explain why you can't.",
				})
				aiBody["tool_choice"] = "required"
				aiBody["messages"] = messages
				continue
			}

			// Genuine stop — model is done.
			if content == "" {
				content = "I processed your request."
			}
			c.JSON(http.StatusOK, ChatResponse{Reply: content})
			return

		case "tool_calls":
			toolCalls := safeGetSlice(msg, "tool_calls")
			if len(toolCalls) == 0 {
				// finish_reason says tool_calls but none present — treat as stop.
				if content != "" {
					c.JSON(http.StatusOK, ChatResponse{Reply: content})
					return
				}
				continue
			}

			// --- Stall detection ---
			sigs := toolCallSignatures(toolCalls)
			if stringSlicesEqual(sigs, lastSigs) {
				stallCount++
				if stallCount >= cfg.StallThreshold {
					// Build a specific nudge telling the model what happened.
					lastToolName := ""
					if len(toolCalls) > 0 {
						lastToolName = safeGetToolName(toolCalls[0])
					}
					nudge := fmt.Sprintf(
						"You've called %s with the same arguments %d times without making progress. If the record doesn't exist, use the _create operation. If you have enough information to answer the user, please do so now without calling more tools.",
						lastToolName, stallCount,
					)
					messages = append(messages, map[string]any{
						"role":    "user",
						"content": nudge,
					})
					stallCount = 0
					aiBody["messages"] = messages
					continue
				}
			} else {
				stallCount = 0
				lastSigs = sigs
			}

			// --- Execute tools ---
			for _, tc := range toolCalls {
				if call, ok := tc.(map[string]any); ok {
					fn := safeGetMap(call, "function")
					slog.Info("AI tool call", "name", safeGetString(fn, "name"), "args", safeGetString(fn, "arguments"))
				}
			}
			toolResults := executeToolCallsForAI(tx, reg, toolCalls, currentUser, siteName)
			for i, tr := range toolResults {
				raw := tr["content"].(string)
				slog.Info("Tool result", "content", raw[:min(len(raw), 200)])
				if isToolError(raw) {
					toolErrors++
					tr["is_error"] = true
				}
				// Cap result size.
				toolResults[i]["content"] = capResultSize(raw, cfg.MaxToolResultChars)
			}

			// --- Error circuit breaker ---
			if toolErrors >= cfg.MaxToolErrors {
				messages = append(messages, map[string]any{
					"role":    "user",
					"content": fmt.Sprintf("%d tool errors have occurred. Please provide your best answer based on what information you have, without calling more tools.", toolErrors),
				})
				aiBody["messages"] = messages
				toolErrors = 0
				continue
			}

			// --- Append assistant message + tool results ---
			// Preserve content alongside tool_calls — don't nil it.
			messages = append(messages, map[string]any{
				"role":       "assistant",
				"content":    msg["content"], // may be nil — that's fine
				"tool_calls": toolCalls,
			})
			messages = append(messages, toolResults...)

			// --- Context compaction ---
			if totalTokens > int(float64(cfg.TokenBudget)*cfg.CompactionThreshold) {
				messages = compactHistory(messages, 6)
				slog.Info("Compacted chat history", "round", round, "estimated_tokens", totalTokens)
			}

			aiBody["messages"] = messages
			// tools and tool_choice stay in aiBody for all rounds.
			continue

		case "length":
			// Truncated. Check for partial tool calls.
			toolCalls := safeGetSlice(msg, "tool_calls")
			if len(toolCalls) > 0 && hasMalformedArgs(toolCalls) {
				// Increase max_tokens for retry.
				cfg.MaxTokensPerCall *= 2
				if cfg.MaxTokensPerCall > 16384 {
					cfg.MaxTokensPerCall = 16384 // hard cap
				}
				aiBody["max_tokens"] = cfg.MaxTokensPerCall
				slog.Warn("Partial tool call detected, increasing max_tokens", "new_max", cfg.MaxTokensPerCall)
				continue
			}
			// No partial tool calls — return what we have.
			if content == "" {
				content = "I ran out of space processing your request. Could you try a more specific query?"
			}
			c.JSON(http.StatusOK, ChatResponse{Reply: content, Action: "truncated"})
			return

		case "content_filter":
			c.JSON(http.StatusOK, ChatResponse{
				Reply: "I can't respond to that request due to content policies.",
			})
			return

		default:
			// Unknown finish_reason. If there's content, return it.
			if content != "" {
				c.JSON(http.StatusOK, ChatResponse{Reply: content})
				return
			}
			// Otherwise continue the loop.
			slog.Warn("Unknown finish_reason, continuing loop", "finish_reason", finishReason)
			continue
		}
	}

	// --- SAFETY NET: Max rounds exhausted ---
	slog.Warn("Max rounds exhausted in chat loop", "max_rounds", cfg.MaxRounds)
	c.JSON(http.StatusOK, ChatResponse{
		Reply:  "I've taken several actions but wasn't able to complete the task. Could you break this into smaller steps?",
		Action: "max_rounds_reached",
	})
}

// ---------------------------------------------------------------------------
// Safe access helpers — prevent panics from unexpected AI provider responses.
// ---------------------------------------------------------------------------

func safeGetString(m map[string]any, key string) string {
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		return ""
	}
	return s
}

func safeGetMap(m map[string]any, key string) map[string]any {
	v, ok := m[key]
	if !ok || v == nil {
		return nil
	}
	mm, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	return mm
}

func safeGetSlice(m map[string]any, key string) []any {
	v, ok := m[key]
	if !ok || v == nil {
		return nil
	}
	s, ok := v.([]any)
	if !ok {
		return nil
	}
	return s
}

func safeGetToolName(tc any) string {
	call, ok := tc.(map[string]any)
	if !ok {
		return "unknown"
	}
	fn, ok := call["function"].(map[string]any)
	if !ok {
		return "unknown"
	}
	return safeGetString(fn, "name")
}

// ---------------------------------------------------------------------------
// Provider resolution
// ---------------------------------------------------------------------------

func resolveProvider(db *sql.DB, siteName, modelOverride string) (providerKey, apiKey, baseURL, model string) {
	store := secret.NewStore(db)

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

	// Fallback: shared AI keys from environment (superadmin-configured).
	if os.Getenv("KORA_SHARED_AI_ENABLED") != "true" {
		return "", "", "", ""
	}
	sharedProviders := []struct{ envKey, base, defaultModel string }{
		{"KORA_SHARED_OPENAI_API_KEY", "https://api.openai.com/v1", "gpt-4o"},
		{"KORA_SHARED_DEEPSEEK_API_KEY", "https://api.deepseek.com", "deepseek-v4-pro"},
		{"KORA_SHARED_ANTHROPIC_API_KEY", "https://api.anthropic.com/v1", "claude-sonnet-4-6"},
	}
	for _, p := range sharedProviders {
		if k := os.Getenv(p.envKey); k != "" {
			m := p.defaultModel
			if modelOverride != "" {
				m = modelOverride
			}
			return p.envKey, k, p.base, m
		}
	}
	return "", "", "", ""
}

// ---------------------------------------------------------------------------
// Tool function generation
// ---------------------------------------------------------------------------

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
				"name":        lower + "_find",
				"description": "Find " + dt.Name + " by field values. Fields: " + strings.Join(fieldNamesForDescription(dt), ", "),
				"parameters": map[string]any{
					"type":       "object",
					"properties": props,
				},
			},
		}, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        lower + "_list",
				"description": "List " + dt.Name + " documents (recent first). Use after _find to browse all records.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"limit":  map[string]any{"type": "integer", "description": "Max results (default 20)"},
						"offset": map[string]any{"type": "integer", "description": "Pagination offset"},
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
				"description": "Create a new " + dt.Name + ". Available fields: " + strings.Join(fieldNamesForDescription(dt), ", "),
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

// ---------------------------------------------------------------------------
// AI provider HTTP call with retry
// ---------------------------------------------------------------------------

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

// callAIWithRetry calls the AI provider with exponential backoff on transient errors.
func callAIWithRetry(baseURL, apiKey string, body map[string]any, cfg AIConfig) (map[string]any, error) {
	var lastErr error
	for attempt := 0; attempt <= cfg.MaxRetries; attempt++ {
		if attempt > 0 {
			backoff := time.Duration(cfg.RetryBackoffMs) * time.Millisecond * time.Duration(math.Pow(2, float64(attempt-1)))
			slog.Info("Retrying AI call", "attempt", attempt, "backoff_ms", backoff.Milliseconds())
			time.Sleep(backoff)
		}

		result, err := callAI(baseURL, apiKey, body)
		if err == nil {
			return result, nil
		}

		lastErr = err

		// Only retry on transient errors (429, 503, 502, 504).
		if !isTransientError(err) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("AI provider call failed after %d retries: %w", cfg.MaxRetries, lastErr)
}

func isTransientError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "429") ||
		strings.Contains(msg, "503") ||
		strings.Contains(msg, "502") ||
		strings.Contains(msg, "504")
}

// ---------------------------------------------------------------------------
// Tool execution
// ---------------------------------------------------------------------------

// executeToolCallsForAI runs tool calls and returns results in OpenAI tool message format.
func executeToolCallsForAI(tx *orm.TxManager, reg *doctype.Registry, toolCalls []any, owner, siteName string) []map[string]any {
	var results []map[string]any
	for _, tc := range toolCalls {
		call, ok := tc.(map[string]any)
		if !ok {
			results = append(results, map[string]any{
				"role":         "tool",
				"tool_call_id": "unknown",
				"content":      "Error: invalid tool call format from AI",
			})
			continue
		}

		id := safeGetString(call, "id")
		fn := safeGetMap(call, "function")
		if fn == nil {
			results = append(results, map[string]any{
				"role":         "tool",
				"tool_call_id": id,
				"content":      "Error: missing function in tool call",
			})
			continue
		}

		name := safeGetString(fn, "name")
		argsJSON := safeGetString(fn, "arguments")

		var args map[string]any
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			results = append(results, map[string]any{
				"role":         "tool",
				"tool_call_id": id,
				"content":      fmt.Sprintf("Error: invalid arguments JSON: %v. Arguments received: %s", err, argsJSON),
			})
			continue
		}

		result := executeSingleTool(tx, reg, name, args, owner, siteName)
		results = append(results, map[string]any{
			"role":         "tool",
			"tool_call_id": id,
			"content":      result,
		})
	}
	return results
}

func executeSingleTool(tx *orm.TxManager, reg *doctype.Registry, toolName string, args map[string]any, owner, siteName string) string {
	// --- System tools (no doctype prefix) ---
	switch toolName {
	case "list_doctypes":
		return executeListDoctypes(reg)
	case "validate_doctype_yaml":
		yamlStr, _ := args["yaml"].(string)
		return executeValidateYAML(yamlStr)
	case "analytics_insights":
		doctypeName, _ := args["doctype"].(string)
		return executeAnalyticsInsights(tx, reg, doctypeName, siteName)
	case "create_doctype_draft":
		yamlStr, _ := args["yaml"].(string)
		return executeCreateDoctypeDraft(tx, reg, yamlStr, owner, siteName)
	}

	// Parse tool name using suffix matching (handles multi-word doctype names like "Work Order").
	doctypeName, operation, ok := parseToolName(toolName)
	if !ok {
		return fmt.Sprintf("Unknown tool: %s", toolName)
	}

	// Find the doctype. Try exact sanitized-name match first, then case-insensitive.
	var dt *doctype.DocType
	for _, d := range reg.All() {
		if sanitizeName(d.Name) == doctypeName {
			dt = d
			break
		}
	}
	if dt == nil {
		for _, d := range reg.All() {
			if strings.EqualFold(sanitizeName(d.Name), doctypeName) {
				dt = d
				break
			}
		}
	}
	if dt == nil {
		return fmt.Sprintf("DocType %q not found", doctypeName)
	}

	switch operation {
	case "find":
		// Build JSON filter array from provided field values, using proper JSON encoding.
		var filtParts []string
		for k, v := range args {
			if v != nil && v != "" && k != "limit" && k != "offset" {
				vJSON, err := json.Marshal(fmt.Sprintf("%v", v))
				if err != nil {
					vJSON = []byte(fmt.Sprintf(`"%v"`, v))
				}
				filtParts = append(filtParts, fmt.Sprintf(`["%s","=",%s]`, k, vJSON))
			}
		}
		filter := "[" + strings.Join(filtParts, ",") + "]"
		docs, total, err := tx.GetList(dt, filter, "", 5, 0, "")
		if err != nil {
			return fmt.Sprintf("Error finding %s: %v", dt.Name, err)
		}
		if total == 0 {
			return fmt.Sprintf("No %s found matching the criteria.", dt.Name)
		}
		// Return count + up to 3 top matches so the model can detect duplicates.
		var summaries []string
		maxShow := 3
		if len(docs) < maxShow {
			maxShow = len(docs)
		}
		for i := 0; i < maxShow; i++ {
			summaries = append(summaries, fmt.Sprintf("%v", docs[i].Fields))
		}
		if total > maxShow {
			summaries = append(summaries, fmt.Sprintf("... and %d more", total-maxShow))
		}
		return fmt.Sprintf("Found %d matching %s: %s", total, dt.Name, strings.Join(summaries, "; "))
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
		// Validate field names — reject unknown fields with a helpful message.
		if unknown := unknownFields(args, dt); len(unknown) > 0 {
			slog.Warn("Rejecting unknown fields in tool call", "unknown", unknown, "valid", availableFieldNames(dt), "doctype", dt.Name)
			return fmt.Sprintf("Error: unknown fields: %s. Valid fields: %s",
				strings.Join(unknown, ", "), availableFieldNames(dt))
		}
		doc := doctype.NewDocument(dt.Name)
		for k, v := range args {
			doc.Set(k, v)
		}
		if err := tx.Insert(dt, doc, owner, "ai-assistant"); err != nil {
			return fmt.Sprintf("Error creating %s: %v", dt.Name, err)
		}
		return fmt.Sprintf("Created %s %q.", dt.Name, doc.Name)
	default:
		return fmt.Sprintf("Unknown operation: %s", operation)
	}
}

// ---------------------------------------------------------------------------
// Tool name parsing (suffix-based — handles multi-word doctype names)
// ---------------------------------------------------------------------------

var knownOps = []string{"_find", "_list", "_get", "_create", "_update", "_delete"}

// parseToolName splits a tool name like "work_order_create" into doctype "work_order" and operation "create".
func parseToolName(toolName string) (doctypeName, operation string, ok bool) {
	for _, op := range knownOps {
		if strings.HasSuffix(toolName, op) {
			return strings.TrimSuffix(toolName, op), op[1:], true // op[1:] strips the leading underscore
		}
	}
	return "", "", false
}

// ---------------------------------------------------------------------------
// System-level tools — doctype creation, validation, dry-run.
// These always create as Draft. Only a human can activate a draft.
// ---------------------------------------------------------------------------

func buildSystemFunctions() []map[string]any {
	// YAML examples embedded in tool descriptions so the AI generates correct syntax.
	simpleExample := `name: Supplier
module: Buying
title_field: company_name
search_fields: company_name, email
sort_field: modified
sort_order: DESC
fields:
  - fieldname: company_name
    fieldtype: Data
    label: Company Name
    reqd: true
    in_list_view: true
  - fieldname: contact_person
    fieldtype: Data
    label: Contact Person
    in_list_view: true
  - fieldname: email
    fieldtype: Data
    label: Email
    options: Email
  - fieldname: phone
    fieldtype: Data
    label: Phone
    options: Phone
  - fieldname: address
    fieldtype: Text
    label: Address`

	complexExample := `name: Invoice
module: Accounting
title_field: customer
search_fields: customer, status
sort_field: modified
sort_order: DESC
is_submittable: true
track_changes: true
fields:
  - fieldname: customer
    fieldtype: Link
    label: Customer
    options: Customer
    reqd: true
    in_list_view: true
  - fieldname: invoice_date
    fieldtype: Date
    label: Invoice Date
    reqd: true
    in_list_view: true
  - fieldname: due_date
    fieldtype: Date
    label: Due Date
  - fieldname: status
    fieldtype: Select
    label: Status
    options: |
      Draft
      Sent
      Paid
      Overdue
      Cancelled
    default: Draft
    in_list_view: true
  - fieldname: section_items
    fieldtype: Section Break
    label: Items
  - fieldname: items
    fieldtype: Table
    label: Items
    options: Invoice Item
  - fieldname: section_totals
    fieldtype: Section Break
    label: Totals
  - fieldname: subtotal
    fieldtype: Currency
    label: Subtotal
    computed: SUM(items.line_total)
    read_only: true
  - fieldname: tax_rate
    fieldtype: Percent
    label: Tax Rate
    default: "16"
  - fieldname: tax_amount
    fieldtype: Currency
    label: Tax Amount
    computed: subtotal * tax_rate / 100
    read_only: true
  - fieldname: grand_total
    fieldtype: Currency
    label: Grand Total
    computed: subtotal + tax_amount
    read_only: true`

	childTableExample := `name: Invoice Item
module: Accounting
is_child_table: true
title_field: product
sort_field: idx
sort_order: ASC
fields:
  - fieldname: product
    fieldtype: Link
    label: Product
    options: Product
    reqd: true
    in_list_view: true
  - fieldname: quantity
    fieldtype: Int
    label: Quantity
    reqd: true
    default: "1"
  - fieldname: unit_price
    fieldtype: Currency
    label: Unit Price
    reqd: true
  - fieldname: line_total
    fieldtype: Currency
    label: Line Total
    computed: quantity * unit_price
    read_only: true`

	return []map[string]any{
		analyticsToolDef(),
		{
			"type": "function",
			"function": map[string]any{
				"name":        "list_doctypes",
				"description": "List all DocTypes in this site. Use this BEFORE creating a new doctype to see what already exists and what Link targets are available.",
				"parameters": map[string]any{
					"type":       "object",
					"properties": map[string]any{},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "validate_doctype_yaml",
				"description": "Validate a DocType YAML definition WITHOUT saving. Always call this first before create_doctype_draft. Returns syntax errors with line numbers and 'did you mean?' suggestions for unknown keys.\n\nFIELD TYPES: Data, Text, Int, Float, Currency, Percent, Check, Date, Time, Datetime, Select (with options), Link (set options to target doctype name), Table (set options to child doctype name), Section Break, Column Break, Heading.\n\nCOMPUTED FIELDS: Use expressions like 'quantity * unit_price', 'SUM(items.line_total)', 'ROUND(expr, 2)'. Computed fields should be read_only: true.\n\nTABLE (CHILD TABLE): When adding a Table field, you MUST also create the child doctype (set is_child_table: true). The child doctype name goes in the Table field's 'options'.\n\nSIMPLE EXAMPLE:\n" + simpleExample + "\n\nCOMPLEX EXAMPLE (with Table, computed, Link, Select):\n" + complexExample + "\n\nCHILD TABLE EXAMPLE:\n" + childTableExample,
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"yaml": map[string]any{"type": "string", "description": "YAML content to validate"},
					},
					"required": []string{"yaml"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "update_doctype_draft",
				"description": "Update an EXISTING DocType as DRAFT. Provide the FULL YAML for the doctype (include all existing fields plus your changes). The existing doctype is replaced with this definition. Always call validate_doctype_yaml first. Only call AFTER user confirms they want to update.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"yaml": map[string]any{"type": "string", "description": "Complete updated doctype YAML"},
					},
					"required": []string{"yaml"},
				},
			},
		},
		{
			"type": "function",
			"function": map[string]any{
				"name":        "create_doctype_draft",
				"description": "Create a NEW DocType as DRAFT only. Does NOT create database tables — a human must review and activate. If the doctype has a Table field, create the child doctype FIRST (as a separate call), then the parent. Always call validate_doctype_yaml before this. Only call this AFTER the user confirms they want to create.\n\nFIELD TYPES: Data, Text, Int, Float, Currency, Percent, Check, Date, Time, Datetime, Select (with options using | prefix for multi-line), Link (options = target doctype name), Table (options = child doctype name), Section Break, Column Break.\n\nCOMPUTED: 'quantity * unit_price', 'SUM(items.line_total)', 'ROUND(expr, N)'. Set read_only: true.\n\nFor child tables: set is_child_table: true. Do NOT include table columns (parent, parentfield, parenttype, idx) — the system adds them automatically.\n\nSIMPLE EXAMPLE:\n" + simpleExample + "\n\nCOMPLEX EXAMPLE (with Table, Link, Select, computed fields, submittable):\n" + complexExample + "\n\nCHILD TABLE EXAMPLE:\n" + childTableExample,
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"yaml": map[string]any{"type": "string", "description": "Complete doctype YAML. Use the examples above as templates."},
					},
					"required": []string{"yaml"},
				},
			},
		},
	}
}

func sanitizeName(name string) string {
	s := strings.ToLower(name)
	s = strings.ReplaceAll(s, " ", "_")
	s = strings.ReplaceAll(s, "-", "_")
	return s
}

// ---------------------------------------------------------------------------
// Stall detection helpers
// ---------------------------------------------------------------------------

// toolCallSignatures returns a canonical signature for each tool call
// (name + sorted JSON args) for comparison across rounds.
func toolCallSignatures(toolCalls []any) []string {
	sigs := make([]string, 0, len(toolCalls))
	for _, tc := range toolCalls {
		call, ok := tc.(map[string]any)
		if !ok {
			continue
		}
		fn := safeGetMap(call, "function")
		if fn == nil {
			continue
		}
		name := safeGetString(fn, "name")
		args := safeGetString(fn, "arguments")
		sigs = append(sigs, name+"::"+args)
	}
	return sigs
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Tool result helpers
// ---------------------------------------------------------------------------

// capResultSize truncates a tool result to maxChars, preserving head and tail.
func capResultSize(content string, maxChars int) string {
	if len(content) <= maxChars {
		return content
	}
	half := maxChars / 2
	return content[:half] +
		fmt.Sprintf("\n\n... [%d characters trimmed] ...\n\n", len(content)-maxChars) +
		content[len(content)-half:]
}

func isToolError(content string) bool {
	return strings.HasPrefix(content, "Error") || strings.HasPrefix(content, "Unknown")
}

// ---------------------------------------------------------------------------
// Textified tool call detection
// ---------------------------------------------------------------------------

var textifiedPatterns = []string{
	"<｜｜DSML｜｜tool_calls>",
	"<function_call>",
	"<invoke name=",
	`"tool_calls"`,
	`"function_call"`,
}

func hasTextifiedToolCall(content string) bool {
	if content == "" {
		return false
	}
	lower := strings.ToLower(content)
	for _, pat := range textifiedPatterns {
		if strings.Contains(lower, pat) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Narrate-then-act detection (GPT-4o false finish)
// ---------------------------------------------------------------------------

var narrativePatterns = []string{
	"i'll look",
	"let me check",
	"i will search",
	"let me search",
	"i'll check",
	"i'll find",
	"let me find",
	"i'll look that up",
	"let me look that up",
	"i will look",
}

func isNarrativePromise(content string) bool {
	if content == "" {
		return false
	}
	lower := strings.ToLower(content)
	for _, pat := range narrativePatterns {
		if strings.Contains(lower, pat) {
			return true
		}
	}
	return false
}

// containsActualData is a cheap heuristic: does the content contain data (markdown table,
// bullet list, key-value pairs) rather than just a conversational promise.
func containsActualData(content string) bool {
	markers := []string{"|", "**", "- ", "1. ", "✅", "❌", "$", "€", "£"}
	for _, m := range markers {
		if strings.Contains(content, m) {
			return true
		}
	}
	// If the content is longer than 200 chars, it likely contains substantive data.
	return len(content) > 200
}

// ---------------------------------------------------------------------------
// Partial tool call detection (for finish_reason == "length")
// ---------------------------------------------------------------------------

func hasMalformedArgs(toolCalls []any) bool {
	for _, tc := range toolCalls {
		call, ok := tc.(map[string]any)
		if !ok {
			continue
		}
		fn := safeGetMap(call, "function")
		if fn == nil {
			continue
		}
		argsJSON := safeGetString(fn, "arguments")
		if argsJSON == "" {
			continue
		}
		var dummy map[string]any
		if err := json.Unmarshal([]byte(argsJSON), &dummy); err != nil {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// History sanitization
// ---------------------------------------------------------------------------

func sanitizeHistory(history []ChatMessage, limit int) []ChatMessage {
	if len(history) == 0 {
		return nil
	}
	// Cap at limit.
	if len(history) > limit {
		history = history[len(history)-limit:]
	}
	// Filter out system messages and messages with tool call content.
	var cleaned []ChatMessage
	for _, m := range history {
		if m.Role == "system" {
			continue
		}
		// Skip messages that look like they contain raw tool calls.
		if hasTextifiedToolCall(m.Content) {
			continue
		}
		cleaned = append(cleaned, m)
	}
	return cleaned
}

// ---------------------------------------------------------------------------
// Context compaction
// ---------------------------------------------------------------------------

// compactHistory keeps the first message (system prompt), the user's first message,
// and the most recent keepRecent messages. Tool results in between are summarized.
func compactHistory(messages []map[string]any, keepRecent int) []map[string]any {
	if len(messages) <= keepRecent+2 {
		return messages
	}

	compacted := make([]map[string]any, 0, keepRecent+3)

	// Always preserve the system prompt (first message).
	compacted = append(compacted, messages[0])

	// Preserve the user's first message (second message after system prompt, or the
	// first user-role message we find).
	for i := 1; i < len(messages)-keepRecent; i++ {
		if safeGetString(messages[i], "role") == "user" {
			compacted = append(compacted, messages[i])
			break
		}
	}

	// Build a summary of what happened in the compacted region.
	summary := summarizeCompacted(messages[1 : len(messages)-keepRecent])
	if summary != "" {
		compacted = append(compacted, map[string]any{
			"role":    "system",
			"content": "[Earlier conversation steps: " + summary + "]",
		})
	}

	// Keep the most recent messages.
	compacted = append(compacted, messages[len(messages)-keepRecent:]...)

	return compacted
}

func summarizeCompacted(messages []map[string]any) string {
	var actions []string
	for _, m := range messages {
		role := safeGetString(m, "role")
		switch role {
		case "tool":
			content := safeGetString(m, "content")
			if content != "" {
				// Extract first line or first 120 chars.
				firstLine := strings.SplitN(content, "\n", 2)[0]
				if len(firstLine) > 120 {
					firstLine = firstLine[:120] + "..."
				}
				actions = append(actions, firstLine)
			}
		case "user":
			content := safeGetString(m, "content")
			if content != "" && !strings.HasPrefix(content, "You've called") && !strings.HasPrefix(content, "You appear to") && !strings.HasPrefix(content, "You mentioned") {
				if len(content) > 100 {
					content = content[:100] + "..."
				}
				actions = append(actions, "User asked: "+content)
			}
		}
	}
	// Take the most recent 3 actions for the summary.
	if len(actions) > 3 {
		actions = actions[len(actions)-3:]
	}
	return strings.Join(actions, "; ")
}

// ---------------------------------------------------------------------------
// Misc helpers
// ---------------------------------------------------------------------------

func findLastAssistantContent(messages []map[string]any) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if safeGetString(messages[i], "role") == "assistant" {
			c := safeGetString(messages[i], "content")
			if c != "" {
				return c
			}
		}
	}
	return ""
}

func formatCell(fieldname string, v any) string {
	if v == nil {
		return ""
	}
	s := fmt.Sprintf("%v", v)
	// Format known decimal fields to 2 places.
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


// unknownFields returns args keys that don't exist on the doctype.
func unknownFields(args map[string]any, dt *doctype.DocType) []string {
	valid := make(map[string]bool)
	for _, f := range dt.DataFields() {
		if f.Fieldtype != "Table" {
			valid[f.Fieldname] = true
		}
	}
	var unknown []string
	for k := range args {
		if !valid[k] {
			unknown = append(unknown, k)
		}
	}
	return unknown
}

// availableFieldNames returns the doctype's data field names as a comma-separated string.
func availableFieldNames(dt *doctype.DocType) string {
	var names []string
	for _, f := range dt.DataFields() {
		if f.Fieldtype != "Table" {
			names = append(names, f.Fieldname)
		}
	}
	return strings.Join(names, ", ")
}

// fieldNamesForDescription returns the names of data fields for use in tool descriptions.
func fieldNamesForDescription(dt *doctype.DocType) []string {
	var names []string
	for _, f := range dt.DataFields() {
		if f.Fieldtype != "Table" {
			name := f.Fieldname
			if f.Reqd {
				name += " (required)"
			}
			names = append(names, name)
		}
	}
	return names
}

// ---------------------------------------------------------------------------
// System tool execution
// ---------------------------------------------------------------------------

func executeListDoctypes(reg *doctype.Registry) string {
	var lines []string
	for _, dt := range reg.All() {
		if dt.IsChildTable {
			continue
		}
		lines = append(lines, fmt.Sprintf("%s (%d fields)", dt.Name, len(dt.DataFields())))
	}
	if len(lines) == 0 {
		return "No doctypes found."
	}
	return strings.Join(lines, "\n")
}

func executeValidateYAML(yamlStr string) string {
	if yamlStr == "" {
		return "Error: no YAML content provided."
	}
	syntaxErrs, validationErrs, err := doctype.ValidateYAML([]byte(yamlStr))
	if err != nil {
		return fmt.Sprintf("Error parsing YAML: %v", err)
	}
	if len(syntaxErrs) == 0 && len(validationErrs) == 0 {
		return "YAML is valid."
	}
	var parts []string
	for _, e := range syntaxErrs {
		parts = append(parts, fmt.Sprintf("Line %d: %s (%s)", e.Line, e.Message, e.Detail))
	}
	for _, e := range validationErrs {
		parts = append(parts, fmt.Sprintf("Validation: %s (field: %s)", e.Message, e.Field))
	}
	return strings.Join(parts, "\n")
}

func executeUpdateDoctypeDraft(tx *orm.TxManager, reg *doctype.Registry, yamlStr, owner, siteName string) string {
	if yamlStr == "" {
		return "Error: no YAML content provided. Use validate_doctype_yaml first."
	}

	// 1. Validate YAML.
	syntaxErrs, validationErrs, err := doctype.ValidateYAML([]byte(yamlStr))
	if err != nil {
		return fmt.Sprintf("Error parsing YAML: %v", err)
	}
	if len(syntaxErrs) > 0 || len(validationErrs) > 0 {
		var msgs []string
		for _, e := range syntaxErrs {
			msgs = append(msgs, fmt.Sprintf("Line %d: %s", e.Line, e.Message))
		}
		for _, e := range validationErrs {
			msgs = append(msgs, fmt.Sprintf("Validation: %s", e.Message))
		}
		return fmt.Sprintf("YAML validation failed:\n%s\n\nFix errors and validate again before updating.", strings.Join(msgs, "\n"))
	}

	// 2. Parse YAML into DocType struct.
	var dt doctype.DocType
	if err := yaml.Unmarshal([]byte(yamlStr), &dt); err != nil {
		return fmt.Sprintf("Error parsing YAML: %v", err)
	}
	if dt.Name == "" {
		return "Error: doctype 'name' is required in the YAML."
	}

	// 3. Check the doctype exists.
	if !reg.Has(dt.Name) {
		return fmt.Sprintf("Error: DocType %q does not exist. Use create_doctype_draft to create new doctypes.", dt.Name)
	}

	// 4. Run full validation.
	if err := dt.Validate(); err != nil {
		return fmt.Sprintf("Error validating doctype: %v", err)
	}

	// 5. Save as Draft — NEVER activate.
	store := configstore.NewStore(tx.DB, tx.Dialect)
	if err := store.SaveDocType(&dt); err != nil {
		return fmt.Sprintf("Error saving doctype: %v", err)
	}

	// 6. Update registry.
	reg.Register(&dt)

	// 7. Create config version as Draft with full snapshot.
	snapshot, _ := store.CollectSnapshot(reg)
	if owner == "" || owner == "mcp-agent" {
		owner = "ai-assistant"
	}
	verID, verNum, err := store.CreateConfigVersion(
		siteName, owner, "Updated "+dt.Name+" via AI (Draft)", "Draft", snapshot,
	)
	if err != nil {
		slog.Warn("config version creation failed", "error", err)
	}

	fields := len(dt.DataFields())
	return fmt.Sprintf(
		"✓ Updated DocType %q as DRAFT (%d fields). Version #%d (ID: %s). A human must review and activate it before it takes effect.",
		dt.Name, fields, verNum, verID,
	)
}

func executeCreateDoctypeDraft(tx *orm.TxManager, reg *doctype.Registry, yamlStr, owner, siteName string) string {
	if yamlStr == "" {
		return "Error: no YAML content provided. Use validate_doctype_yaml first."
	}

	// 1. Validate YAML.
	syntaxErrs, validationErrs, err := doctype.ValidateYAML([]byte(yamlStr))
	if err != nil {
		return fmt.Sprintf("Error parsing YAML: %v", err)
	}
	if len(syntaxErrs) > 0 || len(validationErrs) > 0 {
		var msgs []string
		for _, e := range syntaxErrs {
			msgs = append(msgs, fmt.Sprintf("Line %d: %s", e.Line, e.Message))
		}
		for _, e := range validationErrs {
			msgs = append(msgs, fmt.Sprintf("Validation: %s", e.Message))
		}
		return fmt.Sprintf("YAML validation failed:\n%s\n\nFix errors and validate again before creating.", strings.Join(msgs, "\n"))
	}

	// 2. Parse YAML into DocType struct.
	var dt doctype.DocType
	if err := yaml.Unmarshal([]byte(yamlStr), &dt); err != nil {
		return fmt.Sprintf("Error parsing YAML: %v", err)
	}
	if dt.Name == "" {
		return "Error: doctype 'name' is required in the YAML."
	}

	// 3. Check for duplicate.
	if reg.Has(dt.Name) {
		return fmt.Sprintf("Error: DocType %q already exists.", dt.Name)
	}

	// 4. Run full validation on the parsed struct.
	if err := dt.Validate(); err != nil {
		return fmt.Sprintf("Error validating doctype: %v", err)
	}

	// 5. Save to configstore as Draft — NEVER activate.
	store := configstore.NewStore(tx.DB, tx.Dialect)
	if err := store.SaveDocType(&dt); err != nil {
		return fmt.Sprintf("Error saving doctype: %v", err)
	}

	// 6. Register in runtime registry.
	reg.Register(&dt)

	// 7. Auto-create default permissions.
	if err := store.AutoCreatePermissionsForDoctype(dt.Name); err != nil {
		return fmt.Sprintf("Doctype created but permission setup failed: %v", err)
	}

	// 8. Create config version as Draft with full snapshot.
	snapshot, _ := store.CollectSnapshot(reg)
	if owner == "" || owner == "mcp-agent" {
		owner = "ai-assistant"
	}
	verID, verNum, err := store.CreateConfigVersion(
		siteName, owner, "Created "+dt.Name+" via AI (Draft)", "Draft", snapshot,
	)
	if err != nil {
		slog.Warn("config version creation failed", "error", err)
	}

	fields := len(dt.DataFields())
	return fmt.Sprintf(
		"✓ Created DocType %q as DRAFT (%d fields). Version #%d (ID: %s). A human must review and activate it before it takes effect.",
		dt.Name, fields, verNum, verID,
	)
}
