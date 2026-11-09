package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"

	"github.com/asenawritescode/kora/internal/api"
	"github.com/asenawritescode/kora/internal/site"
	"github.com/gin-gonic/gin"
)

// PlatformRouteConfig contains platform-plane routes that do not depend on a
// tenant runtime. Management inspection is optional for Cloud-free self-hosting.
type PlatformRouteConfig struct {
	Router                          *gin.Engine
	Directory                       api.ManagementDirectory
	ManagementToken                 string
	ManagementCallerID              string
	PublicAppBaseURL                string
	ManagementOperations            *api.ManagementOperationRoutes
	TenantConfigRevision            api.TenantConfigRevisionResolver
	ManagementCheckpointVerifier    CheckpointVerifier
	ManagementPackageResolver       ManagementPackageArtifactResolver
	DeploymentID                    string
	ManagementDatabaseAllocationRef string
	PrimaryDB                       *sql.DB
	PrimaryDBWorkAdmission          interface {
		Acquire(context.Context) (func(), error)
	}
	PlatformDB            *sql.DB
	PlatformWorkAdmission interface {
		Acquire(context.Context) (func(), error)
	}
	BinaryVersion string
}

// RegisterPlatformRoutes installs liveness, health, readiness, and optional
// authenticated management-inspection endpoints on the platform router.
func RegisterPlatformRoutes(config PlatformRouteConfig) error {
	if config.Router == nil {
		return errors.New("Engine HTTP router is required")
	}
	var managementHandler *api.ManagementInspectHandler
	if config.ManagementToken != "" {
		handler, err := api.NewManagementInspectHandler(config.Directory, config.ManagementToken, config.DeploymentID, config.PlatformWorkAdmission)
		if err != nil {
			return err
		}
		handler.Register(config.Router)
		handler.SetTenantConfigRevisionResolver(config.TenantConfigRevision)
		managementHandler = handler
	}
	if config.ManagementOperations != nil {
		if managementHandler == nil {
			return errors.New("management operation routes require the dedicated management bearer token")
		}
		if err := managementHandler.RegisterOperations(config.Router, *config.ManagementOperations); err != nil {
			return fmt.Errorf("register management operation routes: %w", err)
		}
	}
	config.Router.GET("/api/v1/ping", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"message": "pong", "version": config.BinaryVersion})
	})
	config.Router.GET("/api/ping", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"message": "pong", "version": config.BinaryVersion})
	})
	config.Router.GET("/health", func(c *gin.Context) {
		dbStatus := "connected"
		checkDB := config.PrimaryDB
		admission := config.PrimaryDBWorkAdmission
		if checkDB == nil {
			checkDB = config.PlatformDB
			admission = config.PlatformWorkAdmission
		}
		if checkDB != nil {
			ctx, cancel := context.WithTimeout(c.Request.Context(), platformReadinessTimeout)
			defer cancel()
			release, err := acquireProbeAdmission(ctx, admission)
			if err == nil {
				err = checkDB.PingContext(ctx)
				release()
			}
			if err != nil {
				dbStatus = "disconnected"
			}
		} else {
			dbStatus = "unknown"
		}
		status := "ok"
		if dbStatus != "connected" {
			status = "degraded"
		}
		c.JSON(http.StatusOK, gin.H{"status": status, "db": dbStatus})
	})
	config.Router.GET("/readyz", PlatformReadinessHandler(config.PlatformDB, config.PlatformWorkAdmission))
	return nil
}

// Keep the directory dependency explicit at this boundary: API management
// projections intentionally expose only credential-free directory reads.
var _ api.ManagementDirectory = (*site.SQLSiteRegistry)(nil)
