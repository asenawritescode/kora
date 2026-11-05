package api

import "github.com/gin-gonic/gin"

// adminOnly rejects requests whose verified execution lacks the administrator role.
func adminOnly() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !requireAdmin(c) {
			c.Abort()
			return
		}
		c.Next()
	}
}
