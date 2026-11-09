//go:build integration

package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/api"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/kernel"
	"github.com/asenawritescode/kora/internal/policy"
	"github.com/asenawritescode/kora/internal/site"
	"github.com/asenawritescode/kora/internal/storage"
	"github.com/gin-gonic/gin"
)

// A workspace created through self-service signup has no module snapshot row.
// Its first sign-in reads the module catalog, which must be an empty catalog
// rather than "unavailable".
func TestBlankTenantApplicationCatalogIsEmptyNotUnavailable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dsn := strings.TrimSpace(os.Getenv("KORA_CONFIG_DISPOSABLE_MYSQL_DSN"))
	if dsn == "" {
		t.Skip("set KORA_CONFIG_DISPOSABLE_MYSQL_DSN to a disposable database")
	}
	database, dbName := isolatedDB(t, "mysql", dsn)
	dialect := db.Resolve("mysql")
	if err := site.BootstrapSystemTables(database, dialect); err != nil {
		t.Fatal(err)
	}
	siteName := fmt.Sprintf("blank-tenant-%d", time.Now().UnixNano())
	info := site.DBSiteInfo{SiteID: siteName, Name: siteName, DBType: "mysql", DBName: dbName, Status: "active"}
	common := &site.CommonConfig{DBType: "mysql"}
	loaded, err := loadTenantRuntime(context.Background(), info, common, nil, "test", tenantRuntimeDependencies{
		connect:   func(*site.Config) (*sql.DB, error) { return database, nil },
		bootstrap: func(*sql.DB, db.Dialect) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	install, err := NewTenantServiceInstaller(TenantServiceConfig{
		Common: common, KernelCommands: kernel.NewCommandRegistry(),
		ResolveStorage: func(*site.Config) (storage.Backend, error) {
			return storage.New(storage.Config{Backend: "local", LocalPath: t.TempDir()})
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := install(info, loaded); err != nil {
		t.Fatal(err)
	}
	services := *loaded.RuntimeServices
	execution, err := policy.NewVerifiedExecution(contract.TenantID(siteName), contract.ActorContext{
		PrincipalID: "owner", SubjectUserID: "owner", PrincipalType: contract.PrincipalHuman, Site: siteName,
		Roles: []string{"Administrator"}, AuthenticatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("site_name", siteName)
		c.Set("site_db", database)
		c.Set("site_db_type", "mysql")
		c.Set("site_registry", loaded.Registry)
		c.Set("site_runtime_services", services)
		c.Set("policy_execution", execution)
	})
	router.GET("/catalog", api.NewHandler(loaded.Registry, nil).HandleApplicationCatalog)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/catalog", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("blank tenant catalog status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response struct {
		Data contract.ApplicationCatalog `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Data.Modules) != 0 || len(response.Data.PublishedOperations) != 0 || len(response.Data.PublishedQueries) != 0 {
		t.Fatalf("blank tenant catalog is not empty: %+v", response.Data)
	}
}
