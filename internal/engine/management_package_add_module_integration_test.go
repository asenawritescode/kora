//go:build integration

package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/configstore"
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/management"
	"github.com/asenawritescode/kora/management/sqlstore"
	knet "github.com/asenawritescode/kora/net"
	"github.com/asenawritescode/kora/schema"
	"github.com/asenawritescode/kora/site"
)

// TestCharacterizeAddModuleToProvisionedTenant pins what happens TODAY when a
// module is added to a tenant provisioned from a composed template (kora#362,
// tracker kora-ui#106). It measures and asserts current behavior and fixes
// nothing. Each "kora#363" assertion is a known gap that the design for
// independently installable packages (option C, kora#363) will change; the test
// is meant to turn red when it does, and must then be updated, not skipped.
func TestCharacterizeAddModuleToProvisionedTenant(t *testing.T) {
	dsn := strings.TrimSpace(os.Getenv("KORA_MANAGEMENT_DISPOSABLE_MYSQL_DSN"))
	if dsn == "" {
		t.Skip("set KORA_MANAGEMENT_DISPOSABLE_MYSQL_DSN to a disposable loopback MySQL service")
	}
	database, info, cleanup := openManagementPackageTestDatabase(t, "mysql", dsn)
	defer cleanup()
	database.SetMaxOpenConns(4)
	ctx, cancel := context.WithTimeout(t.Context(), 180*time.Second)
	defer cancel()
	dialect := db.Resolve("mysql")
	tenant, err := contract.ParseTenantID(info.SiteID)
	ops, opsErr := sqlstore.New(database, dialect)
	h := &addModuleHarness{t: t, ctx: ctx, database: database, info: info, dialect: dialect, tenant: tenant, ops: ops}
	h.check(err)
	h.check(opsErr)
	h.check(site.BootstrapSystemTablesContext(ctx, database, dialect))
	h.check(ops.EnsureSchema(ctx))

	// Inventory-only template, pos module alone (it requires inventory), and a
	// template composing both.
	inventoryPack := composedPackFiles(t, filepath.Join("..", "..", "config", "inventory"), "inventory-1.0.1")
	inventoryTemplate := h.artifact("retail-inventory", "retail-inventory-1.0.0", "template", nil, inventoryPack,
		[]contract.ManifestComponent{{ReleaseID: "inventory-1.0.1", Selection: "required"}})
	posModule := h.artifact("pos", "pos-1.0.1", "module", []contract.ManifestPackRef{{PackageID: "inventory", Version: "1.0.0"}},
		h.plainPackFiles(filepath.Join("..", "..", "config", "pos")), nil)
	retailTemplate := h.artifact("retail-pos", "retail-pos-1.0.1", "template", nil, composedRetailFiles(t),
		[]contract.ManifestComponent{{ReleaseID: "inventory-1.0.1", Selection: "required"}, {ReleaseID: "pos-1.0.1", Selection: "required"}})

	// Step 1: provision from the inventory-only template. Measured: succeeds,
	// recording only the template as an installed package, not inventory (kora#363).
	if op := h.apply(inventoryTemplate); op.State != contract.OperationSucceeded {
		t.Fatalf("step 1 = %s %+v; want succeeded", op.State, op.Error)
	}
	s1 := h.observe()
	if strings.Join(s1.packages, ",") != "retail-inventory" || strings.Join(s1.modules, ",") != "inventory.app,inventory.core" {
		t.Errorf("step 1 packages %v modules %v; want only the template package and the two inventory modules (kora#363)", s1.packages, s1.modules)
	}

	// Step 2: 3 Items and a tenant field on the package-owned Item, added via the
	// calls the DocType admin update handler makes.
	for i := 1; i <= 3; i++ {
		_, err := database.Exec("INSERT INTO `tabItem` (name, owner, creation, modified, modified_by, doc_status, idx, item_name, uom) VALUES (?, 'tester', NOW(), NOW(), 'tester', 0, 0, ?, 'Each')", fmt.Sprintf("ITEM-%d", i), fmt.Sprintf("Item %d", i))
		h.check(err)
	}
	h.addItemField("loyalty_tag")
	_, err = database.Exec("UPDATE `tabItem` SET loyalty_tag = 'gold' WHERE name = 'ITEM-1'")
	h.check(err)
	s2 := h.observe()
	if s2.items != 3 || !s2.fieldInRegistry || !s2.fieldInTable || !s2.fieldInSnapshot || s2.tagValue != "gold" {
		t.Fatalf("step 2 baseline = %+v; want 3 items and loyalty_tag in registry, table, config snapshot", s2)
	}

	// Step 3: add pos alone. Measured: refused with CONFLICT. pos requires
	// inventory but no installed package has that id, so ValidateInstalledPackages
	// rejects it. Nothing changes: revision, packages, records, field (kora#363).
	op := h.apply(posModule)
	if op.State != contract.OperationFailed || op.Error == nil || op.Error.Type != contract.CodeConflict ||
		op.Error.Message != "package dependencies or conflicts do not match the installed tenant composition" {
		t.Errorf("step 3 = %s %+v; want failed CONFLICT on package dependencies (kora#363)", op.State, op.Error)
	}
	if s3 := h.observe(); !reflect.DeepEqual(s3, s2) {
		t.Errorf("step 3 changed the tenant: before %+v after %+v; want no change", s2, s3)
	}

	// Step 4: re-apply the same template. Measured: succeeds but is not a no-op.
	// The revision advances and the package's Item replaces the tenant's whole
	// Item DocType, so loyalty_tag leaves the registry and config snapshot.
	// Records and the orphaned column with its data stay. Packages unchanged (kora#363).
	if op = h.apply(inventoryTemplate); op.State != contract.OperationSucceeded {
		t.Fatalf("step 4 = %s %+v; want succeeded", op.State, op.Error)
	}
	want4 := s2
	want4.revision, want4.fieldInRegistry, want4.fieldInSnapshot = s2.revision+1, false, false
	s4 := h.observe()
	if !reflect.DeepEqual(s4, want4) {
		t.Errorf("step 4 observed %+v; want %+v (tenant field dropped from config, column and data kept; kora#363)", s4, want4)
	}

	// Step 5: the audit's alternative, a template composing inventory and pos.
	// Measured: applies, adds the pos modules, records a second template (not
	// the components), and the tenant field stays out of config (kora#363).
	if op = h.apply(retailTemplate); op.State != contract.OperationSucceeded {
		t.Fatalf("step 5 = %s %+v; want succeeded", op.State, op.Error)
	}
	want5 := s4
	want5.revision = s4.revision + 1
	want5.packages = []string{"retail-inventory", "retail-pos"}
	want5.modules = []string{"inventory.app", "inventory.core", "pos.app", "pos.core"}
	if s5 := h.observe(); !reflect.DeepEqual(s5, want5) {
		t.Errorf("step 5 observed %+v; want %+v (kora#363)", s5, want5)
	}

	// Step 6: module upgrade. Measured: a version change is blocked with
	// version_requires_verified_package_activation (kora#363).
	modules, err := configstore.NewModuleStore(database, dialect).Load(h.tenant)
	h.check(err)
	var core contract.ModuleInstallation
	for _, installation := range modules.Installations {
		if installation.ModuleID == "inventory.core" {
			core = installation
		}
	}
	upgrade := contract.PlanModuleChange{ModuleID: core.ModuleID, TargetState: contract.ModuleLifecycleState(core.State), TargetVersion: core.Version + ".next", ExpectedRevision: modules.Revision}
	upgrade.TargetState = contract.ModuleDraining
	plan, err := configstore.PlanModuleLifecycleChange(ctx, modules, upgrade)
	blocked := false
	for _, blocker := range plan.DependencyBlockers {
		blocked = blocked || blocker.Code == "version_requires_verified_package_activation"
	}
	if err != nil || !blocked {
		t.Errorf("step 6 upgrade plan = %+v, %v; want blocker version_requires_verified_package_activation (kora#363)", plan, err)
	}
}

type addModuleHarness struct {
	t        *testing.T
	ctx      context.Context
	database *sql.DB
	info     site.DBSiteInfo
	dialect  db.Dialect
	tenant   contract.TenantID
	ops      *sqlstore.Store
}

type addModuleArtifact struct {
	manifest contract.MarketplacePackageManifest
	files    []contract.PackFile
	digest   string
}

type addModuleObservation struct {
	revision                                       uint64
	packages, modules                              []string
	items                                          int
	fieldInRegistry, fieldInTable, fieldInSnapshot bool
	tagValue                                       string
}

func (h *addModuleHarness) check(err error) {
	h.t.Helper()
	if err != nil {
		h.t.Fatal(err)
	}
}

func (h *addModuleHarness) artifact(packageID, releaseID, kind string, requires []contract.ManifestPackRef, files []contract.PackFile, composition []contract.ManifestComponent) addModuleArtifact {
	manifest := contract.MarketplacePackageManifest{
		SchemaVersion: 1, PackageID: packageID, ReleaseID: releaseID, Version: "1.0.0", Kind: kind,
		Public:   contract.ManifestPublic{Slug: packageID, Name: packageID, Summary: packageID},
		Engine:   contract.ManifestEngine{MinVersion: "1.0.0"},
		Requires: requires, Composition: composition,
	}
	canonical, err := contract.CanonicalManifestJSON(manifest)
	h.check(err)
	digest, err := contract.PackDigest(canonical, files)
	h.check(err)
	return addModuleArtifact{manifest: manifest, files: files, digest: digest}
}

// apply runs one ApplyPackage operation through the real management executor on
// a tenant runtime freshly loaded from the database, until it is terminal.
func (h *addModuleHarness) apply(a addModuleArtifact) management.Operation {
	h.t.Helper()
	revision, err := configstore.CurrentConfigRevision(h.ctx, h.database, h.dialect)
	h.check(err)
	id := contract.OperationID(contract.NewID())
	command := contract.ApplyPackage{
		Meta:   contract.CommandMeta{IdempotencyKey: contract.IdempotencyKey("add-module-" + string(id[len(id)-8:]))},
		Tenant: h.tenant, ArtifactRef: a.manifest.ReleaseID, Digest: a.digest,
		ExpectedConfigRevision: contract.Revision(fmt.Sprint(revision)),
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/engine/marketplace/releases/"+a.manifest.ReleaseID || r.Header.Get("Authorization") != "Bearer add-module-token" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Kora-Artifact-Digest", a.digest)
		_ = json.NewEncoder(w).Encode(map[string]any{"manifest": a.manifest, "files": a.files, "digest": a.digest})
	}))
	defer server.Close()
	resolver, err := NewHTTPManagementPackageArtifactResolver(server.URL, "add-module-token", nil)
	h.check(err)
	loaded := installTenantServicesForTest(h.t, h.info, h.database)
	executor, err := newManagementPackageExecutor(packageExecutorDirectory{info: h.info}, packageExecutorAdmission{}, &packageExecutorCoordinator{}, knet.NewSiteRouter([]*knet.LoadedSite{loaded}), resolver)
	h.check(err)
	service, err := management.NewService(h.ops, packageExecutorIDs{id: string(id)}, time.Now)
	h.check(err)
	worker, err := management.NewWorker(h.ops, executor, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.check(err)
	_, err = service.Accept(h.ctx, "cloud-service", command)
	h.check(err)
	workerCtx, stop := context.WithCancel(h.ctx)
	done := make(chan error, 1)
	go func() {
		done <- worker.Run(workerCtx, management.WorkerConfig{Owner: "add-module-worker", Concurrency: 1, Lease: 10 * time.Second, Poll: 10 * time.Millisecond})
	}()
	var operation management.Operation
	for operation.State != contract.OperationSucceeded && operation.State != contract.OperationFailed && operation.State != contract.OperationRecoveryRequired {
		if operation, err = service.Get(h.ctx, "cloud-service", id); err != nil {
			h.t.Fatal(err)
		}
		select {
		case <-h.ctx.Done():
			h.t.Fatalf("package %s did not finish; last state %s", a.manifest.ReleaseID, operation.State)
		case <-time.After(20 * time.Millisecond):
		}
	}
	stop()
	h.check(<-done)
	return operation
}

// observe reads the config revision, installed package and module sets, and
// Item's records, registry, column and snapshot.
func (h *addModuleHarness) observe() addModuleObservation {
	h.t.Helper()
	var o addModuleObservation
	var err error
	o.revision, err = configstore.CurrentConfigRevision(h.ctx, h.database, h.dialect)
	h.check(err)
	modules, err := configstore.NewModuleStore(h.database, h.dialect).Load(h.tenant)
	h.check(err)
	for _, p := range modules.Packages {
		o.packages = append(o.packages, p.PackageID)
	}
	for _, m := range modules.Modules {
		o.modules = append(o.modules, string(m.ID))
	}
	loaded := installTenantServicesForTest(h.t, h.info, h.database)
	o.fieldInRegistry = loaded.Registry.Get("Item").GetField("loyalty_tag") != nil
	h.check(h.database.QueryRow("SELECT COUNT(*) FROM `tabItem`").Scan(&o.items))
	var columns int
	h.check(h.database.QueryRow("SELECT COUNT(*) FROM information_schema.columns WHERE table_schema = DATABASE() AND table_name = 'tabItem' AND column_name = 'loyalty_tag'").Scan(&columns))
	if o.fieldInTable = columns == 1; o.fieldInTable {
		h.check(h.database.QueryRow("SELECT COALESCE(loyalty_tag, '') FROM `tabItem` WHERE name = 'ITEM-1'").Scan(&o.tagValue))
	}
	snapshot, err := configstore.NewStore(h.database, h.dialect).CollectSnapshot(loaded.Registry, h.info.Name)
	h.check(err)
	for _, dt := range snapshot.DocTypes {
		o.fieldInSnapshot = o.fieldInSnapshot || dt.Name == "Item" && dt.GetField("loyalty_tag") != nil
	}
	return o
}

// addItemField mirrors HandleSystemDoctypeUpdate (activate=true): save the
// DocType, migrate, record a config version.
func (h *addModuleHarness) addItemField(fieldname string) {
	h.t.Helper()
	reg := installTenantServicesForTest(h.t, h.info, h.database).Registry
	raw, err := json.Marshal(reg.Get("Item"))
	h.check(err)
	var updated doctype.DocType
	h.check(json.Unmarshal(raw, &updated))
	updated.Fields = append(updated.Fields, doctype.Field{Fieldname: fieldname, Fieldtype: "Data", Label: "Loyalty Tag"})
	store := configstore.NewStore(h.database, h.dialect)
	h.check(store.SaveDocType(&updated, h.info.Name))
	reg.Register(&updated)
	h.check(schema.MigrateSiteFromRegistry(h.database, h.info.DBName, reg, h.dialect))
	snapshot, err := store.CollectSnapshot(reg, h.info.Name)
	h.check(err)
	_, _, err = store.CreateConfigVersion(h.info.Name, "tester", "Updated Item via web", "Active", snapshot)
	h.check(err)
}

// plainPackFiles reads a pack tree un-namespaced, as a standalone module ships.
func (h *addModuleHarness) plainPackFiles(root string) []contract.PackFile {
	var files []contract.PackFile
	h.check(fs.WalkDir(os.DirFS(root), ".", func(p string, d fs.DirEntry, err error) error {
		contentType, ok := contract.PackageFileType(p)
		if err != nil || d.IsDir() || !ok {
			return err
		}
		body, err := os.ReadFile(filepath.Join(root, p))
		files = append(files, contract.PackFile{Path: p, ContentType: contentType, Content: string(body)})
		return err
	}))
	return files
}
