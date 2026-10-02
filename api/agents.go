package api

import (
	"fmt"
	"github.com/asenawritescode/kora/org"
	"github.com/gin-gonic/gin"
	"net/http"
	"strings"
)

var agentPermissionOperations = map[string]bool{
	"read": true, "write": true, "create": true, "delete": true,
	"submit": true, "cancel": true, "amend": true, "export": true,
	"import": true, "report": true,
}

func (h *Handler) agentStore(c *gin.Context) *org.AgentStore {
	site := c.GetString("site_name")
	if site == "" {
		site = "default"
	}
	if h.AgentStores == nil {
		h.AgentStores = map[string]*org.AgentStore{}
	}
	if h.AgentStores[site] == nil {
		if h.TxManager != nil && h.TxManager.DB != nil {
			if store, err := org.NewSQLAgentStore(h.TxManager.DB); err == nil {
				h.AgentStores[site] = store
			}
		}
		if h.AgentStores[site] == nil {
			h.AgentStores[site] = org.NewAgentStore()
		}
	}
	return h.AgentStores[site]
}
func (h *Handler) HandleAgentManifest(c *gin.Context) {
	store := h.agentStore(c)
	id := c.Param("id")
	if c.Request.Method == http.MethodGet {
		m, ok := store.GetManifest(id)
		if !ok {
			c.JSON(http.StatusNotFound, gin.H{"error": "agent manifest not found"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"data": m})
		return
	}
	var m org.AgentManifest
	if err := c.ShouldBindJSON(&m); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	m.ID = id
	if err := h.validateAgentPermissions(c, m); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	saved, err := store.SaveManifest(m)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": saved})
}

func (h *Handler) validateAgentPermissions(c *gin.Context, m org.AgentManifest) error {
	reg := h.siteRegistry(c)
	if reg == nil {
		if len(m.Roles) == 0 && len(m.DirectPermissions) == 0 && len(m.DeniedPermissions) == 0 && len(m.ApprovalRules) == 0 {
			return nil
		}
		return fmt.Errorf("permission policy unavailable")
	}
	for _, role := range m.Roles {
		if strings.TrimSpace(role) == "" || reg.Permissions.GetRole(role) == nil {
			return fmt.Errorf("unknown agent role %q", role)
		}
	}
	checks := [][]org.AgentPermission{m.DirectPermissions, m.DeniedPermissions, m.ApprovalRules}
	for _, permissions := range checks {
		for _, p := range permissions {
			if p.Doctype == "" || !reg.Has(p.Doctype) {
				return fmt.Errorf("unknown agent permission doctype %q", p.Doctype)
			}
			if !agentPermissionOperations[p.Operation] {
				return fmt.Errorf("unsupported agent permission operation %q", p.Operation)
			}
		}
	}
	return nil
}

// HandleAgentPermissionCatalog exposes DocType-derived operations to Studio.
func (h *Handler) HandleAgentPermissionCatalog(c *gin.Context) {
	reg := h.siteRegistry(c)
	items := make([]gin.H, 0, len(reg.All())*len(agentPermissionOperations))
	operations := []string{"read", "create", "write", "delete", "submit", "cancel", "amend", "export", "import", "report"}
	for _, dt := range reg.All() {
		for _, operation := range operations {
			items = append(items, gin.H{"doctype": dt.Name, "operation": operation, "label": dt.Name + " · " + operation})
		}
	}
	c.JSON(http.StatusOK, gin.H{"data": items})
}

// HandleAgentEffectivePermissions returns the permission matrix result for an agent.
func (h *Handler) HandleAgentEffectivePermissions(c *gin.Context) {
	manifest, ok := h.agentStore(c).GetManifest(c.Param("id"))
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "agent manifest not found"})
		return
	}
	reg := h.siteRegistry(c)
	type effectivePermission struct {
		Doctype          string `json:"doctype"`
		Operation        string `json:"operation"`
		Allowed          bool   `json:"allowed"`
		Source           string `json:"source"`
		ApprovalRequired bool   `json:"approval_required"`
	}
	denied := map[string]bool{}
	for _, p := range manifest.DeniedPermissions {
		denied[p.Doctype+":"+p.Operation] = true
	}
	approval := map[string]bool{}
	for _, p := range manifest.ApprovalRules {
		approval[p.Doctype+":"+p.Operation] = true
	}
	result := []effectivePermission{}
	for _, dt := range reg.All() {
		for _, operation := range []string{"read", "create", "write", "delete", "submit", "cancel", "amend", "export", "import", "report"} {
			key := dt.Name + ":" + operation
			allowed, _ := reg.Permissions.UserCan(manifest.Roles, dt.Name, operation)
			source := "none"
			for _, p := range manifest.DirectPermissions {
				if p.Doctype == dt.Name && p.Operation == operation {
					allowed = true
					source = "direct"
				}
			}
			if source == "none" {
				for _, role := range manifest.Roles {
					if reg.Permissions.Can(role, dt.Name, operation) {
						source = "role:" + role
						break
					}
				}
			}
			if denied[key] {
				allowed = false
				source = "denied"
			}
			result = append(result, effectivePermission{dt.Name, operation, allowed, source, approval[key]})
		}
	}
	c.JSON(http.StatusOK, gin.H{"data": result})
}
func (h *Handler) HandleAgentRuns(c *gin.Context) {
	store := h.agentStore(c)
	id := c.Param("id")
	if c.Request.Method == http.MethodGet {
		c.JSON(http.StatusOK, gin.H{"data": store.Runs(id)})
		return
	}
	var run org.AgentRun
	if err := c.ShouldBindJSON(&run); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	run.AgentID = id
	if run.Doctype != "" {
		reg := h.siteRegistry(c)
		if reg == nil || !reg.Has(run.Doctype) {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("unknown agent run doctype %q", run.Doctype)})
			return
		}
		if !agentPermissionOperations[run.Operation] {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("unsupported agent run operation %q", run.Operation)})
			return
		}
	}
	saved, err := store.RecordRun(run)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"data": saved})
}
