package api

import (
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/asenawritescode/kora/conversation"
	"github.com/gin-gonic/gin"
)

type conversationCreateRequest struct {
	Channel           string `json:"channel"`
	ExternalContactID string `json:"external_contact_id"`
	CorrelationID     string `json:"correlation_id"`
}
type conversationMessageRequest struct {
	Text        string `json:"text"`
	QuestionKey string `json:"question_key"`
}
type supportRequest struct {
	Reason  string `json:"reason"`
	Confirm bool   `json:"confirm"`
}

func (h *Handler) conversationStore(c *gin.Context) conversation.Store {
	return conversation.Store{DB: h.siteTx(c).DB, Rebind: func(q string) string { return h.siteQuery(c, q) }}
}
func (h *Handler) conversationTenant(c *gin.Context) string { return c.GetString("site_name") }

func storeQuery(store conversation.Store, query string) string {
	if store.Rebind != nil {
		return store.Rebind(query)
	}
	return query
}

func (h *Handler) HandleConversationCreate(c *gin.Context) {
	var req conversationCreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequestError(c, "conversation.invalid_json", "I couldn't start that conversation yet.", nil)
		return
	}
	if req.CorrelationID == "" {
		req.CorrelationID = c.GetHeader("Idempotency-Key")
	}
	if req.CorrelationID == "" {
		req.CorrelationID = conversation.ID("cor")
	}
	store := h.conversationStore(c)
	// Correlation IDs are idempotency keys scoped to the tenant.
	var existing string
	err := h.siteTx(c).DB.QueryRow(h.siteQuery(c, `SELECT id FROM _kora_conversation WHERE tenant_id=? AND correlation_id=?`), h.conversationTenant(c), req.CorrelationID).Scan(&existing)
	if err == nil {
		conv, getErr := store.Get(h.conversationTenant(c), existing)
		if getErr == nil {
			c.JSON(http.StatusOK, Response{Data: conv})
			return
		}
	}
	conv, err := store.Create(h.conversationTenant(c), req.Channel, req.ExternalContactID, req.CorrelationID)
	if err != nil {
		internalError(c, "starting conversation", err)
		return
	}
	_, _ = store.AddMessage(conv.TenantID, conv.ID, "assistant", "Let’s build a workspace around the way your team works. I’ll ask a few short questions, then prepare your first workflow, records, team access, and approvals. No technical setup required.", string(conversation.TopicBusiness), "")
	c.JSON(http.StatusCreated, Response{Data: conv})
}

func (h *Handler) HandleConversationGet(c *gin.Context) {
	store := h.conversationStore(c)
	tenant := h.conversationTenant(c)
	conv, err := store.Get(tenant, c.Param("id"))
	if err != nil {
		notFoundError(c, "conversation.not_found", "Conversation not found", nil)
		return
	}
	messages, err := store.Messages(tenant, conv.ID)
	if err != nil {
		internalError(c, "loading conversation", err)
		return
	}
	c.JSON(http.StatusOK, Response{Data: gin.H{"conversation": conv, "messages": messages, "progress": conversationProgress(store, tenant, conv)}})
}

func conversationProgress(store conversation.Store, tenant string, conv conversation.Conversation) conversation.Progress {
	completed := []conversation.Topic{}
	current := conversation.TopicBusiness
	rows, err := store.DB.Query(storeQuery(store, `SELECT question_key, confirmation_state FROM _kora_interview_answer a JOIN _kora_conversation c ON c.id=a.conversation_id WHERE c.tenant_id=? AND a.conversation_id=?`), tenant, conv.ID)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var key, state string
			if rows.Scan(&key, &state) == nil && state == "confirmed" {
				completed = append(completed, conversation.Topic(key))
				current = conversation.NextTopic(conversation.Topic(key))
			}
		}
	}
	if current == conversation.TopicReady || conv.Status == "completed" {
		current = conversation.TopicReady
	}
	return conversation.Progress{Current: current, Completed: completed}
}

func topicPrompt(topic conversation.Topic) string {
	switch topic {
	case conversation.TopicBusiness:
		return "What would you like to make easier in your day-to-day work?"
	case conversation.TopicGoal:
		return "What is the main outcome you want from that work?"
	case conversation.TopicRecords:
		return "What information or records do you need to keep track of?"
	case conversation.TopicProcess:
		return "What are the usual steps, including anything that needs a review or approval?"
	case conversation.TopicTeam:
		return "Who should be able to view, create, approve, or manage this work?"
	default:
		return "Your workspace is ready to review. We prepared a first version from your conversation."
	}
}

func (h *Handler) HandleConversationMessage(c *gin.Context) {
	var req conversationMessageRequest
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Text) == "" {
		badRequestError(c, "conversation.message_required", "Tell me a little more in your own words.", nil)
		return
	}
	store := h.conversationStore(c)
	tenant := h.conversationTenant(c)
	conv, err := store.Get(tenant, c.Param("id"))
	if err != nil {
		notFoundError(c, "conversation.not_found", "Conversation not found", nil)
		return
	}
	if conv.Status == "completed" {
		conflictError(c, "conversation.read_only", "This conversation is complete and read-only.", nil)
		return
	}
	key := req.QuestionKey
	if key == "" {
		key = string(conv.CurrentTopic)
	}
	interpretation := "I heard: " + strings.TrimSpace(req.Text) + ". I’ll use this as a starting point and keep it editable."
	userMsg, err := store.AddMessage(tenant, conv.ID, "user", strings.TrimSpace(req.Text), key, "")
	if err != nil {
		internalError(c, "saving conversation message", err)
		return
	}
	_ = store.SaveAnswer(tenant, conv.ID, conversation.Answer{QuestionKey: key, OriginalText: req.Text, Confidence: .75, SourceMessageID: userMsg.ID, ConfirmationState: "pending", FeedbackShown: interpretation})
	assistant, err := store.AddMessage(tenant, conv.ID, "assistant", interpretation, key, interpretation)
	if err != nil {
		internalError(c, "saving conversation reply", err)
		return
	}
	c.JSON(http.StatusOK, Response{Data: gin.H{"message": assistant, "interpretation": interpretation, "next_question": topicPrompt(conv.CurrentTopic), "progress": conversationProgress(store, tenant, conv)}})
}

func (h *Handler) HandleConversationConfirm(c *gin.Context) {
	tenant := h.conversationTenant(c)
	_, err := h.siteTx(c).DB.Exec(h.siteQuery(c, `UPDATE _kora_interview_answer SET confirmation_state='confirmed' WHERE conversation_id=? AND question_key=? AND EXISTS (SELECT 1 FROM _kora_conversation WHERE id=? AND tenant_id=?)`), c.Param("id"), c.Param("key"), c.Param("id"), tenant)
	if err != nil {
		internalError(c, "confirming answer", err)
		return
	}
	store := h.conversationStore(c)
	conv, err := store.Get(tenant, c.Param("id"))
	if err != nil {
		notFoundError(c, "conversation.not_found", "Conversation not found", nil)
		return
	}
	next := conversation.NextTopic(conv.CurrentTopic)
	status := string(conv.Status)
	if next == conversation.TopicReady {
		status = "ready_for_review"
	}
	_, err = store.DB.Exec(storeQuery(store, `UPDATE _kora_conversation SET current_topic=?, status=? WHERE tenant_id=? AND id=?`), next, status, tenant, conv.ID)
	if err != nil {
		internalError(c, "advancing conversation", err)
		return
	}
	conv.CurrentTopic = next
	conv.Status = status
	c.JSON(http.StatusOK, Response{Data: gin.H{"progress": conversationProgress(store, tenant, conv), "next_question": topicPrompt(next)}})
}

func (h *Handler) HandleConversationProgress(c *gin.Context) {
	store := h.conversationStore(c)
	conv, err := store.Get(h.conversationTenant(c), c.Param("id"))
	if err != nil {
		notFoundError(c, "conversation.not_found", "Conversation not found", nil)
		return
	}
	c.JSON(http.StatusOK, Response{Data: conversationProgress(store, h.conversationTenant(c), conv)})
}

func (h *Handler) HandleConversationSupport(c *gin.Context) {
	var req supportRequest
	if err := c.ShouldBindJSON(&req); err != nil || !req.Confirm {
		badRequestError(c, "support.confirmation_required", "Please confirm before sending this conversation to Kora support.", nil)
		return
	}
	tenant := h.conversationTenant(c)
	conv, err := h.conversationStore(c).Get(tenant, c.Param("id"))
	if err != nil {
		notFoundError(c, "conversation.not_found", "Conversation not found", nil)
		return
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" {
		reason = "Help with workspace setup"
	}
	safe := redactSupport(reason)
	_, err = h.siteTx(c).DB.Exec(h.siteQuery(c, `INSERT INTO _kora_support_ticket (id,conversation_id,tenant_id,reason,safe_summary,correlation_id,status,created_at) VALUES (?,?,?,?,?,?,?,?)`), conversation.ID("ticket"), conv.ID, tenant, reason, safe, conv.CorrelationID, "created", time.Now().UTC())
	if err != nil {
		internalError(c, "creating support ticket", err)
		return
	}
	c.JSON(http.StatusCreated, Response{Data: gin.H{"status": "created", "message": "We’ll send a short summary to the Kora team so they can help without making you repeat yourself.", "correlation_id": conv.CorrelationID}})
}

var secretPattern = regexp.MustCompile(`(?i)(password|secret|token|api[_ -]?key)\s*[:=]\s*[^,\s]+`)

func redactSupport(s string) string { return secretPattern.ReplaceAllString(s, "$1: [redacted]") }

func RegisterConversationRoutes(group *gin.RouterGroup, h *Handler) {
	cloud := group.Group("/cloud")
	cloud.POST("/conversations", h.HandleConversationCreate)
	cloud.GET("/conversations/:id", h.HandleConversationGet)
	cloud.POST("/conversations/:id/messages", h.HandleConversationMessage)
	cloud.POST("/conversations/:id/answers/:key/confirm", h.HandleConversationConfirm)
	cloud.GET("/conversations/:id/progress", h.HandleConversationProgress)
	cloud.POST("/conversations/:id/support-ticket", h.HandleConversationSupport)
}
