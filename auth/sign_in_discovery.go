package auth

import (
	"database/sql"
	"log/slog"
	"net/http"

	kdb "github.com/asenawritescode/kora/db"
	"github.com/gin-gonic/gin"
)

// SignInOption is one way to sign in to a tenant, as shown on the login page.
// It never carries issuer secrets or client credentials.
type SignInOption struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	DisplayName string `json:"display_name"`
	StartURL    string `json:"start_url"`
}

type signInDiscovery struct {
	MagicLink bool           `json:"magic_link"`
	Providers []SignInOption `json:"providers"`
}

func requestSiteDB(c *gin.Context, fallback *sql.DB) *sql.DB {
	if value, ok := c.Get("site_db"); ok {
		if siteDB, valid := value.(*sql.DB); valid && siteDB != nil {
			return siteDB
		}
	}
	return fallback
}

func requestDialect(c *gin.Context) kdb.Dialect {
	return kdb.Resolve(c.GetString("site_db_type"))
}

// localSignInDisabled writes a 403 and returns true when the tenant only
// accepts single sign-on. Policy read failures fail closed.
func localSignInDisabled(c *gin.Context, database *sql.DB) bool {
	allowed, err := LocalSignInAllowed(c.Request.Context(), database, requestDialect(c), c.GetString("site_name"))
	if err != nil {
		slog.Error("reading local sign-in policy", "site", c.GetString("site_name"), "error", err)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{"code": "auth.policy_unavailable", "message": "Sign-in is temporarily unavailable."}})
		return true
	}
	if !allowed {
		c.JSON(http.StatusForbidden, gin.H{"error": gin.H{"code": "auth.local_sign_in_disabled", "message": "This workspace uses single sign-on. Choose a sign-in provider instead."}})
		return true
	}
	return false
}

// registerSignInDiscovery lists the sign-in options a tenant's login page
// should offer: local magic links (when mail is configured and policy allows)
// and enabled OIDC providers.
func registerSignInDiscovery(group *gin.RouterGroup, db *sql.DB, magicLinkConfigured bool) {
	group.GET("/providers", func(c *gin.Context) {
		site := c.GetString("site_name")
		database := requestSiteDB(c, db)
		if database == nil || site == "" {
			c.JSON(http.StatusNotFound, gin.H{"error": gin.H{"code": "site.not_found", "message": "Site not found."}})
			return
		}
		ctx := c.Request.Context()
		dialect := requestDialect(c)
		allowed, err := LocalSignInAllowed(ctx, database, dialect, site)
		if err != nil {
			slog.Error("reading local sign-in policy", "site", site, "error", err)
			allowed = false
		}
		result := signInDiscovery{MagicLink: magicLinkConfigured && allowed, Providers: []SignInOption{}}
		providers, err := SQLAuthProviders{DB: database, Dialect: dialect}.List(ctx, site)
		if err != nil {
			slog.Error("listing sign-in providers", "site", site, "error", err)
		}
		for _, provider := range providers {
			if !provider.Enabled || provider.Validate() != nil {
				continue
			}
			result.Providers = append(result.Providers, SignInOption{
				ID: provider.ID, Type: "oidc", DisplayName: provider.DisplayName,
				StartURL: "/api/v1/auth/oidc/" + provider.ID + "/start",
			})
		}
		c.JSON(http.StatusOK, gin.H{"data": result})
	})
}
