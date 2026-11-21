package api

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"

	"github.com/gin-gonic/gin"

	"github.com/asenawritescode/kora/internal/api/ai"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/policy"
)

// metricsRunner returns the runner the AI chat uses for its metrics tools. Each read is served by the handler
// the API serves for that route, on a context carrying the request's site, database and verified execution, so
// it applies the same grants, scope and business clock as a direct call. Nothing is read around the handlers.
func (h *Handler) metricsRunner(c *gin.Context) ai.MetricsRunner {
	return serveMetricsRoutes(h.metrics, c.Keys, c.Request)
}

// serveMetricsRoutes returns a runner whose reads run the metrics handlers on a request carrying keys, cloned from base.
func serveMetricsRoutes(m metricsAPI, keys map[any]any, base *http.Request) ai.MetricsRunner {
	return func(route ai.MetricsRoute, body []byte) ai.MetricsResponse {
		rec := httptest.NewRecorder()
		sub, _ := gin.CreateTestContext(rec)
		for key, value := range keys {
			sub.Set(key, value)
		}
		sub.Request = base.Clone(base.Context())
		sub.Request.Body = io.NopCloser(bytes.NewReader(body))
		sub.Request.ContentLength = int64(len(body))
		sub.Request.Method = http.MethodGet
		if route == ai.MetricsRouteQuery || route == ai.MetricsRouteDrill {
			sub.Request.Method = http.MethodPost
		}
		dispatchMetricsRoute(m, sub, route)
		return ai.MetricsResponse{Status: rec.Code, Body: rec.Body.Bytes()}
	}
}

// dispatchMetricsRoute runs the handler behind one metrics route. An unknown route answers 404.
func dispatchMetricsRoute(m metricsAPI, c *gin.Context, route ai.MetricsRoute) {
	switch route {
	case ai.MetricsRouteMetrics:
		m.list(c)
	case ai.MetricsRouteExplore:
		exploreCatalog(c, m.registry)
	case ai.MetricsRouteReports:
		reportsAPI{metrics: m}.list(c)
	case ai.MetricsRouteQuery:
		m.query(c)
	case ai.MetricsRouteDrill:
		m.drill(c)
	default:
		c.Status(http.StatusNotFound)
	}
}

// MetricsCaller is the verified caller of metrics reads made outside an HTTP request, such as the MCP server.
type MetricsCaller struct {
	Context   context.Context
	Site      string
	TenantID  string
	Execution policy.Execution
}

// MetricsReader serves metrics reads for a verified caller outside the HTTP router. The reads use the API's handlers,
// so they apply the same grants, scope and business clock as a direct call.
type MetricsReader struct {
	api      metricsAPI
	database *sql.DB
	dialect  db.Dialect
}

// NewMetricsReader returns the reader of one site. Keep one per site: it caches the site's metric definitions.
func NewMetricsReader(registry *doctype.Registry, database *sql.DB, dialect db.Dialect) *MetricsReader {
	return &MetricsReader{api: newMetricsAPI(registry, database, dialect), database: database, dialect: dialect}
}

// Runner returns the runner of one caller. Each read runs on a request carrying the caller's site, tenant, database
// and verified execution, and on the caller's context.
func (r *MetricsReader) Runner(caller MetricsCaller) ai.MetricsRunner {
	keys := map[any]any{
		"site_name":        caller.Site,
		"site_id":          caller.TenantID,
		"site_db":          r.database,
		"site_db_type":     r.dialect.DriverName(),
		"policy_execution": caller.Execution,
	}
	base := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(caller.Context)
	return serveMetricsRoutes(r.api, keys, base)
}
