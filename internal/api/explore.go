package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/metrics"
)

// registerExploreRoutes mounts GET /explore/catalog, the record types a caller may ask questions of.
func registerExploreRoutes(apiGroup *gin.RouterGroup, registry *doctype.Registry) {
	apiGroup.Group("/explore").GET("/catalog", func(c *gin.Context) {
		reg := analyticsRegistry(c, registry)
		var docs []*doctype.DocType
		read, join := map[string]bool{}, map[string]bool{}
		if reg != nil {
			docs = reg.All()
			read, join = exploreAccess(c, reg, docs)
		}
		c.JSON(http.StatusOK, Response{Data: metrics.ExploreCatalog(docs, read, join)})
	})
}

// exploreAccess returns the record types the caller may question (read) and the record types a dimension
// path or label may join to (join).
func exploreAccess(c *gin.Context, reg *doctype.Registry, docs []*doctype.DocType) (read, join map[string]bool) {
	read, join = map[string]bool{}, map[string]bool{}
	for _, dt := range docs {
		if dt.Facts != nil && !dt.IsChildTable && readableRecordType(c, reg, dt.Name) {
			read[dt.Name] = true
		}
		if joinableRecordType(c, reg, dt.Name) {
			join[dt.Name] = true
		}
	}
	return read, join
}

// joinableRecordType reports whether a dimension path or a label may read a record type: the caller's grant
// on it is unrestricted. An owner-only grant, a field-assigned grant and a legal-entity scope each narrow
// the rows, so a linked record type with one of them is not joined to.
func joinableRecordType(c *gin.Context, reg *doctype.Registry, name string) bool {
	row, denial := permScope(c, reg, name, "read")
	return denial == nil && !row.Owner && len(row.AssignedFields) == 0 && row.Business.Kind != contract.ScopeKindLegalEntity
}

// readableRecordType reports whether the caller may read every row of a record type through the metric
// read path: the same grant check a metric query applies.
func readableRecordType(c *gin.Context, reg *doctype.Registry, name string) bool {
	_, denial := metricScope(c, reg, []string{name})
	return denial == nil
}
