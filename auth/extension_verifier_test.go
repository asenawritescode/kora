package auth

import (
	"context"
	"database/sql"
	"net/http/httptest"
	"testing"

	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/extension"
	"github.com/gin-gonic/gin"
)

type fakeExtensionTokens map[string]extension.Credential

func (f fakeExtensionTokens) VerifyAccessToken(_ context.Context, site, tokenHash string) (extension.Credential, error) {
	credential, ok := f[site+"/"+tokenHash]
	if !ok {
		return extension.Credential{}, extension.ErrNotFound
	}
	return credential, nil
}

func TestSiteGuardUsesInjectedExtensionTokens(t *testing.T) {
	tokens := fakeExtensionTokens{
		"test.local/" + HashExtensionAccessToken("good"): {Name: "bot", APIPermissions: `[{"doctype":"Task","read":true}]`},
	}
	guard := &SiteGuard{ExtensionTokens: func(*sql.DB, kdb.QueryDialect) ExtensionTokenVerifier { return tokens }}

	tests := []struct {
		token string
		want  bool
	}{
		{"good", true},
		{"revoked", false},
	}
	for _, tt := range tests {
		t.Run(tt.token, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Set("site_db", &sql.DB{})
			c.Set("site_name", "test.local")
			c.Request = httptest.NewRequest("GET", "/api/v1/resource/Task", nil)
			if got := guard.authenticateExtension(c, tt.token); got != tt.want {
				t.Fatalf("authenticateExtension(%q) = %v, want %v", tt.token, got, tt.want)
			}
			if tt.want && c.GetString("extension_name") != "bot" {
				t.Fatalf("extension_name = %q, want bot", c.GetString("extension_name"))
			}
		})
	}
}

func TestNotifyExtensionTokensChangedReachesGuardHook(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var invalidated []string
	guard := &SiteGuard{ExtensionTokensChanged: func(site string) { invalidated = append(invalidated, site) }}
	router := gin.New()
	router.Use(guard.Middleware(true))
	router.GET("/api/ping", func(c *gin.Context) { NotifyExtensionTokensChanged(c, "acme") })

	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/ping", nil))
	if len(invalidated) != 1 || invalidated[0] != "acme" {
		t.Fatalf("invalidated = %v, want [acme]", invalidated)
	}

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	NotifyExtensionTokensChanged(c, "acme") // no hook installed: must be a no-op
}
