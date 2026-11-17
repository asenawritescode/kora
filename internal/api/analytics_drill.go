package api

import (
	"database/sql"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/asenawritescode/kora/internal/analytics"
	db "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
)

// registerAnalyticsDrill mounts POST /analytics/drill (no records are read).
func registerAnalyticsDrill(ag *gin.RouterGroup, registry *doctype.Registry, siteDB *sql.DB, dialect db.Dialect) {
	ag.POST("/drill", func(c *gin.Context) {
		var request analytics.DrillRequest
		if err := c.ShouldBindJSON(&request); err != nil {
			writeError(c, http.StatusBadRequest, "validation.invalid_json", "Invalid analytics drill request", nil)
			return
		}
		siteRegistry := analyticsRegistry(c, registry)
		catalog, err := analyticsCatalogWithReports(c, siteRegistry, getSiteDB(c, siteDB), dialect)
		if err != nil {
			internalError(c, "building analytics catalog", err)
			return
		}
		if !checkRelatedModelPermissions(c, siteRegistry, catalog.Models, analytics.SemanticRequest{Queries: []analytics.ModelQuery{{Model: request.Model}}}) {
			return
		}
		drill, err := analytics.BuildDrill(catalog, siteRegistry, request, analyticsQueryScope(c))
		if err != nil {
			writeError(c, http.StatusUnprocessableEntity, "analytics.drill_invalid", err.Error(), nil)
			return
		}
		c.JSON(http.StatusOK, Response{Data: drill})
	})
}
