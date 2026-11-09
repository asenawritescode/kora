package api

import (
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/gin-gonic/gin"
)

// declaresModuleOwnership reports whether a DocType body claims module ownership,
// which only package activation may assign.
func declaresModuleOwnership(dt *doctype.DocType) bool {
	return dt.ModuleOwnerID != "" || dt.ModuleOwnershipMode != "" || dt.ModuleAccess != ""
}

// preserveModuleOwnership keeps the installed ownership on an edited DocType so
// the generic editor cannot strip or reassign it.
func preserveModuleOwnership(dst, current *doctype.DocType) {
	dst.ModuleOwnerID = current.ModuleOwnerID
	dst.ModuleOwnershipMode = current.ModuleOwnershipMode
	dst.ModuleAccess = current.ModuleAccess
}

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
