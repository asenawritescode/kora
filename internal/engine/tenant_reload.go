package engine

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/asenawritescode/kora/internal/dbaccess"
	knet "github.com/asenawritescode/kora/internal/net"
	"github.com/asenawritescode/kora/internal/site"
	"github.com/gin-gonic/gin"
)

var (
	ErrReloadTenantNotFound = errors.New("tenant not found in directory")
	ErrReloadTenantConflict = errors.New("tenant runtime reload conflict")
)

type reloadConflictError struct{ cause error }

func (e reloadConflictError) Error() string { return e.cause.Error() }
func (e reloadConflictError) Unwrap() []error {
	return []error{ErrReloadTenantConflict, e.cause}
}

func asReloadConflict(err error) error {
	if err == nil {
		return nil
	}
	return reloadConflictError{cause: err}
}

// TenantReloadController binds the protected admin operation to the same
// Engine runtime coordinator used by directory feed and resync activation.
type TenantReloadController struct {
	Tenants       *knet.SiteRouter
	Directory     TenantRuntimeDirectory
	PlatformDB    *sql.DB
	WorkAdmission interface {
		Acquire(context.Context) (func(), error)
	}
	Coordinator *TenantRuntimeCoordinator
}

// Reload activates a currently registered tenant or adds one that is present
// in the platform registry but not yet loaded in this Engine.
func (c *TenantReloadController) Reload(ctx context.Context, name string) (string, error) {
	if c == nil || c.Tenants == nil || c.Directory == nil || c.PlatformDB == nil || c.Coordinator == nil {
		return "", errors.New("tenant reload controller is not fully configured")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("site is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if existing := c.Tenants.SnapshotByName(name); existing != nil {
		var info site.DBSiteInfo
		err := c.withDirectoryWork(ctx, "read tenant directory record", func() error {
			var lookupErr error
			info, lookupErr = c.Directory.GetByIDContext(ctx, string(existing.Activation.Tenant))
			return lookupErr
		})
		if err != nil {
			return "", fmt.Errorf("read current tenant directory record: %w", err)
		}
		var directoryRevision uint64
		err = c.withDirectoryWork(ctx, "read tenant directory revision", func() error {
			if contextual, ok := c.Directory.(interface {
				CurrentDirectoryRevisionContext(context.Context) (uint64, error)
			}); ok {
				var revisionErr error
				directoryRevision, revisionErr = contextual.CurrentDirectoryRevisionContext(ctx)
				return revisionErr
			}
			var revisionErr error
			directoryRevision, revisionErr = c.Directory.CurrentDirectoryRevision()
			return revisionErr
		})
		if err != nil {
			return "", fmt.Errorf("read current tenant directory revision: %w", err)
		}
		if err := c.Coordinator.Replace(ctx, info, directoryRevision); err != nil {
			return "reloaded", asReloadConflict(err)
		}
		return "reloaded", nil
	}

	var infos []site.DBSiteInfo
	err := c.withDirectoryWork(ctx, "discover tenant records", func() error {
		var readErr error
		infos, readErr = site.DiscoverSitesFromDB(ctx, c.PlatformDB)
		return readErr
	})
	if err != nil {
		return "", fmt.Errorf("discover tenant records: %w", err)
	}
	for _, info := range infos {
		if info.Name != name {
			continue
		}
		var directoryRevision uint64
		err := c.withDirectoryWork(ctx, "read tenant directory revision", func() error {
			if contextual, ok := c.Directory.(interface {
				CurrentDirectoryRevisionContext(context.Context) (uint64, error)
			}); ok {
				var revisionErr error
				directoryRevision, revisionErr = contextual.CurrentDirectoryRevisionContext(ctx)
				return revisionErr
			}
			var revisionErr error
			directoryRevision, revisionErr = c.Directory.CurrentDirectoryRevision()
			return revisionErr
		})
		if err != nil {
			return "", fmt.Errorf("read current tenant directory revision: %w", err)
		}
		if err := c.Coordinator.Add(ctx, info, directoryRevision); err != nil {
			return "loaded", err
		}
		return "loaded", nil
	}
	return "", ErrReloadTenantNotFound
}

func (c *TenantReloadController) withDirectoryWork(ctx context.Context, operation string, work func() error) error {
	if work == nil {
		return fmt.Errorf("%s: work is required", operation)
	}
	if c.WorkAdmission == nil {
		return work()
	}
	release, err := c.WorkAdmission.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("admit %s: %w", operation, err)
	}
	defer release()
	if err := ctx.Err(); err != nil {
		return err
	}
	return work()
}

// RegisterTenantReloadRoute binds the protected admin endpoint to the Engine
// controller. The token remains a dedicated reload credential.
func RegisterTenantReloadRoute(router *gin.Engine, token string, controller *TenantReloadController) error {
	if router == nil {
		return errors.New("Engine HTTP router is required")
	}
	if controller == nil {
		return errors.New("tenant reload controller is required")
	}
	router.POST("/_kora/admin/reload-site", func(c *gin.Context) {
		if token == "" {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "reload token is not configured"})
			return
		}
		provided := sha256.Sum256([]byte(c.GetHeader("Authorization")))
		want := sha256.Sum256([]byte("Bearer " + token))
		if subtle.ConstantTimeCompare(provided[:], want[:]) != 1 {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}
		var request struct {
			Site string `json:"site"`
		}
		if err := json.NewDecoder(c.Request.Body).Decode(&request); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		request.Site = strings.TrimSpace(request.Site)
		if request.Site == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "site is required"})
			return
		}
		status, err := controller.Reload(c.Request.Context(), request.Site)
		if errors.Is(err, dbaccess.ErrExhausted) || errors.Is(err, context.DeadlineExceeded) {
			c.Header("Retry-After", "1")
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "platform_capacity"})
			return
		}
		if errors.Is(err, ErrReloadTenantNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "site not found in registry"})
			return
		}
		if errors.Is(err, ErrReloadTenantConflict) {
			code := "site_alias_conflict"
			if status == "reloaded" {
				code = "site_reload_failed"
			}
			c.JSON(http.StatusConflict, gin.H{"error": code, "message": err.Error()})
			return
		}
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": status, "site": request.Site})
	})
	return nil
}
