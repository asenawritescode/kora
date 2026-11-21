package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/metrics"
)

// registerExploreRoutes mounts GET /explore/catalog, the record types a caller may ask questions of.
func registerExploreRoutes(apiGroup *gin.RouterGroup, registry *doctype.Registry) {
	apiGroup.Group("/explore").GET("/catalog", func(c *gin.Context) {
		reg := analyticsRegistry(c, registry)
		var docs []*doctype.DocType
		if reg != nil {
			for _, dt := range reg.All() {
				if dt.Facts != nil && !dt.IsChildTable && readableRecordType(c, reg, dt.Name) {
					docs = append(docs, dt)
				}
			}
		}
		c.JSON(http.StatusOK, Response{Data: metrics.ExploreCatalog(docs)})
	})
}

// readableRecordType reports whether the caller may read every row of a record type through the metric
// read path: the same grant check a metric query applies.
func readableRecordType(c *gin.Context, reg *doctype.Registry, name string) bool {
	_, denial := metricScope(c, reg, []string{name})
	return denial == nil
}
