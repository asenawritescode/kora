package api

import (
	"database/sql"

	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/gin-gonic/gin"
)

// analyticsRegistry is named for its first caller; it returns the registry
// pinned to the request's site, or fallback when the request carries none.
func analyticsRegistry(c *gin.Context, fallback *doctype.Registry) *doctype.Registry {
	if value, ok := c.Get("site_registry"); ok {
		if registry, ok := value.(*doctype.Registry); ok && registry != nil {
			return registry
		}
	}
	return fallback
}

// getSiteDB returns the database pinned to the request's site, or fallback.
func getSiteDB(c *gin.Context, fallback *sql.DB) *sql.DB {
	if db, ok := c.Get("site_db"); ok {
		if sqlDB, ok := db.(*sql.DB); ok {
			return sqlDB
		}
	}
	return fallback
}
