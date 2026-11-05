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
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/api"
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/kernel"
	"github.com/asenawritescode/kora/policy"
	"github.com/asenawritescode/kora/site"
	"github.com/asenawritescode/kora/storage"
	"github.com/gin-gonic/gin"
)

// Acceptance test for the pos+inventory packs: a fresh tenant imports them,
// starts its runtime with no module blockers and publishes the operations and
// queries in the actor catalog, filtered by the actor's grants.
func TestPOSPackPublishesCatalogAcrossDialects(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct{ driver, env string }{
		{"mysql", "KORA_CONFIG_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "KORA_CONFIG_DISPOSABLE_POSTGRES_DSN"},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			dsn := strings.TrimSpace(os.Getenv(tc.env))
			if dsn == "" {
				t.Skipf("set %s to a disposable database", tc.env)
			}
			database, dbName := isolatedDB(t, tc.driver, dsn)
			dialect := db.Resolve(tc.driver)
			if err := site.BootstrapSystemTables(database, dialect); err != nil {
				t.Fatal(err)
			}
			siteName := fmt.Sprintf("pack-catalog-%d.example.test", time.Now().UnixNano())
			pos := filepath.Join("..", "..", "config", "pos")
			if err := site.ImportConfig(database, doctype.NewRegistry(), dbName, siteName, pos, dialect); err != nil {
				t.Fatalf("import pos+inventory: %v", err)
			}

			info := site.DBSiteInfo{SiteID: siteName, Name: siteName, DBType: tc.driver, DBName: dbName, Status: "active"}
			common := &site.CommonConfig{DBType: tc.driver}
			loaded, err := loadTenantRuntime(context.Background(), info, common, nil, "test", tenantRuntimeDependencies{
				connect:   func(*site.SiteConfig) (*sql.DB, error) { return database, nil },
				bootstrap: func(*sql.DB, db.Dialect) error { return nil },
			})
			if err != nil {
				t.Fatalf("load tenant runtime: %v", err)
			}
			install, err := NewTenantServiceInstaller(TenantServiceConfig{
				Common:         common,
				KernelCommands: kernel.NewCommandRegistry(),
				ResolveStorage: func(*site.SiteConfig) (storage.Backend, error) {
					return storage.New(storage.Config{Backend: "local", LocalPath: t.TempDir()})
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := install(info, loaded); err != nil {
				t.Fatalf("install tenant services: %v", err)
			}
			services := *loaded.RuntimeServices
			if services.ModuleBindings == nil || len(services.ModuleBindings.Blockers) != 0 {
				t.Fatalf("module bindings = %+v", services.ModuleBindings)
			}

			handler := api.NewHandler(loaded.Registry, nil)
			get := func(role string) contract.ApplicationCatalog {
				t.Helper()
				exec, err := policy.NewVerifiedExecution(contract.TenantID(siteName), contract.ActorContext{
					PrincipalID: role, SubjectUserID: role, PrincipalType: contract.PrincipalHuman, Site: siteName,
					Roles: []string{role}, AuthenticatedAt: time.Now().UTC(),
				})
				if err != nil {
					t.Fatal(err)
				}
				router := gin.New()
				router.GET("/api/v2/modules/catalog", func(c *gin.Context) {
					c.Set("site_name", siteName)
					c.Set("site_db", database)
					c.Set("site_registry", loaded.Registry)
					c.Set("site_runtime_services", services)
					c.Set("policy_execution", exec)
				}, handler.HandleApplicationCatalog)
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v2/modules/catalog", nil))
				if rec.Code != http.StatusOK {
					t.Fatalf("%s catalog status=%d body=%s", role, rec.Code, rec.Body.String())
				}
				var body struct {
					Data contract.ApplicationCatalog `json:"data"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatal(err)
				}
				return body.Data
			}
			refs := func(c contract.ApplicationCatalog) (ops, queries []string) {
				for _, o := range c.PublishedOperations {
					ops = append(ops, o.OperationRef)
				}
				for _, q := range c.PublishedQueries {
					queries = append(queries, string(q.OwnerModuleID)+"/"+q.Name)
				}
				sort.Strings(ops)
				sort.Strings(queries)
				return
			}
			checkSchemas := func(role string, c contract.ApplicationCatalog) {
				have := map[contract.SchemaRef]bool{}
				for _, s := range c.Schemas {
					have[s.Ref] = true
				}
				for _, o := range c.PublishedOperations {
					if !have[o.InputSchema] || !have[o.OutputSchema] {
						t.Errorf("%s: operation %s schema missing from catalog", role, o.OperationRef)
					}
				}
				for _, q := range c.PublishedQueries {
					if !have[q.InputSchema] || !have[q.OutputSchema] {
						t.Errorf("%s: query %s schema missing from catalog", role, q.Name)
					}
				}
			}

			owner := get("Owner")
			ownerOps, ownerQueries := refs(owner)
			t.Logf("owner operations=%v queries=%v", ownerOps, ownerQueries)
			if len(ownerOps) != 9 {
				t.Errorf("owner operations = %d, want 9: %v", len(ownerOps), ownerOps)
			}
			for _, name := range []string{"receive_stock", "adjust_stock", "open_shift", "close_shift", "open_till", "close_till", "record_sale", "return_sale", "void_sale"} {
				found := false
				for _, ref := range ownerOps {
					found = found || strings.HasSuffix(ref, "~"+name)
				}
				if !found {
					t.Errorf("owner catalog lacks operation %s", name)
				}
			}
			if len(ownerQueries) != 7 {
				t.Errorf("owner queries = %d, want 7: %v", len(ownerQueries), ownerQueries)
			}
			checkSchemas("Owner", owner)
			sale := false
			for _, o := range owner.PublishedOperations {
				if strings.HasSuffix(o.OperationRef, "~record_sale") {
					sale = true
					if o.Presentation.Layout != "line_entry" || o.Presentation.LineEntry == nil {
						t.Errorf("record_sale presentation = %+v, want line_entry", o.Presentation)
					}
				}
			}
			if !sale {
				t.Error("owner catalog lacks record_sale")
			}

			// A Cashier sees a subset: nothing outside the Owner's catalog.
			cashier := get("Cashier")
			cashierOps, cashierQueries := refs(cashier)
			t.Logf("cashier operations=%v queries=%v", cashierOps, cashierQueries)
			checkSchemas("Cashier", cashier)
			if len(cashierOps) == 0 || len(cashierOps) > len(ownerOps) || len(cashierQueries) > len(ownerQueries) {
				t.Errorf("cashier catalog not a non-empty subset: ops=%v queries=%v", cashierOps, cashierQueries)
			}
			inOwner := map[string]bool{}
			for _, ref := range append(append([]string{}, ownerOps...), ownerQueries...) {
				inOwner[ref] = true
			}
			for _, ref := range append(append([]string{}, cashierOps...), cashierQueries...) {
				if !inOwner[ref] {
					t.Errorf("cashier sees %s that owner does not", ref)
				}
			}
		})
	}
}

