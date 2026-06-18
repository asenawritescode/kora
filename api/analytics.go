package api

import (
	"database/sql"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/asenawritescode/kora/analytics"
	"github.com/asenawritescode/kora/doctype"
)

// RegisterAnalyticsRoutes registers analytics API endpoints.
// siteDB is the fallback DB; per-request DB is resolved from gin context.
// registry is used to auto-generate metrics from DocType metadata.
// siteBuses maps site name → EventBus; empty map = analytics disabled for all sites.
func RegisterAnalyticsRoutes(apiGroup *gin.RouterGroup, registry *doctype.Registry, siteDB *sql.DB, siteBuses map[string]analytics.EventBus) {
	ag := apiGroup.Group("/analytics")

	// Status endpoint always available — reports whether analytics is running.
	ag.GET("/status", func(c *gin.Context) {
		siteName := c.GetString("site_name")
		bus := siteBuses[siteName]
		c.JSON(http.StatusOK, Response{
			Data: analytics.GetStatus(bus),
		})
	})

	// If analytics is disabled entirely, data endpoints return 503.
	if len(siteBuses) == 0 {
		ag.GET("/metrics", notAvailable)
		ag.GET("/metrics/:name", notAvailable)
		ag.POST("/metrics/:name/query", notAvailable)
		ag.GET("/insights/:doctype", notAvailable)
		return
	}

	ag.GET("/metrics", func(c *gin.Context) {
		metrics := resolveMetrics(c, registry)
		c.JSON(http.StatusOK, Response{Data: metrics})
	})

	ag.GET("/metrics/:name", func(c *gin.Context) {
		metrics := resolveMetrics(c, registry)
		for _, m := range metrics {
			if m.Name == c.Param("name") {
				c.JSON(http.StatusOK, Response{Data: m})
				return
			}
		}
		c.JSON(http.StatusNotFound, ErrorResponse{
			Error: map[string]string{"message": "Metric not found"},
		})
	})

	ag.POST("/metrics/:name/query", func(c *gin.Context) {
		qe := getQueryEngine(c, siteDB)
		if qe == nil {
			c.JSON(http.StatusServiceUnavailable, ErrorResponse{
				Error: map[string]string{"message": "Analytics not available for this site"},
			})
			return
		}

		var req analytics.QueryRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			req = analytics.QueryRequest{}
		}
		req.Metric = c.Param("name")

		metrics := resolveMetrics(c, registry)
		var metric *analytics.Metric
		for _, m := range metrics {
			if m.Name == req.Metric {
				metric = m
				break
			}
		}
		if metric == nil {
			c.JSON(http.StatusNotFound, ErrorResponse{
				Error: map[string]string{"message": "Metric not found"},
			})
			return
		}

		result, err := qe.Resolve(metric, req)
		if err != nil {
			internalError(c, "analytics query failed", err)
			return
		}

		c.JSON(http.StatusOK, Response{Data: result})
	})

	ag.GET("/insights/:doctype", func(c *gin.Context) {
		qe := getQueryEngine(c, siteDB)
		if qe == nil {
			c.JSON(http.StatusServiceUnavailable, ErrorResponse{
				Error: map[string]string{"message": "Analytics not available for this site"},
			})
			return
		}

		doctypeName := c.Param("doctype")
		metrics := resolveMetrics(c, registry)
		insights, err := qe.ResolveInsights(doctypeName, metrics)
		if err != nil {
			internalError(c, "insights query failed", err)
			return
		}

		c.JSON(http.StatusOK, Response{Data: insights})
	})
}

// resolveMetrics returns all metrics for the current site: auto-generated from
// DocType metadata plus any user-defined custom metrics.
func resolveMetrics(c *gin.Context, registry *doctype.Registry) []*analytics.Metric {
	var all []*analytics.Metric
	for _, name := range registry.Names() {
		dt := registry.Get(name)
		if dt == nil {
			continue
		}
		all = append(all, analytics.GenerateMetrics(dt)...)
		if dt.IsSubmittable {
			if wf := registry.Workflows.Get(name); wf != nil {
				all = append(all, analytics.GenerateWorkflowMetrics(dt, wf)...)
			}
		}
	}
	return all
}

// getQueryEngine returns a QueryEngine for the current request's site.
func getQueryEngine(c *gin.Context, fallbackDB *sql.DB) *analytics.QueryEngine {
	siteName := c.GetString("site_name")
	if siteName == "" {
		return nil
	}
	db, _ := c.Get("site_db")
	if db == nil {
		if fallbackDB != nil {
			return &analytics.QueryEngine{DB: fallbackDB, SiteName: siteName}
		}
		return nil
	}
	return &analytics.QueryEngine{DB: db.(*sql.DB), SiteName: siteName}
}

func notAvailable(c *gin.Context) {
	c.JSON(http.StatusServiceUnavailable, ErrorResponse{
		Error: map[string]string{"message": "Analytics is not enabled. Set KORA_ANALYTICS=true"},
	})
}
