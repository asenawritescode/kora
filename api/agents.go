package api

import (
	"github.com/asenawritescode/kora/org"
	"github.com/gin-gonic/gin"
	"net/http"
)

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
	saved, err := store.SaveManifest(m)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": saved})
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
	saved, err := store.RecordRun(run)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"data": saved})
}
