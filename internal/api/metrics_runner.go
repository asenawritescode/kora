package api

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"

	"github.com/gin-gonic/gin"

	"github.com/asenawritescode/kora/internal/api/ai"
)

// metricsRunner returns the runner the AI chat uses for its metrics tools. Each read is served by the handler
// the API serves for that route, on a context carrying the request's site, database and verified execution, so
// it applies the same grants, scope and business clock as a direct call. Nothing is read around the handlers.
func (h *Handler) metricsRunner(c *gin.Context) ai.MetricsRunner {
	return func(route ai.MetricsRoute, body []byte) ai.MetricsResponse {
		rec := httptest.NewRecorder()
		sub, _ := gin.CreateTestContext(rec)
		for key, value := range c.Keys {
			sub.Set(key, value)
		}
		sub.Request = c.Request.Clone(c.Request.Context())
		sub.Request.Body = io.NopCloser(bytes.NewReader(body))
		sub.Request.ContentLength = int64(len(body))
		sub.Request.Method = http.MethodGet
		if route == ai.MetricsRouteQuery || route == ai.MetricsRouteDrill {
			sub.Request.Method = http.MethodPost
		}
		h.dispatchMetricsRoute(sub, route)
		return ai.MetricsResponse{Status: rec.Code, Body: rec.Body.Bytes()}
	}
}

// dispatchMetricsRoute runs the handler behind one metrics route. An unknown route answers 404.
func (h *Handler) dispatchMetricsRoute(c *gin.Context, route ai.MetricsRoute) {
	m := h.metrics
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
