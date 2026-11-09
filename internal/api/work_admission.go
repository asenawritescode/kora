package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/asenawritescode/kora/internal/dbaccess"
	"github.com/gin-gonic/gin"
)

// WorkAdmissionMiddleware bounds concurrent request work for the routed
// tenant. It is installed once on versioned API groups so nested handler and
// ORM operations share one admission slot for the whole request.
func WorkAdmissionMiddleware(registry *SiteRuntimeServices) gin.HandlerFunc {
	return func(c *gin.Context) {
		if admitted, _ := c.Get("tenant_work_admitted"); admitted == true {
			c.Next()
			return
		}
		path := c.Request.URL.Path
		method := c.Request.Method
		streamHandshake := (method == http.MethodGet || method == http.MethodHead) &&
			(strings.HasSuffix(path, "/system/realtime") || (strings.Contains(path, "/files/") && !strings.Contains(path, "/public/files/")))
		if strings.Contains(path, "/public/files/") || path == "/upload" || strings.HasSuffix(path, "/upload") {
			c.Next()
			return
		}
		services := RuntimeServiceForContext(c, registry, c.GetString("site_name"))
		if services.WorkAdmission == nil {
			c.Next()
			return
		}
		release, err := services.WorkAdmission.Acquire(c.Request.Context())
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				c.Abort()
				return
			}
			c.Header("Retry-After", "1")
			writeError(c, http.StatusServiceUnavailable, "dependency.unavailable", "Tenant work capacity is temporarily exhausted", nil)
			c.Abort()
			return
		}
		admittedContext, idempotentRelease := dbaccess.AttachWorkAdmission(c.Request.Context(), release)
		c.Request = c.Request.WithContext(admittedContext)
		defer idempotentRelease()
		c.Set("tenant_work_admitted", true)
		if streamHandshake {
			c.Set("tenant_stream_auth_release", idempotentRelease)
		}
		c.Next()
	}
}

// ReleaseTenantStreamAuthWork releases the short permit held while a streamed
// route authenticates. Stream handlers call it after middleware authorization
// and before storage or realtime streaming begins. Middleware retains a
// deferred idempotent release for early aborts and handler failures.
func ReleaseTenantStreamAuthWork(c *gin.Context) {
	if c == nil {
		return
	}
	if value, ok := c.Get("tenant_stream_auth_release"); ok {
		if release, ok := value.(func()); ok && release != nil {
			release()
		}
	}
}
