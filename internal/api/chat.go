package api

import (
	"database/sql"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/asenawritescode/kora/internal/api/ai"
)

// HandleChat processes one turn through the durable AI chat workflow.
func (h *Handler) HandleChat(c *gin.Context) {
	turn, ok := h.chatTurn(c)
	if !ok {
		return
	}
	ai.ServeChat(c, turn)
}

// chatTurn is the request's input to a chat turn, shared by the workspace chat and the channel chat: the tenant
// transaction scoped to the request, the verified identity and the handler's services. It writes the refusal and
// returns false when the request has no tenant database or no verified identity.
func (h *Handler) chatTurn(c *gin.Context) (ai.ChatTurn, bool) {
	reg := h.siteRegistry(c)
	siteNameRaw, _ := c.Get("site_name")
	siteName, _ := siteNameRaw.(string)
	baseTx := h.siteTx(c)
	if baseTx == nil {
		writeError(c, http.StatusServiceUnavailable, "database.unavailable", "Tenant database is unavailable.", nil)
		return ai.ChatTurn{}, false
	}
	txCopy := *baseTx
	tx := &txCopy
	execution, hasExecution := requestExecution(c)
	if hasExecution {
		tx.BusinessScope = businessScopeFromRequest(c, execution)
	}

	currentUser, verified := requestUserID(c)
	if !verified {
		writeError(c, http.StatusUnauthorized, "auth.execution_required", "Verified execution identity is required.", nil)
		return ai.ChatTurn{}, false
	}
	return ai.ChatTurn{Tx: tx, Registry: reg, Site: siteName, User: currentUser, Metrics: h.metricsRunner(c),
		Insights: insightsAllowed(c, reg), Commands: h.kernelCommands(h.runtimeService(c, siteName)), Operations: NewAIModuleOperations(h, c),
		Execution: execution, SessionID: c.GetString("session_sid"), CorrelationID: c.GetString("correlation_id"), IdempotencyKey: c.GetString("idempotency_key")}, true
}

type aiChatConversationResponse struct {
	ConversationID string           `json:"conversation_id,omitempty"`
	Messages       []ai.ChatMessage `json:"messages"`
	LastRunID      string           `json:"last_run_id,omitempty"`
	// Attachments are the answer cards and clarifying questions of the transcript, by run.
	Attachments []ai.AnswerAttachment `json:"attachments,omitempty"`
}

// HandleAIChatConversation returns the persisted transcript for the current
// user and page context. The server owns transcript history; clients only send
// the new turn and its current page context.
func (h *Handler) HandleAIChatConversation(c *gin.Context) {
	tx := h.siteTx(c)
	siteName := c.GetString("site_name")
	currentUser, verified := requestUserID(c)
	if !verified {
		writeError(c, http.StatusUnauthorized, "auth.execution_required", "Verified execution identity is required.", nil)
		return
	}
	ctx := ai.WithDialect(c.Request.Context(), tx.Dialect)
	if err := ai.EnsureAIRunTables(ctx, tx.DB, tx.Dialect); err != nil {
		writeError(c, http.StatusInternalServerError, "ai_run.failed", "Unable to load AI conversation.", nil)
		return
	}
	chatContext := ai.ChatContext{
		Pathname:     c.Query("pathname"),
		ShellMode:    c.Query("shellMode"),
		Doctype:      c.Query("doctype"),
		DocumentName: c.Query("documentName"),
		ThreadID:     c.Query("threadId"),
	}
	if len(chatContext.ThreadID) > 100 {
		writeError(c, http.StatusBadRequest, "ai_run.invalid_conversation", "Invalid conversation identifier.", nil)
		return
	}
	subjectKey := ai.ConversationSubjectKey(currentUser, chatContext)
	conversation, err := ai.LoadConversation(ctx, tx.DB, siteName, subjectKey)
	if err == sql.ErrNoRows {
		c.JSON(http.StatusOK, Response{Data: aiChatConversationResponse{Messages: []ai.ChatMessage{}}})
		return
	}
	if err != nil {
		writeError(c, http.StatusInternalServerError, "ai_run.failed", "Unable to load AI conversation.", nil)
		return
	}
	messages, err := ai.ListConversationMessages(ctx, tx.DB, siteName, conversation.ID)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "ai_run.failed", "Unable to load AI conversation.", nil)
		return
	}
	attachments, err := ai.ListAnswerAttachments(ctx, tx.DB, siteName, conversation.ID)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "ai_run.failed", "Unable to load AI conversation.", nil)
		return
	}
	c.JSON(http.StatusOK, Response{Data: aiChatConversationResponse{
		ConversationID: conversation.ID,
		Messages:       messages,
		LastRunID:      conversation.LastRunID,
		Attachments:    attachments,
	}})
}
