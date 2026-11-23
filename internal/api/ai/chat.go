package ai

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/oklog/ulid/v2"

	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/orm"
	"github.com/asenawritescode/kora/internal/secret"
	"github.com/asenawritescode/kora/internal/tenanttime"
)

// ChatRequest is the JSON body for POST /api/v1/ai/chat.
type ChatRequest struct {
	Message      string      `json:"message"`
	Model        string      `json:"model,omitempty"` // override default model
	Context      ChatContext `json:"context,omitempty"`
	DisableTools bool        `json:"disable_tools,omitempty"` // allow safe planning without workspace mutations
}

// ChatContext carries lightweight UI context from the workspace shell.
type ChatContext struct {
	Pathname     string `json:"pathname,omitempty"`
	ShellMode    string `json:"shellMode,omitempty"`
	Doctype      string `json:"doctype,omitempty"`
	DocumentName string `json:"documentName,omitempty"`
	ThreadID     string `json:"threadId,omitempty"`
}

// ChatMessage is a single turn in the conversation.
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	// RunID is the run that wrote the message, so the workspace places its cards and choices under the right reply.
	RunID string `json:"run_id"`
}

// ConversationSubjectKey scopes a durable chat thread to one user and UI context.
func ConversationSubjectKey(currentUser string, context ChatContext) string {
	key := currentUser + ":" + context.Pathname + ":" + context.Doctype + ":" + context.DocumentName
	if context.ThreadID != "" {
		digest := sha256.Sum256([]byte(key + "\x00" + context.ThreadID))
		return "thread:" + hex.EncodeToString(digest[:])
	}
	return key
}

// ChatResponse is the data payload for POST /api/v1/ai/chat.
type ChatResponse struct {
	Reply  string `json:"reply"`
	Action string `json:"action,omitempty"` // what the AI did (e.g., "listed 3 customers")
	RunID  string `json:"run_id,omitempty"`
	// Cards are the answer cards of the reply, drawn from its query results.
	Cards []contract.AnswerCard `json:"cards,omitempty"`
	// Choices is set when the reply is a clarifying question: the reply is the question.
	Choices *contract.ChoiceQuestion `json:"choices,omitempty"`
	// Previews are the writes waiting for the person's yes; the reply is their card.
	Previews []previewCard `json:"pending_writes,omitempty"`
}

// followUpBlurb gives the model the question the conversation last answered, so a follow-up keeps its metric.
func followUpBlurb(question json.RawMessage) string {
	if len(question) == 0 {
		return ""
	}
	return "\n\nPREVIOUS METRICS QUESTION (for a follow-up: keep its metric and filters, and move its period as the person asks; say the exact dates you used):\n" + string(question)
}

func writeChatResponse(c *gin.Context, response ChatResponse) {
	c.JSON(http.StatusOK, gin.H{"data": response})
}

func siteCurrencyForChat(ctx context.Context, database *sql.DB, siteName string, dialect kdb.QueryDialect) (string, error) {
	var currency string
	err := database.QueryRowContext(ctx, kdb.Rebind(dialect, "SELECT setting_value FROM _kora_site_setting WHERE site = ? AND setting_key = ?"), siteName, "currency").Scan(&currency)
	if errors.Is(err, sql.ErrNoRows) {
		// Match GET /api/v1/system/settings, which defines this site's default.
		return "KES", nil
	}
	if err != nil {
		return "", err
	}
	currency = strings.ToUpper(strings.TrimSpace(currency))
	if currency == "" {
		return "KES", nil
	}
	return currency, nil
}

// pageContextBlurb describes the screen the person is on, for the model. It is empty without a page.
// businessDayBlurb tells the model the tenant's business day, so "today" and "this week" resolve in the shop's own
// zone and day cutoff. It sits in the turn context, so the standing prompt stays the same every day.
func businessDayBlurb(ctx context.Context, database *sql.DB, dialect kdb.QueryDialect, site string, now time.Time) string {
	clock := tenanttime.Default()
	if database != nil && dialect != nil {
		if loaded, err := tenanttime.Load(ctx, database, dialect, site); err == nil {
			clock = loaded
		}
	}
	day := clock.BusinessDate(now)
	weekday := time.Date(day.Year, day.Month, day.Day, 12, 0, 0, 0, time.UTC).Weekday()
	return fmt.Sprintf("Business day: %s, %s.", weekday, day)
}

func pageContextBlurb(page ChatContext) string {
	if page.Pathname == "" && page.Doctype == "" && page.DocumentName == "" {
		return ""
	}
	return fmt.Sprintf(`

CURRENT PAGE CONTEXT:
- pathname: %s
- shell mode: %s
- doctype: %s
- document: %s
Use this context to stay grounded in the current screen and avoid asking the user to repeat what is already visible.`,
		page.Pathname,
		page.ShellMode,
		page.Doctype,
		page.DocumentName,
	)
}

// chatSystemPrompt is the standing instruction set of the chat. It is the same for every turn of a site and role:
// the per-turn context (page, previews, follow-up) goes in its own message, see buildChatMessages.
const chatSystemPrompt = `You are Zana, the AI assistant in this business's workspace. You help people find, create and update records and understand numbers.

RULES
- Keep replies to one or two short sentences unless asked for more. Lists: five entries or fewer.
- Use the person's words. Never show tool names, codes, ids or error details.
- Numbers come only from tool results in this turn. Never estimate, round or invent a figure. Give exact dates.
- All fields in the tool schemas are available. Never say a field is not exposed.
- Find before you create: call find_records to check for a match.
- Before the first write of a record type, call describe_record_type.
- Writes preview first (create_record, update_record, run_action): nothing is saved yet.
- Never invent a value: ask for a missing one. Preview marks suggestions.
- After a preview, call confirm_write with the write_id only after a later yes.
- OPEN PREVIEWS are not saved yet; confirm one only when the person agrees.
- Pack actions: list_actions finds them; run_action previews. Leave payment references empty; confirm sets them.
- How many records exist: find_records gives the total. Metrics need a period.
- On missing_fields, ask one short question naming only the missing fields.
- On a tool error, say in one sentence what blocks it and how to fix it. Never guess values.
- Comma-separated values map to fields in sequence. Try them first.
- Ask before any delete or bulk change: name what will change and ask "Continue?".
- Show yes and no as ✅ or ❌.
- If the workspace cannot do something, say so in one sentence.

RECORD TYPES AND SCRIPTS
- Before drafting a record type, ask what it tracks, who uses it and what is required. Then ask "Create as draft?". Never show YAML.
- You never activate a draft; an administrator does, in Workspace → Admin → Versions.
- Write scripts only with script_validate and script_create. They are saved inactive; never say one is live. An administrator activates it in Workspace → Admin → Scripts.
- Scripts never bypass approvals or write direct database access.

NUMBERS AND ACCESS
- Business numbers come from the metrics tools. Explore and Insights show them too.
- Answer one question with one figure. Ask which metric or period only when two or more genuinely fit.
- If no metric covers a measure, say so in one sentence and give no number.
- A follow-up keeps the last metric and filters; only the period changes.
- Access is set in Workspace → Admin → Permissions.`

// turnContextMessage joins the parts of one turn's context into the message placed before the latest user message.
// Empty parts are dropped, and no text at all means the turn adds no context message.
func turnContextMessage(parts ...string) string {
	kept := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			kept = append(kept, part)
		}
	}
	return strings.Join(kept, "\n\n")
}

// buildChatMessages lays out one provider request: the standing system text, the conversation so far, the turn's
// context, then the latest user message. The turn context is a system message just before the user's message, so the
// standing text (and the tools that follow it in the request) stay byte-identical from turn to turn for prompt caching.
func buildChatMessages(standing string, history []ChatMessage, turnContext, message string) []map[string]any {
	messages := make([]map[string]any, 0, len(history)+3)
	messages = append(messages, map[string]any{"role": "system", "content": standing})
	for _, h := range history {
		messages = append(messages, map[string]any{"role": h.Role, "content": h.Content})
	}
	if turnContext != "" {
		messages = append(messages, map[string]any{"role": "system", "content": turnContext})
	}
	return append(messages, map[string]any{"role": "user", "content": message})
}

// chatFunctions is the tool list of one chat turn: the catalog, without the metrics reads when the person has no
// Insights, and on a channel only the tools that channel allows.
func chatFunctions(turn ChatTurn, reg *doctype.Registry, disabled bool) []map[string]any {
	if disabled {
		return nil
	}
	catalog := BuildToolCatalog(reg, CatalogOptions{Execution: turn.Tx.Execution, BusinessScope: turn.Tx.BusinessScope, Operations: turn.Operations})
	if turn.Channel != "" {
		catalog.Tools = toolsOnChannel(catalog.Tools, turn.Channel)
	}
	functions := buildOpenAIToolsFromCatalog(catalog)
	if !turn.Insights {
		functions = withoutMetricsTools(functions)
	}
	return functions
}

func toolsOnChannel(tools []ToolDescriptor, channel string) []ToolDescriptor {
	kept := make([]ToolDescriptor, 0, len(tools))
	for _, tool := range tools {
		if slices.Contains(tool.ChannelAllowlist, channel) {
			kept = append(kept, tool)
		}
	}
	return kept
}

// channelReplyStyle tells the model how a channel reads its replies. It is empty on the workspace chat, and it sits
// in the turn context, so the standing prompt stays the same for every channel.
func channelReplyStyle(channel string) string {
	if channel != "whatsapp" {
		return ""
	}
	return "REPLY STYLE: this reply goes to WhatsApp. Plain text only, no markdown. At most 300 characters. Offer choices as a numbered list."
}

// insightsNoteFor is what the model is told about Insights: nothing when the person has it.
func insightsNoteFor(insights bool) string {
	if insights {
		return ""
	}
	return insightsUnavailableNote
}

func financialDisplayContext(currency string) string {
	currency = strings.ToUpper(strings.TrimSpace(currency))
	if currency == "" {
		return `
FINANCIAL DISPLAY CONTEXT:
- The site's currency setting could not be read. Do not guess a currency or add a currency symbol/code to amounts. Ask for clarification when a currency is necessary.`
	}
	return fmt.Sprintf(`
FINANCIAL DISPLAY CONTEXT:
- The site's configured currency is %s.
- Use %s for monetary amounts that do not explicitly identify another currency, including analytics totals.
- Do not infer currency from locale, language, model defaults, or previous examples. Do not convert amounts unless the user asks for a conversion.`, currency, currency)
}

// openedConversation is the durable conversation a turn runs in and the transcript the model is given.
type openedConversation struct {
	ID         string
	Transcript []ChatMessage
	// LastQuestion is the metrics question the conversation last answered, nil when there is none.
	LastQuestion json.RawMessage
}

// conversationKey names the conversation of one turn: its site, its subject and the run that opens it.
type conversationKey struct {
	Site, SubjectKey, RunID, Message string
}

// openConversation loads the conversation of the subject, or creates it for this run. It answers the status and
// the message of the refusal when the conversation cannot be read or saved.
func openConversation(ctx context.Context, tx *orm.TxManager, key conversationKey) (openedConversation, int, string) {
	siteName, subjectKey, runID := key.Site, key.SubjectKey, key.RunID
	conv, err := LoadConversation(ctx, tx.DB, siteName, subjectKey)
	switch {
	case err == nil:
		transcript, listErr := ListConversationMessages(ctx, tx.DB, siteName, conv.ID)
		if listErr != nil {
			return openedConversation{}, http.StatusInternalServerError, "Unable to load AI conversation."
		}
		last, lastErr := lastAnswerQuestion(ctx, tx.DB, siteName, conv.ID)
		if lastErr != nil {
			return openedConversation{}, http.StatusInternalServerError, "Unable to load AI conversation."
		}
		return openedConversation{ID: conv.ID, Transcript: transcript, LastQuestion: last}, http.StatusOK, ""
	case errors.Is(err, sql.ErrNoRows):
		if err := UpsertConversation(ctx, tx.DB, ConversationRecord{
			ID:         runID,
			Site:       siteName,
			Channel:    "chat",
			SubjectKey: subjectKey,
			Title:      truncateScript(key.Message, 240),
			Status:     "active",
			LastRunID:  runID,
		}); err != nil {
			slog.Error("AI conversation persistence failed", "site", siteName, "error", err)
			return openedConversation{}, http.StatusInternalServerError, "Unable to persist AI conversation."
		}
		return openedConversation{ID: runID}, http.StatusOK, ""
	default:
		return openedConversation{}, http.StatusInternalServerError, "Unable to load AI conversation."
	}
}

// ServeChat serves one workspace chat turn: it decodes the request, runs the turn and writes the reply.
func ServeChat(c *gin.Context, turn ChatTurn) {
	var req ChatRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": gin.H{"message": "Invalid request format"},
		})
		return
	}
	outcome := HandleChat(c.Request.Context(), turn, req)
	if outcome.Failure != nil {
		c.JSON(outcome.Status, outcome.Failure)
		return
	}
	writeChatResponse(c, outcome.Reply)
}

// ChatOutcome is what one chat turn answered: the reply, or the status and error body of the refusal.
type ChatOutcome struct {
	Status  int
	Reply   ChatResponse
	Failure gin.H
	// Usage is the provider usage of the turn, summed over its rounds.
	Usage Usage
	// ConversationID is the conversation the turn ran in; empty when the turn was refused before it opened one.
	ConversationID string
}

func replyOutcome(reply ChatResponse) ChatOutcome {
	return ChatOutcome{Status: http.StatusOK, Reply: reply}
}

func failOutcome(status int, body gin.H) ChatOutcome {
	return ChatOutcome{Status: status, Failure: body}
}

// chatSiteCurrency reads the site's currency for the turn. A read failure is logged and the turn continues with no
// currency, which the system prompt tells the model not to guess.
func chatSiteCurrency(ctx context.Context, tx *orm.TxManager, siteName string) string {
	currency, err := siteCurrencyForChat(ctx, tx.DB, siteName, tx.Dialect)
	if err != nil {
		slog.Warn("AI chat could not read site currency", "site", siteName, "error", err)
	}
	return currency
}

// providerNotConfiguredOutcome is the refusal for a turn with no provider. A site that is connected to Cloud never
// gets the Secrets advice: its shared provider is Cloud's to reconnect.
func providerNotConfiguredOutcome() ChatOutcome {
	message := "AI chat needs a provider key. Add an OpenAI, DeepSeek, or Anthropic key in Workspace → Admin → Secrets, or ask your administrator whether a shared provider is configured."
	if _, connected := cloudGatewayURL(); connected {
		message = "Kigae Cloud's shared AI provider is unavailable. Ask your administrator to reconnect the Cloud provider."
	}
	return failOutcome(http.StatusServiceUnavailable, gin.H{
		"error": gin.H{
			"code":    "ai.provider_not_configured",
			"message": message,
		},
	})
}

// refusalFailure maps a Cloud gateway refusal to the chat failure the person sees: the gateway's status, code and
// message, passed through. ok is false for any other error.
func refusalFailure(err error) (outcome ChatOutcome, ok bool) {
	var refusal *gatewayRefusal
	if !errors.As(err, &refusal) {
		return ChatOutcome{}, false
	}
	return failOutcome(refusal.Status, gin.H{"error": gin.H{"code": refusal.Code, "message": refusal.Message}}), true
}

// HandleChat runs one chat turn for a decoded request: it persists the message, runs the provider and tool loop, and
// returns the outcome. The workspace chat and the channel chat both run it.
func HandleChat(ctx context.Context, turn ChatTurn, req ChatRequest) (out ChatOutcome) {
	tx, reg, siteName, currentUser := turn.Tx, turn.Registry, turn.Site, turn.User
	requestCtx := WithDialect(ctx, tx.Dialect)
	currency := chatSiteCurrency(requestCtx, tx, siteName)
	if len(req.Context.ThreadID) > 100 {
		return failOutcome(http.StatusBadRequest, gin.H{"error": gin.H{"message": "Invalid conversation identifier."}})
	}
	runID := ulid.Make().String()
	subjectKey := ConversationSubjectKey(currentUser, req.Context)
	auditCtx := enrichAuditContext(requestCtx, currentUser, turn.SessionID, turn.CorrelationID, turn.IdempotencyKey)

	// Read the configured AI provider key.
	providerKey, apiKey, baseURL, model := resolveProvider(tx.DB, siteName, req.Model, tx.Dialect)
	if apiKey == "" {
		out = providerNotConfiguredOutcome()
		return
	}

	// Load AI configuration (per-model defaults + site overrides).
	store := secret.NewStore(tx.DB, tx.Dialect)
	cfg := LoadAIConfig(store, siteName, model)
	if err := ValidateProviderProfile(ProviderProfile{
		ProviderKey:    providerKey,
		BaseURL:        baseURL,
		Model:          model,
		HTTPTimeoutSec: cfg.HTTPTimeoutSec,
	}); err != nil {
		out = failOutcome(http.StatusBadRequest, gin.H{
			"error": gin.H{"message": "Invalid AI provider configuration: " + err.Error()},
		})
		return
	}
	if err := EnsureAIRunTables(requestCtx, tx.DB, tx.Dialect); err != nil {
		slog.Error("AI run storage initialization failed", "site", siteName, "error", err)
		out = failOutcome(http.StatusInternalServerError, gin.H{"error": gin.H{"message": "Unable to initialize AI run storage."}})
		return
	}
	conv, convStatus, convMessage := openConversation(requestCtx, tx, conversationKey{Site: siteName, SubjectKey: subjectKey, RunID: runID, Message: req.Message})
	if convStatus != http.StatusOK {
		out = failOutcome(convStatus, gin.H{"error": gin.H{"message": convMessage}})
		return
	}
	conversationID, transcript := conv.ID, conv.Transcript
	turnContext := turnContextMessage(businessDayBlurb(requestCtx, tx.DB, tx.Dialect, siteName, time.Now()), pageContextBlurb(req.Context), followUpBlurb(conv.LastQuestion), turnPreviewsContext(requestCtx, turn, conversationID), channelReplyStyle(turn.Channel))
	if err := UpsertRun(requestCtx, tx.DB, RunRecord{
		ID:             runID,
		Site:           siteName,
		ConversationID: conversationID,
		Channel:        "chat",
		Status:         "planning",
		InputMessage:   req.Message,
		Model:          model,
		Provider:       providerKey,
	}); err != nil {
		out = failOutcome(http.StatusInternalServerError, gin.H{"error": gin.H{"message": "Unable to persist AI run."}})
		return
	}
	if err := AppendConversationMessage(requestCtx, tx.DB, MessageRecord{Site: siteName, ConversationID: conversationID, RunID: runID, Role: "user", Content: req.Message, Kind: "message"}); err != nil {
		out = failOutcome(http.StatusInternalServerError, gin.H{"error": gin.H{"message": "Unable to save your message."}})
		return
	}

	// Build function definitions from the canonical tool catalog projection.
	functions := chatFunctions(turn, reg, req.DisableTools)

	// Durable server history is the source of truth for the conversation.
	sanitizedHistory := sanitizeHistory(transcript, cfg.HistoryLimit)

	standing := chatSystemPrompt + financialDisplayContext(currency) + insightsNoteFor(turn.Insights)
	messages := buildChatMessages(standing, sanitizedHistory, turnContext, req.Message)

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
		out = failOutcome(http.StatusBadRequest, gin.H{
			"error": gin.H{"message": "AI request exceeds the configured token budget."},
		})
		return
	}
	reservation, err := ReserveBudget(requestCtx, tx.DB, BudgetRequest{Site: siteName, Model: model, RequestedTokens: estimatePromptTokens(messages, functions) + cfg.MaxTokensPerCall, Budget: cfg.TokenBudget, Note: "chat request"})
	if err != nil {
		out = failOutcome(http.StatusBadRequest, gin.H{
			"error": gin.H{"message": err.Error()},
		})
		return
	}
	defer func() {
		_ = ReleaseBudget(requestCtx, tx.DB, reservation)
	}()

	answers := newAnswerTurn(turn, currency, userWords(transcript, req.Message))

	// --- Multi-Round Tool Execution Loop ---
	var (
		runUsage   Usage // the provider usage of this run so far, summed over its rounds
		toolErrors int   // cumulative tool errors for circuit breaker
		stops      = newTurnStops(time.Now())
		stop       = stopMaxRounds // why the turn ends when no round answers; the loop sets it on every other stop
	)

	defer func() {
		out.Usage = runUsage
		out.ConversationID = conversationID
	}()

loop:
	for round := 0; round < cfg.MaxRounds; round++ {
		if reason, ok := stops.beforeRound(time.Now()); ok {
			stop = reason
			break loop
		}
		stepID := ulid.Make().String()
		_ = UpsertStep(requestCtx, tx.DB, StepRecord{ID: stepID, Site: siteName, RunID: runID, ConversationID: conversationID, StepKey: fmt.Sprintf("round-%d", round+1), Status: "planning"})
		_ = UpsertTask(requestCtx, tx.DB, TaskRecord{
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
		_ = UpsertRun(requestCtx, tx.DB, RunRecord{
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
		aiResp, err := callAIWithRetry(providerCall{
			BaseURL: baseURL,
			APIKey:  apiKey,
			Body:    aiBody,
			Config:  cfg,
			Headers: gatewayHeadersFor(providerKey, siteName, turn.Channel, runID),
			Record: func(attempt int, status string, latency time.Duration, reply providerReply, attemptErr error) {
				tokens := reply.Usage.tokenClasses()
				if status == "failed" {
					tokens[contract.UsageClassPartial] = 0
				}
				_ = RecordUsage(auditCtx, tx.DB, contract.UsageEvent{
					RunID:       runID,
					Site:        siteName,
					Model:       model,
					Provider:    providerKey,
					Attempt:     attempt,
					Status:      status,
					Tokens:      tokens,
					LatencyMs:   latency.Milliseconds(),
					OccurredAt:  time.Now().UTC(),
					Attribution: usageAttribution(reply.Model),
				})
			},
		})
		if err != nil {
			slog.Error("AI provider call failed", "error", err, "round", round)
			_ = UpdateStepStatus(requestCtx, tx.DB, stepID, StepUpdate{Status: "failed", Summary: err.Error(), ErrorMessage: err.Error()})
			_ = MarkTaskStatus(requestCtx, tx.DB, stepID, "failed", err.Error())
			_ = UpsertRun(requestCtx, tx.DB, RunRecord{
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
			// A gateway refusal ends the turn with the gateway's own code and message, on any round.
			if refused, ok := refusalFailure(err); ok {
				out = refused
				return
			}
			// On first-round failure, return an error.
			// On later rounds, return whatever tool results we've accumulated.
			if round == 0 {
				out = failOutcome(http.StatusInternalServerError, gin.H{
					"error": gin.H{"message": "AI provider error: " + err.Error()},
				})
				return
			}
			// Fallback: return the last assistant message content if available.
			fallbackReply := "I encountered an error while processing your request. Please try again."
			if lastContent := findLastAssistantContent(messages); lastContent != "" {
				fallbackReply = answers.finalReply(lastContent, req.Message)
			}
			_ = AppendConversationMessage(requestCtx, tx.DB, MessageRecord{Site: siteName, ConversationID: conversationID, RunID: runID, Role: "assistant", Content: fallbackReply, Kind: "summary", StepID: stepID})
			_ = UpdateStepStatus(requestCtx, tx.DB, stepID, StepUpdate{Status: "partial", Summary: fallbackReply, ErrorMessage: err.Error()})
			_ = MarkTaskStatus(requestCtx, tx.DB, stepID, "partial", err.Error())
			_ = UpsertRun(requestCtx, tx.DB, RunRecord{
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
			out = replyOutcome(ChatResponse{Reply: fallbackReply, Action: "partial", RunID: runID})
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
			out = failOutcome(http.StatusInternalServerError, gin.H{
				"error": gin.H{"message": respErr.Error()},
			})
			return
		}

		finishReason := safeGetString(choice, "finish_reason")
		content := safeGetString(msg, "content")

		// Sum the round's usage into the run. Without provider usage, estimate from the content: 4 chars ≈ 1 token.
		if usage := parseUsage(aiResp); usage.reported() {
			runUsage = runUsage.add(usage)
		} else {
			runUsage.Total += int64(len(content) / 4)
		}
		totalTokens := int(runUsage.Total)
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
		_ = FinalizeBudget(requestCtx, tx.DB, reservation, totalTokens)

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
			if fix := answers.correction(content, req.Message); fix != nil {
				messages = append(messages, fix...)
				aiBody["messages"] = messages
				continue
			}
			content = answers.finalReply(content, req.Message)
			completeAnswer(requestCtx, tx, finalAnswer{Site: siteName, ConversationID: conversationID, RunID: runID, StepID: stepID, Message: req.Message, SubjectKey: subjectKey, Model: model, Provider: providerKey, Content: content, Messages: messages, Cards: answers.cards(), Question: answers.lastQuestion()})
			out = replyOutcome(ChatResponse{Reply: content, RunID: runID, Cards: answers.cards()})
			return

		case "tool_calls":
			toolCalls := safeGetSlice(msg, "tool_calls")
			if len(toolCalls) == 0 {
				// finish_reason says tool_calls but none present — treat as stop.
				if content != "" {
					out = replyOutcome(ChatResponse{Reply: answers.finalReply(content, req.Message), RunID: runID, Cards: answers.cards()})
					return
				}
				continue
			}

			// --- Stall detection ---
			if stops.stallOn(toolCallSignatures(toolCalls)) {
				stop = stopStall
				break loop
			}
			if totalTokens > cfg.TokenBudget {
				stop = stopTokenBudget
				break loop
			}

			// --- Execute tools ---
			logToolCalls(toolCalls)
			toolResults := RunToolTurn(auditCtx, tx, reg, toolCallBatch{Calls: toolCalls, Owner: currentUser, Site: siteName, RunID: runID, StepID: stepID, ConversationID: conversationID, Execution: turn.Execution, Turn: answers, Commands: turn.Commands, Operations: turn.Operations})
			toolErrors += tallyToolResults(toolResults, cfg.MaxToolResultChars)

			// --- Clarifying question: the turn ends here, with the choices shown to the person. The question is
			// only known once its tool has run, so the check follows the execution, before the model is asked again. ---
			if answers.choice != nil {
				out = replyOutcome(endWithChoice(requestCtx, tx, answers.choice, finalAnswer{Site: siteName, ConversationID: conversationID, RunID: runID, StepID: stepID, Message: req.Message, Model: model, Provider: providerKey}))
				return
			}

			// --- Writes previewed: the turn ends with one card for them. The model is not asked again. ---
			if len(answers.previews) > 0 {
				out = replyOutcome(endWithPreviews(requestCtx, tx, answers.previews, finalAnswer{Site: siteName, ConversationID: conversationID, RunID: runID, StepID: stepID, Message: req.Message, SubjectKey: subjectKey, Model: model, Provider: providerKey}))
				return
			}

			if reason, ok := stops.afterResults(toolResults); ok {
				stop = reason
				break loop
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

			// --- Append assistant message + tool results for the next call ---
			messages = appendRound(messages, msg["content"], toolCalls, toolResults, functions)

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
			content = answers.finalReply(content, req.Message)
			_ = AppendConversationMessage(requestCtx, tx.DB, MessageRecord{Site: siteName, ConversationID: conversationID, RunID: runID, Role: "assistant", Content: content, Kind: "summary", StepID: stepID})
			_ = UpdateStepStatus(requestCtx, tx.DB, stepID, StepUpdate{Status: "failed", Summary: content, OutputJSON: content, ErrorMessage: "length"})
			_ = MarkTaskStatus(requestCtx, tx.DB, stepID, "failed", "length")
			_ = UpsertRun(requestCtx, tx.DB, RunRecord{
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
			out = replyOutcome(ChatResponse{Reply: content, Action: "truncated", RunID: runID, Cards: answers.cards()})
			return

		case "content_filter":
			out = replyOutcome(ChatResponse{
				Reply: "I can't respond to that request due to content policies.",
				RunID: runID,
			})
			_ = UpdateStepStatus(requestCtx, tx.DB, stepID, StepUpdate{Status: "failed", Summary: "content_filter", ErrorMessage: "content_filter"})
			_ = MarkTaskStatus(requestCtx, tx.DB, stepID, "failed", "content_filter")
			_ = UpsertRun(requestCtx, tx.DB, RunRecord{
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
				content = answers.finalReply(content, req.Message)
				_ = AppendConversationMessage(requestCtx, tx.DB, MessageRecord{Site: siteName, ConversationID: conversationID, RunID: runID, Role: "assistant", Content: content, Kind: "summary", StepID: stepID})
				_ = UpdateStepStatus(requestCtx, tx.DB, stepID, StepUpdate{Status: "completed", Summary: content, OutputJSON: content})
				_ = MarkTaskStatus(requestCtx, tx.DB, stepID, "done", content)
				_ = QueueFollowUpTasks(requestCtx, tx.DB, RunScope{Site: siteName, RunID: runID, ConversationID: conversationID}, deriveFollowUpTasks(content, toolResultsFromMessages(messages)))
				_ = UpsertRun(requestCtx, tx.DB, RunRecord{
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
				_ = SummarizeRun(requestCtx, tx.DB, runID)
				out = replyOutcome(ChatResponse{Reply: content, RunID: runID, Cards: answers.cards()})
				return
			}
			// Otherwise continue the loop.
			slog.Warn("Unknown finish_reason, continuing loop", "finish_reason", finishReason)
			continue
		}
	}

	// --- Stop: the turn ends with the reply its trace supports ---
	slog.Warn("Chat turn stopped", "reason", stop, "max_rounds", cfg.MaxRounds)
	stopText := stopReply(stop, stops)
	_ = AppendConversationMessage(requestCtx, tx.DB, MessageRecord{Site: siteName, ConversationID: conversationID, RunID: runID, Role: "assistant", Content: stopText, Kind: "summary"})
	out = replyOutcome(ChatResponse{
		Reply:  stopText,
		Action: stopAction(stop),
		RunID:  runID,
	})
	_ = UpsertRun(requestCtx, tx.DB, RunRecord{
		ID:             runID,
		Site:           siteName,
		ConversationID: conversationID,
		Channel:        "chat",
		Status:         "failed",
		InputMessage:   req.Message,
		ErrorMessage:   string(stop),
		Model:          model,
		Provider:       providerKey,
	})
	_ = SummarizeRun(requestCtx, tx.DB, runID)
	return
}

// stopAction is the chat response action of a stopped turn. The round cap keeps its own action name.
func stopAction(stop stopReason) string {
	if stop == stopMaxRounds {
		return "max_rounds_reached"
	}
	return "turn_stopped"
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
