package ai

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/oklog/ulid/v2"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/orm"
	"github.com/asenawritescode/kora/secret"
)

// ChatRequest is the JSON body for POST /api/chat.
type ChatRequest struct {
	Message string        `json:"message"`
	History []ChatMessage `json:"history,omitempty"`
	Model   string        `json:"model,omitempty"` // override default model
	RunID   string        `json:"run_id,omitempty"`
	Context ChatContext   `json:"context,omitempty"`
}

// ChatContext carries lightweight UI context from the workspace shell.
type ChatContext struct {
	Pathname     string `json:"pathname,omitempty"`
	ShellMode    string `json:"shellMode,omitempty"`
	Doctype      string `json:"doctype,omitempty"`
	DocumentName string `json:"documentName,omitempty"`
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
	RunID  string `json:"run_id,omitempty"`
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
	runID := req.RunID
	if runID == "" {
		runID = ulid.Make().String()
	}
	subjectKey := currentUser + ":" + req.Context.Pathname + ":" + req.Context.Doctype + ":" + req.Context.DocumentName
	auditCtx := enrichAuditContext(c.Request.Context(), currentUser, c.GetString("session_sid"), c.GetString("correlation_id"), c.GetString("idempotency_key"))
	var existingRun *RunRecord
	if rec, err := LoadRun(c.Request.Context(), tx.DB, runID); err == nil {
		existingRun = &rec
	}

	// Read the configured AI provider key.
	providerKey, apiKey, baseURL, model := resolveProvider(tx.DB, siteName, req.Model)
	if apiKey == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{"message": "No AI provider configured. Go to /workspace/admin/secrets to add your API key (OpenAI, DeepSeek, or Anthropic)."},
		})
		return
	}

	// Load AI configuration (per-model defaults + site overrides).
	store := secret.NewStore(tx.DB)
	cfg := LoadAIConfig(store, siteName, model)
	if err := ValidateProviderProfile(ProviderProfile{
		ProviderKey:    providerKey,
		BaseURL:        baseURL,
		Model:          model,
		HTTPTimeoutSec: cfg.HTTPTimeoutSec,
	}); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{"message": "Invalid AI provider configuration: " + err.Error()},
		})
		return
	}
	if err := EnsureAIRunTables(c.Request.Context(), tx.DB); err != nil {
		slog.Error("AI run storage initialization failed", "site", siteName, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": "Unable to initialize AI run storage."}})
		return
	}
	conversationID := runID
	if existingRun != nil && existingRun.ConversationID != "" {
		conversationID = existingRun.ConversationID
	} else if conv, err := LoadConversation(c.Request.Context(), tx.DB, siteName, subjectKey); err == nil {
		conversationID = conv.ID
	} else {
		if err := UpsertConversation(c.Request.Context(), tx.DB, ConversationRecord{
			ID:         conversationID,
			Site:       siteName,
			Channel:    "chat",
			SubjectKey: subjectKey,
			Title:      req.Message,
			Status:     "active",
			LastRunID:  runID,
		}); err != nil {
			slog.Error("AI conversation persistence failed", "site", siteName, "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": "Unable to persist AI conversation."}})
			return
		}
	}
	if err := UpsertRun(c.Request.Context(), tx.DB, RunRecord{
		ID:             runID,
		Site:           siteName,
		ConversationID: conversationID,
		Channel:        "chat",
		Status:         "planning",
		InputMessage:   req.Message,
		Model:          model,
		Provider:       providerKey,
	}); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": gin.H{"message": "Unable to persist AI run."}})
		return
	}
	if existingRun == nil {
		_ = AppendMessage(c.Request.Context(), tx.DB, siteName, conversationID, runID, "user", req.Message, "message", "", 1)
	} else if existingRun.InputMessage != "" {
		req.Message = existingRun.InputMessage
	}

	// Build function definitions from the canonical tool catalog projection.
	functions := buildOpenAIToolsFromCatalog(BuildToolCatalog(reg))

	// Validate and cap incoming history.
	sanitizedHistory := sanitizeHistory(req.History, cfg.HistoryLimit)

	// Build messages array with system instructions.
	contextBlurb := ""
	if req.Context.Pathname != "" || req.Context.Doctype != "" || req.Context.DocumentName != "" {
		contextBlurb = fmt.Sprintf(`

CURRENT PAGE CONTEXT:
- pathname: %s
- shell mode: %s
- doctype: %s
- document: %s
Use this context to stay grounded in the current screen and avoid asking the user to repeat what is already visible.`,
			req.Context.Pathname,
			req.Context.ShellMode,
			req.Context.Doctype,
			req.Context.DocumentName,
		)
	}
	messages := []map[string]any{{
		"role": "system",
		"content": `You are a helpful AI assistant for a business application called Kora. Help users manage their data — create, find, update, and analyze business records.` + contextBlurb + `

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

	if estimatePromptTokens(messages, functions) > cfg.TokenBudget {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{"message": "AI request exceeds the configured token budget."},
		})
		return
	}
	reservation, err := ReserveBudget(c.Request.Context(), tx.DB, siteName, model, estimatePromptTokens(messages, functions)+cfg.MaxTokensPerCall, cfg.TokenBudget, "chat request")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{"message": err.Error()},
		})
		return
	}
	defer func() {
		_ = ReleaseBudget(c.Request.Context(), tx.DB, reservation)
	}()

	// --- Multi-Round Tool Execution Loop ---
	var (
		totalTokens int      // approximate running token count
		stallCount  int      // consecutive identical tool calls
		toolErrors  int      // cumulative tool errors for circuit breaker
		lastSigs    []string // tool call signatures from previous round
	)

	for round := 0; round < cfg.MaxRounds; round++ {
		stepID := ulid.Make().String()
		_ = UpsertStep(c.Request.Context(), tx.DB, stepID, siteName, runID, conversationID, fmt.Sprintf("round-%d", round+1), "planning", "", "", "", "", "")
		_ = UpsertTask(c.Request.Context(), tx.DB, TaskRecord{
			ID:             stepID,
			Site:           siteName,
			RunID:          runID,
			ConversationID: conversationID,
			Kind:           "round",
			Title:          fmt.Sprintf("Round %d", round+1),
			Description:    "Track the model's progress for this round and keep a queue of follow-up work.",
			Status:         "in_progress",
			SortOrder:      round + 1,
		})
		_ = UpsertRun(c.Request.Context(), tx.DB, RunRecord{
			ID:             runID,
			Site:           siteName,
			ConversationID: conversationID,
			Channel:        "chat",
			Status:         "planning",
			Model:          model,
			Provider:       providerKey,
			CurrentStepID:  stepID,
			InputMessage:   req.Message,
		})
		aiResp, err := callAIWithRetry(baseURL, apiKey, aiBody, cfg, func(attempt int, status string, latency time.Duration, attemptErr error) {
			tokens := map[contract.UsageClass]int64{}
			if status == "failed" {
				tokens[contract.UsageClassPartial] = 0
			}
			_ = RecordUsage(auditCtx, tx.DB, contract.UsageEvent{
				RunID:      runID,
				Site:       siteName,
				Model:      model,
				Provider:   providerKey,
				Attempt:    attempt,
				Status:     status,
				Tokens:     tokens,
				LatencyMs:  latency.Milliseconds(),
				OccurredAt: time.Now().UTC(),
				Attribution: map[string]string{
					"operation": "chat",
				},
			})
		})
		if err != nil {
			slog.Error("AI provider call failed", "error", err, "round", round)
			_ = UpdateStepStatus(c.Request.Context(), tx.DB, stepID, "failed", err.Error(), "", "", "", err.Error())
			_ = MarkTaskStatus(c.Request.Context(), tx.DB, stepID, "failed", err.Error())
			_ = UpsertRun(c.Request.Context(), tx.DB, RunRecord{
				ID:             runID,
				Site:           siteName,
				ConversationID: conversationID,
				Channel:        "chat",
				Status:         "failed",
				InputMessage:   req.Message,
				ErrorMessage:   err.Error(),
				Model:          model,
				Provider:       providerKey,
			})
			_ = RecordAudit(auditCtx, tx.DB, AuditEvent{
				Site:           siteName,
				RunID:          runID,
				StepID:         stepID,
				ConversationID: conversationID,
				Kind:           "model_attempt",
				Name:           model,
				Status:         "failed",
				Details: map[string]any{
					"round":    round + 1,
					"provider": providerKey,
					"error":    err.Error(),
				},
			})
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
			_ = AppendMessage(c.Request.Context(), tx.DB, siteName, conversationID, runID, "assistant", fallbackReply, "summary", stepID, round+2)
			_ = UpdateStepStatus(c.Request.Context(), tx.DB, stepID, "partial", fallbackReply, "", "", "", err.Error())
			_ = MarkTaskStatus(c.Request.Context(), tx.DB, stepID, "partial", err.Error())
			_ = UpsertRun(c.Request.Context(), tx.DB, RunRecord{
				ID:             runID,
				Site:           siteName,
				ConversationID: conversationID,
				Channel:        "chat",
				Status:         "partial",
				InputMessage:   req.Message,
				OutputMessage:  fallbackReply,
				ErrorMessage:   err.Error(),
				Model:          model,
				Provider:       providerKey,
			})
			c.JSON(http.StatusOK, ChatResponse{Reply: fallbackReply, Action: "partial", RunID: runID})
			return
		}

		// --- Safe extraction of the AI response ---
		choice, msg, respErr := extractAIChoice(aiResp)
		if respErr != nil {
			_ = RecordAudit(auditCtx, tx.DB, AuditEvent{
				Site:           siteName,
				RunID:          runID,
				StepID:         stepID,
				ConversationID: conversationID,
				Kind:           "model_attempt",
				Name:           model,
				Status:         "failed",
				Details: map[string]any{
					"round":    round + 1,
					"provider": providerKey,
					"error":    respErr.Error(),
				},
			})
			slog.Error("AI provider returned malformed response", "error", respErr, "response", aiResp)
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": gin.H{"message": respErr.Error()},
			})
			return
		}

		finishReason := safeGetString(choice, "finish_reason")
		content := safeGetString(msg, "content")

		// Track token usage if available.
		tokens := map[contract.UsageClass]int64{}
		if usage := safeGetMap(aiResp, "usage"); usage != nil {
			if tt, ok := usage["total_tokens"].(float64); ok {
				totalTokens += int(tt)
				tokens[contract.UsageClassTotal] = int64(tt)
			}
			if tt, ok := usage["prompt_tokens"].(float64); ok {
				tokens[contract.UsageClassInput] = int64(tt)
			}
			if tt, ok := usage["completion_tokens"].(float64); ok {
				tokens[contract.UsageClassOutput] = int64(tt)
			}
		} else {
			// Rough estimate: 4 chars ≈ 1 token.
			estimated := len(content) / 4
			totalTokens += estimated
			tokens[contract.UsageClassPartial] = int64(estimated)
		}
		_ = RecordAudit(auditCtx, tx.DB, AuditEvent{
			Site:           siteName,
			RunID:          runID,
			StepID:         stepID,
			ConversationID: conversationID,
			Kind:           "model_attempt",
			Name:           model,
			Status:         "completed",
			Details: map[string]any{
				"round":         round + 1,
				"provider":      providerKey,
				"finish_reason": finishReason,
			},
		})
		_ = FinalizeBudget(c.Request.Context(), tx.DB, reservation, totalTokens)

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
			_ = AppendMessage(c.Request.Context(), tx.DB, siteName, conversationID, runID, "assistant", content, "summary", stepID, round+2)
			_ = UpdateStepStatus(c.Request.Context(), tx.DB, stepID, "completed", content, "", "", content, "")
			_ = MarkTaskStatus(c.Request.Context(), tx.DB, stepID, "done", content)
			_ = QueueFollowUpTasks(c.Request.Context(), tx.DB, siteName, runID, conversationID, deriveFollowUpTasks(content, toolResultsFromMessages(messages)))
			_ = UpsertRun(c.Request.Context(), tx.DB, RunRecord{
				ID:             runID,
				Site:           siteName,
				ConversationID: conversationID,
				Channel:        "chat",
				Status:         "completed",
				InputMessage:   req.Message,
				OutputMessage:  content,
				Summary:        content,
				Model:          model,
				Provider:       providerKey,
			})
			_ = UpsertConversation(c.Request.Context(), tx.DB, ConversationRecord{
				ID:            conversationID,
				Site:          siteName,
				Channel:       "chat",
				SubjectKey:    subjectKey,
				Title:         req.Message,
				Summary:       content,
				Status:        "active",
				LastRunID:     runID,
				LastMessageAt: time.Now().UTC(),
			})
			_ = SummarizeRun(c.Request.Context(), tx.DB, runID)
			c.JSON(http.StatusOK, ChatResponse{Reply: content, RunID: runID})
			return

		case "tool_calls":
			toolCalls := safeGetSlice(msg, "tool_calls")
			if len(toolCalls) == 0 {
				// finish_reason says tool_calls but none present — treat as stop.
				if content != "" {
					c.JSON(http.StatusOK, ChatResponse{Reply: content, RunID: runID})
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
			toolResults := executeToolCallsForAI(auditCtx, tx, reg, toolCalls, currentUser, siteName, runID, stepID, conversationID)
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
			_ = AppendMessage(c.Request.Context(), tx.DB, siteName, conversationID, runID, "assistant", content, "summary", stepID, round+2)
			_ = UpdateStepStatus(c.Request.Context(), tx.DB, stepID, "failed", content, "", "", content, "length")
			_ = MarkTaskStatus(c.Request.Context(), tx.DB, stepID, "failed", "length")
			_ = UpsertRun(c.Request.Context(), tx.DB, RunRecord{
				ID:             runID,
				Site:           siteName,
				ConversationID: conversationID,
				Channel:        "chat",
				Status:         "failed",
				InputMessage:   req.Message,
				OutputMessage:  content,
				ErrorMessage:   "length",
				Model:          model,
				Provider:       providerKey,
			})
			c.JSON(http.StatusOK, ChatResponse{Reply: content, Action: "truncated", RunID: runID})
			return

		case "content_filter":
			c.JSON(http.StatusOK, ChatResponse{
				Reply: "I can't respond to that request due to content policies.",
				RunID: runID,
			})
			_ = UpdateStepStatus(c.Request.Context(), tx.DB, stepID, "failed", "content_filter", "", "", "", "content_filter")
			_ = MarkTaskStatus(c.Request.Context(), tx.DB, stepID, "failed", "content_filter")
			_ = UpsertRun(c.Request.Context(), tx.DB, RunRecord{
				ID:             runID,
				Site:           siteName,
				ConversationID: conversationID,
				Channel:        "chat",
				Status:         "failed",
				InputMessage:   req.Message,
				ErrorMessage:   "content_filter",
				Model:          model,
				Provider:       providerKey,
			})
			return

		default:
			// Unknown finish_reason. If there's content, return it.
			if content != "" {
				_ = AppendMessage(c.Request.Context(), tx.DB, siteName, conversationID, runID, "assistant", content, "summary", stepID, round+2)
				_ = UpdateStepStatus(c.Request.Context(), tx.DB, stepID, "completed", content, "", "", content, "")
				_ = MarkTaskStatus(c.Request.Context(), tx.DB, stepID, "done", content)
				_ = QueueFollowUpTasks(c.Request.Context(), tx.DB, siteName, runID, conversationID, deriveFollowUpTasks(content, toolResultsFromMessages(messages)))
				_ = UpsertRun(c.Request.Context(), tx.DB, RunRecord{
					ID:             runID,
					Site:           siteName,
					ConversationID: conversationID,
					Channel:        "chat",
					Status:         "completed",
					InputMessage:   req.Message,
					OutputMessage:  content,
					Summary:        content,
					Model:          model,
					Provider:       providerKey,
				})
				_ = SummarizeRun(c.Request.Context(), tx.DB, runID)
				c.JSON(http.StatusOK, ChatResponse{Reply: content, RunID: runID})
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
		RunID:  runID,
	})
	_ = UpsertRun(c.Request.Context(), tx.DB, RunRecord{
		ID:             runID,
		Site:           siteName,
		ConversationID: conversationID,
		Channel:        "chat",
		Status:         "failed",
		InputMessage:   req.Message,
		ErrorMessage:   "max_rounds_reached",
		Model:          model,
		Provider:       providerKey,
	})
	_ = SummarizeRun(c.Request.Context(), tx.DB, runID)
}

func estimatePromptTokens(messages []map[string]any, functions []map[string]any) int {
	totalChars := 0
	for _, msg := range messages {
		if content, ok := msg["content"].(string); ok {
			totalChars += len(content)
		}
	}
	for _, fn := range functions {
		if raw, err := json.Marshal(fn); err == nil {
			totalChars += len(raw)
		}
	}
	return totalChars / 4
}

func deriveFollowUpTasks(content string, toolResults []TaskRecord) []TaskRecord {
	parts := strings.Split(content, "\n")
	out := make([]TaskRecord, 0, len(parts)+len(toolResults))
	for i, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		out = append(out, TaskRecord{
			Kind:        "follow_up",
			Title:       part,
			Description: "Derived from the latest assistant response.",
			Status:      "queued",
			SortOrder:   i + 1,
			Notes:       "generated from run summary",
		})
	}
	for _, task := range toolResults {
		if task.Title == "" && task.Description == "" {
			continue
		}
		next := TaskRecord{
			Kind:        "tool_result",
			Title:       task.Title,
			Description: task.Description,
			Status:      "queued",
			SortOrder:   len(out) + 1,
			Notes:       task.Notes,
		}
		if next.Title == "" {
			next.Title = task.Description
		}
		out = append(out, next)
	}
	return out
}

func toolResultsFromMessages(messages []map[string]any) []TaskRecord {
	var out []TaskRecord
	for _, msg := range messages {
		if safeGetString(msg, "role") != "tool" {
			continue
		}
		content := safeGetString(msg, "content")
		if content == "" {
			continue
		}
		out = append(out, TaskRecord{
			Kind:        "tool_result",
			Title:       content,
			Description: content,
			Status:      "queued",
		})
	}
	return out
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
