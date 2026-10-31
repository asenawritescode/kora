package engine

import (
	"context"
	"database/sql"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

const platformReadinessTimeout = 2 * time.Second

// PlatformReadinessHandler probes the canonical directory database using a
// bounded request context. It deliberately reports a stable component code,
// never a provider error that could contain connection details.
type readinessWorkAdmission interface {
	Acquire(context.Context) (func(), error)
}

func PlatformReadinessHandler(platformDB *sql.DB, admissions ...readinessWorkAdmission) gin.HandlerFunc {
	var admission readinessWorkAdmission
	if len(admissions) > 0 {
		admission = admissions[0]
	}
	return func(c *gin.Context) {
		if platformDB == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "not_ready", "component": "platform_directory"})
			return
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), platformReadinessTimeout)
		defer cancel()
		release, err := acquireProbeAdmission(ctx, admission)
		if err == nil {
			err = platformDB.PingContext(ctx)
			release()
		}
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "not_ready", "component": "platform_directory"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ready"})
	}
}

func acquireProbeAdmission(ctx context.Context, admission readinessWorkAdmission) (func(), error) {
	if admission == nil {
		return func() {}, nil
	}
	return admission.Acquire(ctx)
}
