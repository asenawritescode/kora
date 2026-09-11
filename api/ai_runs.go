package api

import (
	"database/sql"
	"net/http"
	"time"

	"github.com/asenawritescode/kora/api/ai"
	"github.com/gin-gonic/gin"
)

type aiCancelRequest struct {
	Reason string `json:"reason"`
}

type aiResumeRequest struct {
	ResumeToken string `json:"resume_token"`
}

type aiGrantApprovalRequest struct {
	GrantedBy string `json:"granted_by"`
}

type aiApprovalListItem struct {
	ID                 string    `json:"id"`
	OperationID        string    `json:"operation_id"`
	ActorPrincipalID   string    `json:"actor_principal_id"`
	ActorPrincipalType string    `json:"actor_principal_type"`
	ToolName           string    `json:"tool_name"`
	State              string    `json:"state"`
	TargetFingerprint  string    `json:"target_fingerprint"`
	ArgumentHash       string    `json:"argument_hash"`
	RecordVersion      int       `json:"record_version"`
	RequestedAt        time.Time `json:"requested_at"`
	ExpiresAt          time.Time `json:"expires_at"`
	GrantedAt          time.Time `json:"granted_at"`
	GrantedBy          string    `json:"granted_by"`
	AuthSessionID      string    `json:"auth_session_id"`
}

// HandleAICancel marks a durable AI run as cancelled.
func (h *Handler) HandleAICancel(c *gin.Context) {
	tx := h.siteTx(c)
	runID := c.Param("id")
	var req aiCancelRequest
	_ = c.ShouldBindJSON(&req)
	if err := ai.CancelRun(c.Request.Context(), tx.DB, runID, req.Reason); err != nil {
		writeError(c, http.StatusNotFound, "ai_run.not_found", err.Error(), nil)
		return
	}
	c.JSON(http.StatusOK, Response{Data: aiCancelResponse{RunID: runID, Status: "cancelled"}})
}

// HandleAIResume transitions a durable AI run back into planning if it is not terminal.
func (h *Handler) HandleAIResume(c *gin.Context) {
	tx := h.siteTx(c)
	runID := c.Param("id")
	var req aiResumeRequest
	_ = c.ShouldBindJSON(&req)
	rec, err := ai.ResumeRun(c.Request.Context(), tx.DB, runID, req.ResumeToken)
	if err != nil {
		writeError(c, http.StatusNotFound, "ai_run.not_found", err.Error(), nil)
		return
	}
	c.JSON(http.StatusOK, Response{Data: aiResumeResponse{
		RunID:       rec.ID,
		Status:      rec.Status,
		ResumeToken: rec.ResumeToken,
	}})
}

// HandleAIGrantApproval transitions a durable approval row from pending_approval to granted.
func (h *Handler) HandleAIGrantApproval(c *gin.Context) {
	tx := h.siteTx(c)
	approvalID := c.Param("id")
	var req aiGrantApprovalRequest
	_ = c.ShouldBindJSON(&req)
	if req.GrantedBy == "" {
		if user, ok := c.Get("user"); ok {
			if s, ok := user.(string); ok {
				req.GrantedBy = s
			}
		}
	}
	rec, err := ai.GrantApproval(c.Request.Context(), tx.DB, approvalID, req.GrantedBy)
	if err != nil {
		writeError(c, http.StatusNotFound, "ai_run.not_found", err.Error(), nil)
		return
	}
	run, err := ai.MarkRunPlanning(c.Request.Context(), tx.DB, rec.OperationID)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "ai_run.failed", err.Error(), nil)
		return
	}
	c.JSON(http.StatusOK, Response{Data: aiGrantApprovalResponse{
		ApprovalID: rec.ID,
		State:      rec.State,
		GrantedBy:  rec.GrantedBy,
		RunID:      run.ID,
		RunStatus:  run.Status,
	}})
}

// HandleAIListApprovals returns approval rows for the current site.
func (h *Handler) HandleAIListApprovals(c *gin.Context) {
	tx := h.siteTx(c)
	if err := ai.EnsureAIRunTables(c.Request.Context(), tx.DB); err != nil {
		writeError(c, http.StatusInternalServerError, "ai_run.failed", err.Error(), nil)
		return
	}
	state := c.Query("state")
	if state == "" {
		state = "pending_approval"
	}
	rows, err := tx.DB.QueryContext(c.Request.Context(), `
SELECT id, operation_id, actor_principal_id, actor_principal_type, tool_name, state, target_fingerprint, argument_hash, record_version, requested_at, expires_at, granted_at, granted_by, auth_session_id
FROM _kora_ai_approval
WHERE site = ? AND state = ?
ORDER BY requested_at DESC`, c.GetString("site_name"), state)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "ai_run.failed", err.Error(), nil)
		return
	}
	defer rows.Close()
	items := make([]aiApprovalListItem, 0)
	for rows.Next() {
		var item aiApprovalListItem
		var expiresAt, grantedAt sql.NullTime
		if err := rows.Scan(&item.ID, &item.OperationID, &item.ActorPrincipalID, &item.ActorPrincipalType, &item.ToolName, &item.State, &item.TargetFingerprint, &item.ArgumentHash, &item.RecordVersion, &item.RequestedAt, &expiresAt, &grantedAt, &item.GrantedBy, &item.AuthSessionID); err != nil {
			writeError(c, http.StatusInternalServerError, "ai_run.failed", err.Error(), nil)
			return
		}
		if expiresAt.Valid {
			item.ExpiresAt = expiresAt.Time
		}
		if grantedAt.Valid {
			item.GrantedAt = grantedAt.Time
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		writeError(c, http.StatusInternalServerError, "ai_run.failed", err.Error(), nil)
		return
	}
	c.JSON(http.StatusOK, Response{Data: items})
}

// HandleAIRetentionCleanup removes expired AI conversations, runs, messages, and steps.
func (h *Handler) HandleAIRetentionCleanup(c *gin.Context) {
	tx := h.siteTx(c)
	removed, err := ai.CleanupExpired(c.Request.Context(), tx.DB, time.Now().UTC())
	if err != nil {
		writeError(c, http.StatusInternalServerError, "ai_run.failed", err.Error(), nil)
		return
	}
	c.JSON(http.StatusOK, Response{Data: aiRetentionCleanupResponse{Removed: removed}})
}
