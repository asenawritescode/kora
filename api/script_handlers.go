package api

import (
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/asenawritescode/kora/script"
	"github.com/gin-gonic/gin"
)

// HandleScriptList returns all scripts for the current site.
func (h *Handler) HandleScriptList(c *gin.Context) {
	siteName, _ := c.Get("site_name")
	siteNameStr, _ := siteName.(string)

	if h.SiteScriptStores == nil {
		c.JSON(http.StatusOK, Response{Data: []any{}})
		return
	}
	store, exists := h.SiteScriptStores[siteNameStr]
	if !exists || store == nil {
		c.JSON(http.StatusOK, Response{Data: []any{}})
		return
	}

	// Load all scripts for this site (all types, all doctypes).
	scripts, err := store.LoadAllForSite(siteNameStr)
	if err != nil {
		slog.Error("listing scripts", "site", siteNameStr, "error", err)
		c.JSON(http.StatusInternalServerError, ErrorResponse{
			Error: map[string]string{"message": "Failed to list scripts"},
		})
		return
	}
	if scripts == nil {
		scripts = []script.ScriptRecord{}
	}

	c.JSON(http.StatusOK, Response{Data: scripts})
}

// HandleScriptGet returns a single script.
func (h *Handler) HandleScriptGet(c *gin.Context) {
	siteName, _ := c.Get("site_name")
	siteNameStr, _ := siteName.(string)
	name := c.Param("name")

	if h.SiteScriptStores == nil {
		c.JSON(http.StatusNotFound, ErrorResponse{Error: map[string]string{"message": "Script not found"}})
		return
	}
	store, exists := h.SiteScriptStores[siteNameStr]
	if !exists || store == nil {
		c.JSON(http.StatusNotFound, ErrorResponse{Error: map[string]string{"message": "Script not found"}})
		return
	}

	rec, err := store.LoadByName(siteNameStr, name)
	if err != nil {
		c.JSON(http.StatusInternalServerError, ErrorResponse{Error: map[string]string{"message": "Failed to load script"}})
		return
	}
	if rec == nil {
		c.JSON(http.StatusNotFound, ErrorResponse{Error: map[string]string{"message": "Script not found"}})
		return
	}

	c.JSON(http.StatusOK, Response{Data: rec})
}

// HandleScriptCreate creates a new script.
func (h *Handler) HandleScriptCreate(c *gin.Context) {
	siteName, _ := c.Get("site_name")
	siteNameStr, _ := siteName.(string)
	user, _ := c.Get("user")
	userStr, _ := user.(string)

	if h.SiteScriptStores == nil {
		c.JSON(http.StatusInternalServerError, ErrorResponse{Error: map[string]string{"message": "Script store not available"}})
		return
	}
	store, exists := h.SiteScriptStores[siteNameStr]
	if !exists || store == nil {
		c.JSON(http.StatusInternalServerError, ErrorResponse{Error: map[string]string{"message": "Script store not available"}})
		return
	}

	var req struct {
		Name          string `json:"name"`
		ScriptType    string `json:"script_type"`
		DocType       string `json:"doctype"`
		Event         string `json:"event"`
		MethodPath    string `json:"method_path"`
		WorkflowAction string `json:"workflow_action"`
		Schedule      string `json:"schedule"`
		Priority      int    `json:"priority"`
		RunAs         string `json:"run_as"`
		TimeoutMs     int    `json:"timeout_ms"`
		Script        string `json:"script"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: map[string]string{"message": "Invalid request body"}})
		return
	}
	if req.Name == "" || req.Script == "" {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: map[string]string{"message": "name and script are required"}})
		return
	}
	if req.ScriptType == "" {
		req.ScriptType = "doc_event"
	}

	// Validate required fields per script type.
	switch req.ScriptType {
	case "doc_event":
		if req.DocType == "" {
			c.JSON(http.StatusBadRequest, ErrorResponse{Error: map[string]string{"message": "doctype is required for doc_event scripts"}})
			return
		}
		if req.Event == "" {
			c.JSON(http.StatusBadRequest, ErrorResponse{Error: map[string]string{"message": "event is required for doc_event scripts"}})
			return
		}
		// Validate event is a known hook point.
		validEvents := map[string]bool{
			"before_insert": true, "after_insert": true,
			"before_save": true, "after_save": true,
			"before_delete": true, "after_delete": true,
			"before_submit": true, "after_submit": true,
			"before_cancel": true, "after_cancel": true,
			"validate": true, "computed": true,
		}
		if !validEvents[req.Event] {
			c.JSON(http.StatusBadRequest, ErrorResponse{Error: map[string]string{"message": fmt.Sprintf("unknown event %q. Valid events: before_save, after_save, validate, computed, etc.", req.Event)}})
			return
		}
	case "api_method":
		if req.MethodPath == "" {
			c.JSON(http.StatusBadRequest, ErrorResponse{Error: map[string]string{"message": "method_path is required for api_method scripts"}})
			return
		}
	case "workflow_action":
		if req.WorkflowAction == "" {
			c.JSON(http.StatusBadRequest, ErrorResponse{Error: map[string]string{"message": "workflow_action is required for workflow_action scripts"}})
			return
		}
	case "scheduled":
		if req.Schedule == "" {
			c.JSON(http.StatusBadRequest, ErrorResponse{Error: map[string]string{"message": "schedule is required for scheduled scripts"}})
			return
		}
	}
	if req.Priority <= 0 {
		req.Priority = 10
	}
	if req.TimeoutMs <= 0 {
		req.TimeoutMs = 5000
	}

	// Validate the script compiles.
	if h.ScriptRunner != nil {
		if err := h.ScriptRunner.Validate(req.Script); err != nil {
			c.JSON(http.StatusBadRequest, ErrorResponse{
				Error: map[string]string{"message": fmt.Sprintf("Script validation failed: %v", err)},
			})
			return
		}
	}

	// Check for hardcoded secrets.
	if issues := detectHardcodedSecrets(req.Script); len(issues) > 0 {
		slog.Warn("script may contain hardcoded secrets", "script", req.Name, "issues", issues)
	}

	rec := script.ScriptRecord{
		Name:           req.Name,
		Site:           siteNameStr,
		ScriptType:     script.Type(req.ScriptType),
		DocType:        req.DocType,
		Event:          script.Event(req.Event),
		MethodPath:     req.MethodPath,
		WorkflowAction: req.WorkflowAction,
		Schedule:       req.Schedule,
		Priority:       req.Priority,
		IsActive:       false, // Always create as inactive — user must explicitly activate.
		RunAs:          req.RunAs,
		TimeoutMs:      req.TimeoutMs,
		Script:         req.Script,
		CreatedBy:      userStr,
		UpdatedBy:      userStr,
		CreatedAt:      time.Now(),
		UpdatedAt:      time.Now(),
	}

	if err := store.Insert(rec); err != nil {
		slog.Error("creating script", "name", req.Name, "error", err)
		c.JSON(http.StatusInternalServerError, ErrorResponse{Error: map[string]string{"message": "Failed to create script"}})
		return
	}

	slog.Info("script created", "name", req.Name, "type", req.ScriptType, "site", siteNameStr)
	c.JSON(http.StatusCreated, Response{Data: rec})
}

// HandleScriptUpdate updates an existing script.
func (h *Handler) HandleScriptUpdate(c *gin.Context) {
	siteName, _ := c.Get("site_name")
	siteNameStr, _ := siteName.(string)
	user, _ := c.Get("user")
	userStr, _ := user.(string)
	name := c.Param("name")

	if h.SiteScriptStores == nil {
		c.JSON(http.StatusNotFound, ErrorResponse{Error: map[string]string{"message": "Script not found"}})
		return
	}
	store, exists := h.SiteScriptStores[siteNameStr]
	if !exists || store == nil {
		c.JSON(http.StatusNotFound, ErrorResponse{Error: map[string]string{"message": "Script not found"}})
		return
	}

	var req struct {
		ScriptType    *string `json:"script_type"`
		DocType       *string `json:"doctype"`
		Event         *string `json:"event"`
		MethodPath    *string `json:"method_path"`
		WorkflowAction *string `json:"workflow_action"`
		Schedule      *string `json:"schedule"`
		Priority      *int    `json:"priority"`
		IsActive      *bool   `json:"is_active"`
		RunAs         *string `json:"run_as"`
		TimeoutMs     *int    `json:"timeout_ms"`
		Script        *string `json:"script"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: map[string]string{"message": "Invalid request body"}})
		return
	}

	// Apply updates.
	if req.Script != nil {
		if h.ScriptRunner != nil {
			if err := h.ScriptRunner.Validate(*req.Script); err != nil {
				c.JSON(http.StatusBadRequest, ErrorResponse{
					Error: map[string]string{"message": fmt.Sprintf("Script validation failed: %v", err)},
				})
				return
			}
		}
	}
	updateReq := script.ScriptUpdateRequest{
		ScriptType: req.ScriptType, DocType: req.DocType, Event: req.Event,
		MethodPath: req.MethodPath, WorkflowAction: req.WorkflowAction,
		Schedule: req.Schedule, Priority: req.Priority, IsActive: req.IsActive,
		RunAs: req.RunAs, TimeoutMs: req.TimeoutMs, Script: req.Script,
	}
	if err := store.Update(siteNameStr, name, updateReq, userStr); err != nil {
		slog.Error("updating script", "name", name, "error", err)
		c.JSON(http.StatusInternalServerError, ErrorResponse{Error: map[string]string{"message": "Failed to update script"}})
		return
	}

	c.JSON(http.StatusOK, Response{Data: map[string]string{"status": "ok"}})
}

// HandleScriptDelete deletes a script.
func (h *Handler) HandleScriptDelete(c *gin.Context) {
	siteName, _ := c.Get("site_name")
	siteNameStr, _ := siteName.(string)
	name := c.Param("name")

	if h.SiteScriptStores == nil {
		c.JSON(http.StatusNotFound, ErrorResponse{Error: map[string]string{"message": "Script not found"}})
		return
	}
	store, exists := h.SiteScriptStores[siteNameStr]
	if !exists || store == nil {
		c.JSON(http.StatusNotFound, ErrorResponse{Error: map[string]string{"message": "Script not found"}})
		return
	}

	if err := store.Delete(siteNameStr, name); err != nil {
		slog.Error("deleting script", "name", name, "error", err)
		c.JSON(http.StatusInternalServerError, ErrorResponse{Error: map[string]string{"message": "Failed to delete script"}})
		return
	}

	c.JSON(http.StatusOK, Response{Data: map[string]string{"status": "deleted"}})
}

// HandleScriptValidate validates a script without saving it.
func (h *Handler) HandleScriptValidate(c *gin.Context) {
	var req struct {
		Script string `json:"script"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: map[string]string{"message": "Invalid request body"}})
		return
	}
	if req.Script == "" {
		c.JSON(http.StatusBadRequest, ErrorResponse{Error: map[string]string{"message": "script is required"}})
		return
	}

	if h.ScriptRunner == nil {
		c.JSON(http.StatusInternalServerError, ErrorResponse{Error: map[string]string{"message": "Script runner not available"}})
		return
	}

	if err := h.ScriptRunner.Validate(req.Script); err != nil {
		c.JSON(http.StatusOK, Response{
			Data: map[string]any{"valid": false, "error": err.Error()},
		})
		return
	}

	issues := detectHardcodedSecrets(req.Script)
	c.JSON(http.StatusOK, Response{
		Data: map[string]any{"valid": true, "warnings": issues},
	})
}

// HandleScriptExecutions returns the execution log for a script.
func (h *Handler) HandleScriptExecutions(c *gin.Context) {
	siteName, _ := c.Get("site_name")
	siteNameStr, _ := siteName.(string)
	name := c.Param("name")

	if h.SiteScriptStores == nil {
		c.JSON(http.StatusOK, Response{Data: []any{}})
		return
	}
	store, exists := h.SiteScriptStores[siteNameStr]
	if !exists || store == nil {
		c.JSON(http.StatusOK, Response{Data: []any{}})
		return
	}

	execs, err := store.LoadExecutions(siteNameStr, name, 50)
	if err != nil {
		slog.Error("loading executions", "script", name, "error", err)
		c.JSON(http.StatusInternalServerError, ErrorResponse{Error: map[string]string{"message": "Failed to load executions"}})
		return
	}

	c.JSON(http.StatusOK, Response{Data: execs})
}

// detectHardcodedSecrets scans script text for patterns that suggest hardcoded credentials.
func detectHardcodedSecrets(script string) []string {
	var issues []string
	patterns := []string{
		"sk_live_", "sk_test_",           // Stripe
		"api_key", "api_secret",           // Generic
		"Bearer ", "Authorization:",       // Auth headers
		"password", "passwd", "pwd",       // Passwords
		"PRIVATE KEY",                     // PEM keys
		"-----BEGIN",                      // PEM blocks
	}
	for _, p := range patterns {
		if containsIgnoreCase(script, p) {
			issues = append(issues, fmt.Sprintf("Script may contain a hardcoded secret (pattern: %q). Use kora.secrets.get() instead.", p))
		}
	}
	return issues
}

func containsIgnoreCase(s, substr string) bool {
	sl := len(s)
	tl := len(substr)
	if tl > sl {
		return false
	}
	for i := 0; i <= sl-tl; i++ {
		match := true
		for j := 0; j < tl; j++ {
			sc := s[i+j]
			tc := substr[j]
			if sc >= 'A' && sc <= 'Z' {
				sc += 32
			}
			if tc >= 'A' && tc <= 'Z' {
				tc += 32
			}
			if sc != tc {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
