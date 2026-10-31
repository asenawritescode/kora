package api

import (
	"database/sql"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/asenawritescode/kora/api/ai"
)

// HandleChat processes one turn through the durable AI chat workflow.
func (h *Handler) HandleChat(c *gin.Context) {
	reg := h.siteRegistry(c)
	siteNameRaw, _ := c.Get("site_name")
	siteName, _ := siteNameRaw.(string)
	tx := h.siteTx(c)

	currentUser, verified := requestUserID(c)
	if !verified {
		writeError(c, http.StatusUnauthorized, "auth.execution_required", "Verified execution identity is required.", nil)
		return
	}

	ai.HandleChat(c, tx, reg, siteName, currentUser)
}

type aiChatConversationResponse struct {
	ConversationID string           `json:"conversation_id,omitempty"`
	Messages       []ai.ChatMessage `json:"messages"`
	LastRunID      string           `json:"last_run_id,omitempty"`
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
	c.JSON(http.StatusOK, Response{Data: aiChatConversationResponse{
		ConversationID: conversation.ID,
		Messages:       messages,
		LastRunID:      conversation.LastRunID,
	}})
}
