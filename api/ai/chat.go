package ai

import (
	"fmt"
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

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
func HandleChat(c *gin.Context, tx *orm.TxManager, reg *doctype.Registry, siteName, currentUser string) {
	var req ChatRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{"message": "Invalid request format"},
		})
		return
	}

	// Read the configured AI provider key.
	_, apiKey, baseURL, model := resolveProvider(tx.DB, siteName, req.Model)
	if apiKey == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{"message": "No AI provider configured. Go to /workspace/admin/secrets to add your API key (OpenAI, DeepSeek, or Anthropic)."},
		})
		return
	}

	// Load AI configuration (per-model defaults + site overrides).
	store := secret.NewStore(tx.DB)
	cfg := LoadAIConfig(store, siteName, model)

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
- BEFORE writing any YAML, ask 2-3 clarifying questions about the business: what they do, what data they track, who the users are, what reports/metrics matter. Understand the workflow FIRST.
- Ask about: key entities, relationships, required vs optional fields, validation rules (min/max, allowed options), who can do what (workflow steps), and what should be searchable.
- THEN call validate_doctype_yaml, SUMMARIZE what you understood in 2-3 lines. Ask "Create this as draft?" Do NOT show the YAML.
- WAIT for user confirmation before calling create_doctype_draft or update_doctype_draft.
- If user says "yes" or "go ahead" or "create it": call create_doctype_draft immediately.
- If user says "add X" or "change Y": adjust and validate again, then ask again.
- The summary must be scannable. Example: "Invoice form: link to Customer, date fields, Draft→Paid status, line items table with auto-calculated totals, tax at 16%. Create as draft?"
- CHILD TABLES: if a doctype has a Table field, create the child doctype FIRST (with is_child_table: true), then create the parent doctype.
- Never show the YAML to the user unless they explicitly ask "show me the YAML."

SYSTEM KNOWLEDGE (what Kora can do — use this to guide users):

ANALYTICS: Every doctype automatically gets analytics. Metrics include total count, daily/monthly trends, breakdowns by Select/Link fields, and sums of Currency/Int/Float fields. Submittable doctypes get workflow state distribution and funnel tracking. Users don't configure analytics — it just works. Direct users to the Insights tab.

VERSIONING & ACTIVATION: Doctype changes save as Draft config versions. Activation runs schema migration — database tables are created/updated. Three safety tiers: Safe (auto-apply), Warning (requires review), Blocked (requires fix). Users activate from /workspace/admin/versions. Draft changes don't affect the live database.

WORKFLOWS: Submittable doctypes support state-machine workflows. States (Draft, Submitted, Approved) with role-gated transitions and optional conditions. Notifications per event. Suggest workflows when users describe approval or lifecycle needs.

PERMISSIONS: 10 operations per role × doctype: Read, Write, Create, Delete, Submit, Cancel, Amend, Export, Import, Report. 'if_owner' scopes to creator. Administrator bypasses all. Manage at /workspace/admin/permissions.

COMPUTED FIELDS: Auto-calculated via expressions: arithmetic ('quantity * unit_price'), aggregation ('SUM(items.line_total)'), rounding ('ROUND(expr, 2)'). Recalculate automatically on dependency changes. Set read_only: true.

LINKED FIELDS: Auto-populate from linked documents. Example: selecting a Product fills the price via linked_field: 'product.selling_price'.

FIELD CONSTRAINTS: Per-field validation: min, max (numbers), min_length, max_length (text), regex (pattern), one_of/not_one_of (allowed values). Enforced at API level.

MULTI-TENANT: Isolated sites with own database, users, doctypes. Created from /console or self-service /onboard. Access via /s/sitename/workspace or custom domain.

AI CHAT: You have tools to list, find, get, create, update documents. You can create doctypes as Draft, validate YAML, and query analytics. Everything scoped to the current site.`,
	}}
	for _, h := range sanitizedHistory {
		messages = append(messages, map[string]any{"role": h.Role, "content": h.Content})
	}
	messages = append(messages, map[string]any{"role": "user", "content": req.Message})

	// Build the initial AI request body. Tools and tool_choice persist across all rounds.
	aiBody := map[string]any{
		"model":               model,
		"messages":            messages,
		"max_tokens":          cfg.MaxTokensPerCall,
		"parallel_tool_calls": false, // Force sequential — avoids ordering issues.
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
				c.JSON(http.StatusInternalServerError, gin.H{
					"error": gin.H{"message": "AI provider error: " + err.Error()},
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
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": gin.H{"message": "AI provider returned an unexpected response format."},
			})
			return
		}
		choice, ok := choices[0].(map[string]any)
		if !ok {
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": gin.H{"message": "AI provider returned an unexpected response format."},
			})
			return
		}

		msg := safeGetMap(choice, "message")
		if msg == nil {
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": gin.H{"message": "AI provider response missing message."},
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


