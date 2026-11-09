package api

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/analytics"
	db "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/natsprovider"
	"github.com/asenawritescode/kora/internal/policy"
)

// RegisterAnalyticsRoutes registers analytics API endpoints.
// siteDB is the fallback DB; per-request DB is resolved from gin context.
// registry is used to auto-generate metrics from DocType metadata.
// siteBuses maps site name → EventBus.
func RegisterAnalyticsRoutes(apiGroup *gin.RouterGroup, registry *doctype.Registry, siteDB *sql.DB, siteBuses map[string]analytics.EventBus, realtimeProviders map[string]*natsprovider.Provider, dialect db.Dialect) {
	RegisterAnalyticsRoutesWithRuntimeServices(apiGroup, registry, siteDB, siteBuses, realtimeProviders, dialect, nil)
}

func RegisterAnalyticsRoutesWithRuntimeServices(apiGroup *gin.RouterGroup, registry *doctype.Registry, siteDB *sql.DB, siteBuses map[string]analytics.EventBus, realtimeProviders map[string]*natsprovider.Provider, dialect db.Dialect, runtimeServices *SiteRuntimeServices) {
	cache := DefaultAnalyticsQueryCache()
	if runtimeServices != nil {
		cache = runtimeServices.analyticsCacheOwner()
	}
	RegisterAnalyticsRoutesWithQueryCache(apiGroup, registry, siteDB, siteBuses, realtimeProviders, dialect, runtimeServices, cache)
}

// RegisterAnalyticsRoutesWithQueryCache installs analytics routes using the
// process-owned cache supplied by Engine composition.
func RegisterAnalyticsRoutesWithQueryCache(apiGroup *gin.RouterGroup, registry *doctype.Registry, siteDB *sql.DB, siteBuses map[string]analytics.EventBus, realtimeProviders map[string]*natsprovider.Provider, dialect db.Dialect, runtimeServices *SiteRuntimeServices, processCache *AnalyticsQueryCache) {
	ag := apiGroup.Group("/analytics")
	if processCache == nil || processCache.cache == nil {
		processCache = DefaultAnalyticsQueryCache()
	}
	queryCache := processCache.cache
	ag.GET("/catalog", func(c *gin.Context) {
		siteRegistry := analyticsRegistry(c, registry)
		docTypes := make([]*doctype.DocType, 0, len(siteRegistry.Names()))
		for _, name := range siteRegistry.Names() {
			docTypes = append(docTypes, siteRegistry.Get(name))
		}
		catalog := analytics.BuildSemanticCatalog(docTypes)
		reports, err := loadAvailableReports(c, siteRegistry, getSiteDB(c, siteDB), dialect)
		if err != nil {
			internalError(c, "loading analytics reports", err)
			return
		}
		if err := analytics.AddReportModels(catalog, reports); err != nil {
			internalError(c, "adding report models to analytics catalog", err)
			return
		}
		if err := analytics.ValidateReportModels(siteRegistry, reports); err != nil {
			internalError(c, "validating report model sources", err)
			return
		}
		catalog.Reports = reports
		if err := catalog.Validate(); err != nil {
			internalError(c, "building analytics catalog", err)
			return
		}
		catalog = filterReadableAnalyticsCatalog(c, siteRegistry, catalog)
		c.JSON(http.StatusOK, Response{Data: catalog})
	})

	ag.GET("/reports", func(c *gin.Context) {
		siteRegistry := analyticsRegistry(c, registry)
		reports, err := loadAvailableReports(c, siteRegistry, getSiteDB(c, siteDB), dialect)
		if err != nil {
			internalError(c, "loading analytics reports", err)
			return
		}
		if err := analytics.ValidateReportModels(siteRegistry, reports); err != nil {
			internalError(c, "validating report model sources", err)
			return
		}
		c.JSON(http.StatusOK, Response{Data: filterReadableAnalyticsReports(c, siteRegistry, reports)})
	})

	ag.POST("/reports/:name/query", func(c *gin.Context) {
		var request analytics.AnalyticsQueryRequest
		if err := c.ShouldBindJSON(&request); err != nil {
			writeError(c, http.StatusBadRequest, "validation.invalid_json", "Invalid analytics query", nil)
			return
		}
		siteRegistry := analyticsRegistry(c, registry)
		docTypes := make([]*doctype.DocType, 0, len(siteRegistry.Names()))
		for _, name := range siteRegistry.Names() {
			docTypes = append(docTypes, siteRegistry.Get(name))
		}
		catalog := analytics.BuildSemanticCatalog(docTypes)
		reports, err := loadAvailableReports(c, siteRegistry, getSiteDB(c, siteDB), dialect)
		if err != nil {
			internalError(c, "loading analytics reports", err)
			return
		}
		if err := analytics.AddReportModels(catalog, reports); err != nil {
			internalError(c, "adding report models to analytics catalog", err)
			return
		}
		if err := analytics.ValidateReportModels(siteRegistry, reports); err != nil {
			internalError(c, "validating report model sources", err)
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
		if request.Time.From == "" {
			request.Time.From = time.Now().AddDate(0, 0, -30).Format("2006-01-02")
		}
		if request.Time.To == "" {
			request.Time.To = time.Now().Format("2006-01-02")
		}
		if request.Time.Granularity == "" {
			request.Time.Granularity = "day"
		}
		if err := report.Validate(catalog); err != nil {
			writeError(c, http.StatusUnprocessableEntity, "analytics.report_invalid", err.Error(), nil)
			return
		}
		request.Queries = make([]analytics.ModelQuery, 0, len(report.Queries))
		for _, query := range report.Queries {
			request.Queries = append(request.Queries, analytics.ModelQuery{Model: query.Model, Measures: query.Measures, Dimensions: query.Dimensions, Filters: query.Filters})
		}
		if !checkRelatedReportPermissions(c, siteRegistry, report, catalog) {
			return
		}
		qe := getQueryEngine(c, siteDB, dialect)
		if qe == nil {
			writeError(c, http.StatusServiceUnavailable, "server.store_unavailable", "Analytics not available for this site", nil)
			return
		}
		if execution, valid := requestExecution(c); valid {
			qe.BusinessScope = businessScopeFromRequest(c, execution)
		}
		applyProjectionActorScopes(c, qe)
		result, err := qe.ResolveSemanticQuery(catalog, request, siteRegistry)
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
		siteRegistry := analyticsRegistry(c, registry)
		docTypes := make([]*doctype.DocType, 0, len(siteRegistry.Names()))
		for _, name := range siteRegistry.Names() {
			docTypes = append(docTypes, siteRegistry.Get(name))
		}
		catalog := analytics.BuildSemanticCatalog(docTypes)
		reports, err := loadAvailableReports(c, siteRegistry, getSiteDB(c, siteDB), dialect)
		if err != nil {
			internalError(c, "loading analytics reports", err)
			return
		}
		if err := analytics.AddReportModels(catalog, reports); err != nil {
			internalError(c, "adding report models to analytics catalog", err)
			return
		}
		if err := analytics.ValidateReportModels(siteRegistry, reports); err != nil {
			internalError(c, "validating report model sources", err)
			return
		}
		catalog.Reports = reports
		if err := catalog.Validate(); err != nil {
			internalError(c, "building analytics catalog", err)
			return
		}
		if !checkRelatedModelPermissions(c, siteRegistry, catalog.Models, request) {
			return
		}
		tenantID := c.GetString("site_id")
		if tenantID == "" {
			tenantID = c.GetString("site_name")
		}
		queryScope := contract.BusinessScope{TenantID: contract.TenantID(tenantID), Kind: contract.ScopeKindTenant}
		if execution, valid := requestExecution(c); valid {
			queryScope = businessScopeFromRequest(c, execution)
		}
		cacheKey := queryCache.key(tenantID, c.GetUint64("site_config_revision"), analyticsPermissionScope(c, siteRegistry)+"\x00"+string(queryScope.Kind)+"\x00"+queryScope.LegalEntityID, request)
		cacheable := !analyticsRequestUsesPublishedProjection(catalog, request)
		if cacheable {
			if cached, ok := queryCache.get(cacheKey); ok {
				c.JSON(http.StatusOK, Response{Data: cached})
				return
			}
		}
		qe := getQueryEngine(c, siteDB, dialect)
		if qe == nil {
			writeError(c, http.StatusServiceUnavailable, "server.store_unavailable", "Analytics not available for this site", nil)
			return
		}
		if execution, valid := requestExecution(c); valid {
			qe.BusinessScope = businessScopeFromRequest(c, execution)
		}
		applyProjectionActorScopes(c, qe)
		result, err := qe.ResolveSemanticQuery(catalog, request, siteRegistry)
		if err != nil {
			writeError(c, http.StatusBadRequest, "analytics.invalid_query", err.Error(), nil)
			return
		}
		if cacheable {
			queryCache.put(cacheKey, result)
		}
		c.JSON(http.StatusOK, Response{Data: result})
	})

	// Status endpoint always available — reports whether analytics is running.
	ag.GET("/status", func(c *gin.Context) {
		siteName := c.GetString("site_name")
		bus := RuntimeServiceForContext(c, runtimeServices, siteName).EventBus
		if bus == nil {
			bus = siteBuses[siteName]
		}
		c.JSON(http.StatusOK, Response{
			Data: analytics.GetStatus(bus),
		})
	})

	// Rebuild is an administrator-triggered recovery operation. It runs in the
	// server so customers never need database or terminal access.
	ag.POST("/rebuild", func(c *gin.Context) {
		execution, verified := requestExecution(c)
		decision, decisionErr := policy.Decide(execution, analyticsRegistry(c, registry), "Analytics", "write")
		if !verified || decisionErr != nil || decision.OwnerOnly || len(decision.AssignedFields) > 0 {
			c.JSON(http.StatusForbidden, ErrorResponse{Error: map[string]string{"code": "permission.admin_required", "message": "Administrator role required"}})
			return
		}
		var request struct {
			DocType string `json:"doctype"`
			From    string `json:"from"`
		}
		if err := c.ShouldBindJSON(&request); err != nil && err != io.EOF {
			badRequestError(c, "validation.invalid_json", "Invalid rebuild request", nil)
			return
		}
		from := time.Now().AddDate(-1, 0, 0)
		if request.From != "" {
			parsed, err := time.Parse("2006-01-02", request.From)
			if err != nil {
				badRequestError(c, "validation.invalid_date", "from must be YYYY-MM-DD", map[string]any{"field": "from"})
				return
			}
			from = parsed
		}
		now := time.Now().UTC()
		siteName := c.GetString("site_name")
		siteDialect := siteDialectFromContext(c, dialect)
		db := getSiteDB(c, siteDB)
		services := RuntimeServiceForContext(c, runtimeServices, siteName)
		if services.AnalyticsRebuild == nil {
			writeError(c, http.StatusServiceUnavailable, "server.store_unavailable", "Analytics rebuild worker is not available for this site", nil)
			return
		}
		job := &analytics.RebuildJob{ID: fmt.Sprintf("analytics-rebuild-%d", time.Now().UnixNano()), Site: siteName, DocType: request.DocType, From: from, Status: "queued", StartedAt: now}
		if err := analytics.CreateRebuildJob(db, siteDialect, job); err != nil {
			internalError(c, "creating analytics rebuild job", err)
			return
		}
		// The tenant runtime owns this worker; HTTP only persists and signals.
		// The durable tenant job table is authoritative, independent of NATS.
		services.AnalyticsRebuild.Notify()
		c.JSON(http.StatusAccepted, Response{Data: job})
	})

	ag.GET("/rebuild/:id", func(c *gin.Context) {
		job, err := analytics.GetRebuildJob(getSiteDB(c, siteDB), siteDialectFromContext(c, dialect), c.GetString("site_name"), c.Param("id"))
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, ErrorResponse{Error: map[string]string{"code": "analytics.rebuild_not_found", "message": "Rebuild job not found"}})
			return
		}
		if err != nil {
			internalError(c, "loading analytics rebuild job", err)
			return
		}
		c.JSON(http.StatusOK, Response{Data: job})
	})

	// POST /metrics — create a custom metric.
	ag.POST("/metrics", func(c *gin.Context) {
		execution, valid := requestExecution(c)
		decision, decideErr := policy.Decide(execution, analyticsRegistry(c, registry), "Analytics", "write")
		if !valid || decideErr != nil || decision.OwnerOnly || len(decision.AssignedFields) > 0 {
			c.JSON(http.StatusForbidden, ErrorResponse{Error: map[string]string{"code": "permission.denied", "message": "Analytics write permission is required"}})
			return
		}
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
		database := getSiteDB(c, siteDB)
		if database == nil {
			writeError(c, http.StatusServiceUnavailable, "server.database_unavailable", "No database connection", nil)
			return
		}
		siteName := c.GetString("site_name")
		siteDialect := siteDialectFromContext(c, dialect)
		updateCols := []string{"label", "type", "doctype", "field_name", "link_field", "group_by_field"}
		query := `INSERT INTO _kora_analytics_metric (site, name, label, type, doctype, field_name, link_field, group_by_field)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?) ` + siteDialect.UpsertClause([]string{"site", "name"}, updateCols)
		_, err := database.Exec(
			db.Rebind(siteDialect, query),
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
		siteRegistry := analyticsRegistry(c, registry)
		metrics := filterReadableAnalyticsMetrics(c, siteRegistry, resolveMetrics(c, siteRegistry, dialect))
		c.JSON(http.StatusOK, Response{Data: metrics})
	})

	ag.GET("/metrics/:name", func(c *gin.Context) {
		siteRegistry := analyticsRegistry(c, registry)
		metrics := resolveMetrics(c, siteRegistry, dialect)
		for _, m := range metrics {
			if m.Name == c.Param("name") {
				if !analyticsSourceReadable(c, siteRegistry, m.DocType) {
					c.Status(http.StatusNotFound)
					return
				}
				c.JSON(http.StatusOK, Response{Data: m})
				return
			}
		}
		writeError(c, http.StatusNotFound, "analytics.metric_not_found", "Metric not found", nil)
	})

	ag.POST("/metrics/:name/query", func(c *gin.Context) {
		var req analytics.QueryRequest
		if !decodeAnalyticsQueryRequest(c, &req) {
			return
		}
		if req.Metric != "" && req.Metric != c.Param("name") {
			badRequestError(c, "analytics.query_invalid", "Query metric must match the route metric.", nil)
			return
		}
		req.Metric = c.Param("name")
		qe := getQueryEngine(c, siteDB, dialect)
		if qe == nil {
			writeError(c, http.StatusServiceUnavailable, "server.store_unavailable", "Analytics not available for this site", nil)
			return
		}

		metrics := resolveMetrics(c, analyticsRegistry(c, registry), dialect)
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
		if !checkAnalyticsRead(c, analyticsRegistry(c, registry), metric.DocType) {
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
		doctypeName := c.Param("doctype")
		siteRegistry := analyticsRegistry(c, registry)
		if siteRegistry == nil || siteRegistry.Get(doctypeName) == nil {
			writeError(c, http.StatusNotFound, "doctype.not_found", "DocType not found", nil)
			return
		}
		if !checkAnalyticsRead(c, siteRegistry, doctypeName) {
			return
		}
		qe := getQueryEngine(c, siteDB, dialect)
		if qe == nil {
			writeError(c, http.StatusServiceUnavailable, "server.store_unavailable", "Analytics not available for this site", nil)
			return
		}
		metrics := resolveMetrics(c, siteRegistry, dialect)
		// Build the requested DocType's generated metrics directly as a safety
		// net for path-based tenant requests and older metric registries.
		if dt := siteRegistry.Get(doctypeName); dt != nil {
			metrics = analytics.GenerateMetrics(dt)
		}
		insights, err := qe.ResolveInsights(doctypeName, metrics)
		if err != nil {
			internalError(c, "insights query failed", err)
			return
		}

		c.JSON(http.StatusOK, Response{Data: insights})
	})
}

func applyProjectionActorScopes(c *gin.Context, qe *analytics.QueryEngine) {
	if c == nil || qe == nil {
		return
	}
	if value, ok := c.Get("analytics_projection_actor_scopes"); ok {
		if scopes, ok := value.(map[string]analytics.ProjectionActorScope); ok {
			qe.ProjectionActorScopes = scopes
		}
	}
}

const analyticsQueryRequestBodyLimit = 64 << 10

func decodeAnalyticsQueryRequest(c *gin.Context, target *analytics.QueryRequest) bool {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, analyticsQueryRequestBodyLimit)
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		message := "Analytics query body is invalid or exceeds 64 KiB."
		badRequestError(c, "analytics.query_invalid", message, nil)
		return false
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		badRequestError(c, "analytics.query_invalid", "Analytics query body must be a JSON object.", nil)
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		badRequestError(c, "analytics.query_invalid", "Analytics query contains invalid or unknown fields.", nil)
		return false
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		badRequestError(c, "analytics.query_invalid", "Analytics query body must contain exactly one JSON object.", nil)
		return false
	}
	if err := target.Validate(); err != nil {
		badRequestError(c, "analytics.query_invalid", err.Error(), nil)
		return false
	}
	return true
}

func analyticsRegistry(c *gin.Context, fallback *doctype.Registry) *doctype.Registry {
	if value, ok := c.Get("site_registry"); ok {
		if registry, ok := value.(*doctype.Registry); ok && registry != nil {
			return registry
		}
	}
	return fallback
}

func loadSemanticReports(c *gin.Context, database *sql.DB, dialect db.Dialect) ([]analytics.ReportDefinition, error) {
	if database == nil {
		return []analytics.ReportDefinition{}, nil
	}
	siteName := c.GetString("site_name")
	var configJSON string
	err := database.QueryRow(db.Rebind(siteDialectFromContext(c, dialect), `SELECT config FROM _kora_config_version WHERE site = ? AND status = 'Active' ORDER BY version DESC LIMIT 1`), siteName).Scan(&configJSON)
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

func loadAvailableReports(c *gin.Context, registry *doctype.Registry, database *sql.DB, dialect db.Dialect) ([]analytics.ReportDefinition, error) {
	docTypes := make([]*doctype.DocType, 0)
	if registry != nil {
		for _, name := range registry.Names() {
			docTypes = append(docTypes, registry.Get(name))
		}
	}
	catalog := analytics.BuildSemanticCatalog(docTypes)
	configured, err := loadSemanticReports(c, database, dialect)
	if err != nil {
		return nil, err
	}
	// Explicit reports override generated defaults with the same name.
	byName := make(map[string]analytics.ReportDefinition, len(configured))
	for _, report := range analytics.GenerateDefaultReports(catalog) {
		byName[report.Name] = report
	}
	for _, report := range configured {
		byName[report.Name] = report
	}
	reports := make([]analytics.ReportDefinition, 0, len(byName))
	for _, report := range byName {
		reports = append(reports, report)
	}
	sort.Slice(reports, func(i, j int) bool { return reports[i].Name < reports[j].Name })
	return reports, nil
}

// resolveMetrics returns all metrics for the current site: auto-generated from
// DocType metadata plus any user-defined custom metrics.
func resolveMetrics(c *gin.Context, registry *doctype.Registry, dialect db.Dialect) []*analytics.Metric {
	var all []*analytics.Metric
	for _, name := range registry.Names() {
		dt := registry.Get(name)
		if dt == nil {
			continue
		}
		all = append(all, analytics.GenerateMetrics(dt)...)
	}
	// Load custom metrics from DB.
	database := getSiteDB(c, nil)
	if database != nil {
		siteName := c.GetString("site_name")
		rows, err := database.Query(
			db.Rebind(siteDialectFromContext(c, dialect), "SELECT name, label, type, doctype, field_name, link_field, group_by_field FROM _kora_analytics_metric WHERE site = ?"),
			siteName,
		)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var m analytics.Metric
				if err := rows.Scan(&m.Name, &m.Label, &m.Type, &m.DocType, &m.Field, &m.LinkField, &m.GroupByField); err != nil {
					continue
				}
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
func getQueryEngine(c *gin.Context, fallbackDB *sql.DB, dialect db.Dialect) *analytics.QueryEngine {
	siteName := c.GetString("site_name")
	if siteName == "" {
		return nil
	}
	db := getSiteDB(c, fallbackDB)
	if db == nil {
		return nil
	}
	return &analytics.QueryEngine{DB: db, SiteName: siteName, Dialect: siteDialectFromContext(c, dialect)}
}
