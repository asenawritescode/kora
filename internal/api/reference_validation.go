package api

import (
	"net/http"

	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/gin-gonic/gin"
)

func rejectInvalidSnapshotReferences(c *gin.Context, snapshot *doctype.ConfigSnapshot, registry *doctype.Registry) bool {
	findings := doctype.ValidateSnapshotReferences(snapshot, registry)
	if len(findings) == 0 {
		return false
	}
	writeError(c, http.StatusBadRequest, "configuration.references_invalid", "Configuration contains unresolved references", map[string]any{"findings": findings})
	return true
}

func rejectInvalidPageManifestReferences(c *gin.Context, manifest *doctype.PageManifest, registry *doctype.Registry) bool {
	findings := doctype.ValidatePageManifestReferences(manifest, registry)
	if len(findings) == 0 {
		return false
	}
	writeError(c, http.StatusBadRequest, "configuration.references_invalid", "Page manifest contains unresolved references", map[string]any{"findings": findings})
	return true
}
