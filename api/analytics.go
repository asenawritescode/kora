package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/asenawritescode/kora/analytics"
	db "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
)

// RegisterAnalyticsRoutes registers analytics API endpoints.
// siteDB is the fallback DB; per-request DB is resolved from gin context.
// registry is used to auto-generate metrics from DocType metadata.
// siteBuses maps site name → EventBus.
func RegisterAnalyticsRoutes(apiGroup *gin.RouterGroup, registry *doctype.Registry, siteDB *sql.DB, siteBuses map[string]analytics.EventBus, dialect db.Dialect) {
	ag := apiGroup.Group("/analytics")
	queryCache := newAnalyticsQueryCache(30*time.Second, 256)

	ag.GET("/catalog", func(c *gin.Context) {
		docTypes := make([]*doctype.DocType, 0, len(registry.Names()))
		for _, name := range registry.Names() {
			docTypes = append(docTypes, registry.Get(name))
		}
		catalog := analytics.BuildSemanticCatalog(docTypes)
		reports, err := loadSemanticReports(c, getSiteDB(c, siteDB))
		if err != nil {
			internalError(c, "loading analytics reports", err)
			return
		}
		catalog.Reports = reports
		if err := catalog.Validate(); err != nil {
			internalError(c, "building analytics catalog", err)
			return
		}
		c.JSON(http.StatusOK, Response{Data: catalog})
	})

	ag.GET("/reports", func(c *gin.Context) {
		reports, err := loadSemanticReports(c, getSiteDB(c, siteDB))
		if err != nil {
			internalError(c, "loading analytics reports", err)
			return
		}
		c.JSON(http.StatusOK, Response{Data: reports})
	})

	ag.POST("/reports/:name/query", func(c *gin.Context) {
		var request analytics.AnalyticsQueryRequest
		if err := c.ShouldBindJSON(&request); err != nil {
			writeError(c, http.StatusBadRequest, "validation.invalid_json", "Invalid analytics query", nil)
			return
		}
		docTypes := make([]*doctype.DocType, 0, len(registry.Names()))
		for _, name := range registry.Names() {
			docTypes = append(docTypes, registry.Get(name))
		}
		catalog := analytics.BuildSemanticCatalog(docTypes)
		reports, err := loadSemanticReports(c, getSiteDB(c, siteDB))
		if err != nil {
			internalError(c, "loading analytics reports", err)
			return
		}
		catalog.Reports = reports
		var report *analytics.ReportDefinition
		for index := range reports {
			if reports[index].Name == c.Param("name") {
				report = &reports[index]
				break
			}
		}
		if report == nil {
			writeError(c, http.StatusNotFound, "analytics.report_not_found", "Report not found", map[string]any{"name": c.Param("name")})
			return
		}
		if err := report.Validate(catalog); err != nil {
			writeError(c, http.StatusUnprocessableEntity, "analytics.report_invalid", err.Error(), nil)
			return
		}
		request.Queries = make([]analytics.ModelQuery, 0, len(report.Queries))
		for _, query := range report.Queries {
			request.Queries = append(request.Queries, analytics.ModelQuery{Model: query.Model, Measures: query.Measures, Dimensions: query.Dimensions, Filters: query.Filters})
		}
		qe := getQueryEngine(c, siteDB)
		if qe == nil {
			writeError(c, http.StatusServiceUnavailable, "server.store_unavailable", "Analytics not available for this site", nil)
			return
		}
		result, err := qe.ResolveSemanticQuery(catalog, request)
		if err != nil {
			writeError(c, http.StatusBadRequest, "analytics.invalid_query", err.Error(), nil)
			return
		}
		c.JSON(http.StatusOK, Response{Data: gin.H{"report": report, "result": result}})
	})

	ag.POST("/query", func(c *gin.Context) {
		var request analytics.AnalyticsQueryRequest
		if err := c.ShouldBindJSON(&request); err != nil {
			writeError(c, http.StatusBadRequest, "validation.invalid_json", "Invalid analytics query", nil)
			return
		}
		docTypes := make([]*doctype.DocType, 0, len(registry.Names()))
		for _, name := range registry.Names() {
			docTypes = append(docTypes, registry.Get(name))
		}
		catalog := analytics.BuildSemanticCatalog(docTypes)
		if err := catalog.Validate(); err != nil {
			internalError(c, "building analytics catalog", err)
			return
		}
		cacheKey := queryCache.key(c.GetString("site_name"), request)
		if cached, ok := queryCache.get(cacheKey); ok {
			c.JSON(http.StatusOK, Response{Data: cached})
			return
		}
		qe := getQueryEngine(c, siteDB)
		if qe == nil {
			writeError(c, http.StatusServiceUnavailable, "server.store_unavailable", "Analytics not available for this site", nil)
			return
		}
		result, err := qe.ResolveSemanticQuery(catalog, request)
		if err != nil {
			writeError(c, http.StatusBadRequest, "analytics.invalid_query", err.Error(), nil)
			return
		}
		queryCache.put(cacheKey, result)
		c.JSON(http.StatusOK, Response{Data: result})
	})

	// Status endpoint always available — reports whether analytics is running.
	ag.GET("/status", func(c *gin.Context) {
		siteName := c.GetString("site_name")
		bus := siteBuses[siteName]
		c.JSON(http.StatusOK, Response{
			Data: analytics.GetStatus(bus),
		})
	})

	// POST /metrics — create a custom metric.
	ag.POST("/metrics", func(c *gin.Context) {
		var input analytics.Metric
		if err := c.ShouldBindJSON(&input); err != nil {
			writeError(c, http.StatusBadRequest, "validation.invalid_json", "Invalid JSON", nil)
			return
		}
		if input.Name == "" || input.DocType == "" {
			writeError(c, http.StatusBadRequest, "validation.required_field", "name and doctype are required", nil)
			return
		}
		input.AutoGenerated = false
		db := getSiteDB(c, siteDB)
		if db == nil {
			writeError(c, http.StatusServiceUnavailable, "server.database_unavailable", "No database connection", nil)
			return
		}
		siteName := c.GetString("site_name")
		updateCols := []string{"label", "type", "doctype", "field_name", "link_field", "group_by_field"}
		_, err := db.Exec(
			`INSERT INTO _kora_analytics_metric (site, name, label, type, doctype, field_name, link_field, group_by_field)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?) `+
				dialect.UpsertClause([]string{"site", "name"}, updateCols),
			siteName, input.Name, input.Label, string(input.Type), input.DocType,
			input.Field, input.LinkField, input.GroupByField,
		)
		if err != nil {
			internalError(c, "saving custom metric", err)
			return
		}
		c.JSON(http.StatusCreated, Response{Data: &input})
	})

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
		writeError(c, http.StatusNotFound, "analytics.metric_not_found", "Metric not found", nil)
	})

	ag.POST("/metrics/:name/query", func(c *gin.Context) {
		qe := getQueryEngine(c, siteDB)
		if qe == nil {
			writeError(c, http.StatusServiceUnavailable, "server.store_unavailable", "Analytics not available for this site", nil)
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
			writeError(c, http.StatusNotFound, "analytics.metric_not_found", "Metric not found", nil)
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
			writeError(c, http.StatusServiceUnavailable, "server.store_unavailable", "Analytics not available for this site", nil)
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

func loadSemanticReports(c *gin.Context, db *sql.DB) ([]analytics.ReportDefinition, error) {
	if db == nil {
		return []analytics.ReportDefinition{}, nil
	}
	siteName := c.GetString("site_name")
	var configJSON string
	err := db.QueryRow(`SELECT config FROM _kora_config_version WHERE site = ? AND status = 'Active' ORDER BY version DESC LIMIT 1`, siteName).Scan(&configJSON)
	if err == sql.ErrNoRows {
		return []analytics.ReportDefinition{}, nil
	}
	if err != nil {
		return nil, err
	}
	snapshot, err := doctype.ParseConfig(configJSON)
	if err != nil {
		return nil, err
	}
	reports := make([]analytics.ReportDefinition, 0, len(snapshot.Reports))
	for _, raw := range snapshot.Reports {
		var report analytics.ReportDefinition
		if err := json.Unmarshal(raw, &report); err != nil {
			return nil, err
		}
		reports = append(reports, report)
	}
	return reports, nil
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
	// Load custom metrics from DB.
	db := getSiteDB(c, nil)
	if db != nil {
		siteName := c.GetString("site_name")
		rows, err := db.Query(
			"SELECT name, label, type, doctype, field_name, link_field, group_by_field FROM _kora_analytics_metric WHERE site = ?",
			siteName,
		)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var m analytics.Metric
				rows.Scan(&m.Name, &m.Label, &m.Type, &m.DocType, &m.Field, &m.LinkField, &m.GroupByField)
				m.AutoGenerated = false
				all = append(all, &m)
			}
		}
	}
	return all
}

// getSiteDB returns the site database connection from gin context, or fallback.
func getSiteDB(c *gin.Context, fallback *sql.DB) *sql.DB {
	if db, ok := c.Get("site_db"); ok {
		if sqlDB, ok := db.(*sql.DB); ok {
			return sqlDB
		}
	}
	return fallback
}

// getQueryEngine returns a QueryEngine for the current request's site.
func getQueryEngine(c *gin.Context, fallbackDB *sql.DB) *analytics.QueryEngine {
	siteName := c.GetString("site_name")
	if siteName == "" {
		return nil
	}
	db := getSiteDB(c, fallbackDB)
	if db == nil {
		return nil
	}
	return &analytics.QueryEngine{DB: db, SiteName: siteName}
}
