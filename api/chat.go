package api

import (
	"github.com/gin-gonic/gin"

	"github.com/asenawritescode/kora/api/ai"
)

// HandleChat processes a chat message, calls the AI provider with function definitions,
// executes any tool calls via the ORM, and returns the AI's response.
// POST /api/chat
func (h *Handler) HandleChat(c *gin.Context) {
	reg := h.siteRegistry(c)
	siteNameRaw, _ := c.Get("site_name")
	siteName, _ := siteNameRaw.(string)
	tx := h.siteTx(c)

	currentUser := "mcp-agent"
	if u, ok := c.Get("user"); ok {
		if s, ok := u.(string); ok && s != "" {
			currentUser = s
		}
	}

	ai.HandleChat(c, tx, reg, siteName, currentUser)
}
