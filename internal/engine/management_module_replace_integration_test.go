//go:build integration

package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/api"
	"github.com/asenawritescode/kora/internal/configstore"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/kernel"
	"github.com/asenawritescode/kora/internal/management/sqlstore"
	"github.com/asenawritescode/kora/internal/policy"
	"github.com/asenawritescode/kora/internal/site"
	"github.com/gin-gonic/gin"
)

// TestPackageActivationReplacesInstalledModuleAcrossDialects drives a standalone
// module package through the real management executor: v1 installs, a same-version
// content change and a downgrade are refused with their reason, and v2 replaces
// v1 with its new command definition, version and catalog entries. A refused
// attempt leaves v1 fully working.
func TestPackageActivationReplacesInstalledModuleAcrossDialects(t *testing.T) {
	for _, tc := range []struct{ name, driver, env string }{
		{"mysql", "mysql", "KORA_MANAGEMENT_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "postgres", "KORA_MANAGEMENT_DISPOSABLE_POSTGRES_DSN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dsn := strings.TrimSpace(os.Getenv(tc.env))
			if dsn == "" {
				t.Skipf("set %s to a disposable loopback SQL service", tc.env)
			}
			database, info, cleanup := openManagementPackageTestDatabase(t, tc.driver, dsn)
			defer cleanup()
			database.SetMaxOpenConns(4)
			ctx, cancel := context.WithTimeout(t.Context(), 240*time.Second)
			defer cancel()
			dialect := db.Resolve(tc.driver)
			tenant, err := contract.ParseTenantID(info.SiteID)
			if err != nil {
				t.Fatal(err)
			}
			ops, err := sqlstore.New(database, dialect)
			h := &addModuleHarness{t: t, ctx: ctx, database: database, info: info, dialect: dialect, tenant: tenant, ops: ops}
			h.check(err)
			h.check(site.BootstrapSystemTables(ctx, database, dialect))
			h.check(ops.EnsureSchema(ctx))

			v1 := h.artifact("inventory", "inventory-1", "module", nil, h.moduleReleaseFiles("1", "Adjust stock"), nil)
			v2 := h.artifact("inventory", "inventory-2", "module", nil, h.moduleReleaseFiles("2", "Adjust stock by count"), nil)
			// Not a valid module version: the activation must fail and leave v1 untouched.
			broken := h.artifact("inventory", "inventory-2-rc", "module", nil, h.moduleReleaseFiles("2.0.0-rc1", "Broken"), nil)
			downgrade := h.artifact("inventory", "inventory-0", "module", nil, h.moduleReleaseFiles("1", "Adjust stock"), nil)

			if op := h.apply(v1); op.State != contract.OperationSucceeded {
				t.Fatalf("install v1 = %s %+v; want succeeded", op.State, op.Error)
			}
			v1Revision := h.moduleRevision("inventory.core")
			if got := h.commandTitle(info.Name, "adjust_stock"); got != "Adjust stock" {
				t.Fatalf("v1 adjust_stock title = %q", got)
			}

			if op := h.apply(broken); op.State != contract.OperationFailed {
				t.Fatalf("broken v2 = %s %+v; want failed", op.State, op.Error)
			}
			if got, rev := h.moduleVersion("inventory.core"), h.moduleRevision("inventory.core"); got != "1.0.0" || rev != v1Revision {
				t.Fatalf("after failed v2: module %q revision %d; want 1.0.0 revision %d", got, rev, v1Revision)
			}
			if got := h.commandTitle(info.Name, "adjust_stock"); got != "Adjust stock" {
				t.Fatalf("after failed v2 adjust_stock title = %q; v1 must stay active", got)
			}

			if op := h.apply(v2); op.State != contract.OperationSucceeded {
				t.Fatalf("upgrade to v2 = %s %+v; want succeeded", op.State, op.Error)
			}
			if got := h.moduleVersion("inventory.core"); got != "2.0.0" {
				t.Fatalf("module after upgrade = %q; want 2.0.0", got)
			}
			if h.moduleRevision("inventory.core") <= v1Revision {
				t.Fatalf("binding revision did not advance on replacement: %d <= %d", h.moduleRevision("inventory.core"), v1Revision)
			}
			if got := h.commandTitle(info.Name, "adjust_stock"); got != "Adjust stock by count" {
				t.Fatalf("v2 adjust_stock title = %q; the new definition must be the active one", got)
			}

			// Installed v2, so a v1 release is a downgrade and is refused with the reason.
			if op := h.apply(downgrade); op.State != contract.OperationFailed || op.Error == nil || !strings.Contains(op.Error.Message, "downgrades are refused") {
				t.Fatalf("downgrade = %s %+v; want failed with the downgrade reason", op.State, op.Error)
			}
			if got := h.moduleVersion("inventory.core"); got != "2.0.0" {
				t.Fatalf("module after refused downgrade = %q; want 2.0.0", got)
			}
			if refs := h.ownerCatalogOperations(info.Name); !containsSuffix(refs, "~adjust_stock") {
				t.Fatalf("published catalog after v2 lacks adjust_stock: %v", refs)
			}
		})
	}
}

// moduleReleaseFiles reads the inventory pack with its pack version and the
// adjust_stock title replaced, so each release changes exactly what the test names.
func (h *addModuleHarness) moduleReleaseFiles(packVersion, adjustTitle string) []contract.PackFile {
	h.t.Helper()
	root := h.t.TempDir()
	h.check(filepath.WalkDir(filepath.Join("..", "..", "config", "inventory"), func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(filepath.Join("..", "..", "config", "inventory"), path)
		if err != nil {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		text := string(body)
		switch rel {
		case "pack.yaml":
			text = "name: inventory\nversion: \"" + packVersion + "\"\n"
		case filepath.Join("commands", "adjust_stock.yaml"):
			text = strings.Replace(text, "title: Adjust stock", "title: "+adjustTitle, 1)
		}
		target := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, []byte(text), 0o600)
	}))
	return h.plainPackFiles(root)
}

func (h *addModuleHarness) moduleVersion(id contract.ModuleID) string {
	h.t.Helper()
	snapshot, err := configstore.NewModuleStore(h.database, h.dialect).Load(h.tenant)
	h.check(err)
	for _, m := range snapshot.Modules {
		if m.ID == id {
			return m.Version
		}
	}
	h.t.Fatalf("module %s not installed", id)
	return ""
}

func (h *addModuleHarness) moduleRevision(id contract.ModuleID) uint64 {
	h.t.Helper()
	snapshot, err := configstore.NewModuleStore(h.database, h.dialect).Load(h.tenant)
	h.check(err)
	for _, installation := range snapshot.Installations {
		if installation.ModuleID == id {
			return installation.Revision
		}
	}
	h.t.Fatalf("installation %s not found", id)
	return 0
}

// commandTitle reads the active config snapshot, the definition the runtime loads.
func (h *addModuleHarness) commandTitle(siteName, name string) string {
	h.t.Helper()
	snapshot, _, err := configstore.NewStore(h.database, h.dialect).LoadActiveSnapshot(siteName)
	h.check(err)
	for _, raw := range snapshot.Commands {
		def, err := kernel.ParseCommandResourceJSON(raw)
		h.check(err)
		if def.Name == name && def.Namespace == "inventory" && def.Presentation != nil {
			return def.Presentation.Title
		}
	}
	h.t.Fatalf("command %s not in active snapshot", name)
	return ""
}

// ownerCatalogOperations returns the operation refs an Inventory Manager sees in the published catalog,
// read through the same handler the Studio catalog uses.
func (h *addModuleHarness) ownerCatalogOperations(siteName string) []string {
	h.t.Helper()
	gin.SetMode(gin.TestMode)
	loaded := installTenantServicesForTest(h.t, h.info, h.database)
	services := *loaded.RuntimeServices
	if services.ModuleBindings == nil || len(services.ModuleBindings.Blockers) != 0 {
		h.t.Fatalf("module bindings not ready: %+v", services.ModuleBindings)
	}
	exec, err := policy.NewVerifiedExecution(h.tenant, contract.ActorContext{
		PrincipalID: "Inventory Manager", SubjectUserID: "Inventory Manager", PrincipalType: contract.PrincipalHuman, Site: siteName,
		Roles: []string{"Inventory Manager"}, AuthenticatedAt: time.Now().UTC(),
	})
	h.check(err)
	router := gin.New()
	router.GET("/api/v2/modules/catalog", func(c *gin.Context) {
		c.Set("site_name", siteName)
		c.Set("site_db", h.database)
		c.Set("site_registry", loaded.Registry)
		c.Set("site_runtime_services", services)
		c.Set("policy_execution", exec)
	}, api.NewHandler(loaded.Registry, nil).HandleApplicationCatalog)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v2/modules/catalog", nil))
	if rec.Code != http.StatusOK {
		h.t.Fatalf("catalog status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data contract.ApplicationCatalog `json:"data"`
	}
	h.check(json.Unmarshal(rec.Body.Bytes(), &body))
	var refs []string
	for _, op := range body.Data.PublishedOperations {
		refs = append(refs, op.OperationRef)
	}
	return refs
}

func containsSuffix(values []string, suffix string) bool {
	for _, value := range values {
		if strings.HasSuffix(value, suffix) {
			return true
		}
	}
	return false
}
