package engine

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/asenawritescode/kora/configstore"
	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/site"
)

// A version 2 register view shipped in a package's views/ directory is
// validated against the package's published surfaces and listed for the tenant.
func TestPackageRegisterViewIsListedForTenant(t *testing.T) {
	const registerView = `
name: Counter
route: /counter
type: register
layout: two_panel
resources:
  - {id: items, query: published.query, params: {owner_module_id: inventory.core, query_name: item_search}}
  - {id: session, query: published.query, params: {owner_module_id: pos.core, query_name: open_till}}
components:
  - {id: products, type: product_grid, version: 2, resource_bindings: {items: items}, bindings: {title: item_name, price: price, sku: barcode}}
  - {id: cart, type: cart_panel, version: 2, region: side}
  - id: tender
    type: tender_panel
    version: 2
    region: side
    bindings: {submit_operation_ref: %s}
    context: {till_session: session.name}
`
	tests := []struct {
		name, operation, wantErr string
	}{
		{name: "line entry and payment operation", operation: "pos.core~pos.checkout~record_sale"},
		{name: "operation without line entry", operation: "pos.core~pos.checkout~void_sale", wantErr: "reference.operation_incompatible"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			database, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "t.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { database.Close() })
			dialect := db.Resolve("libsql")
			if err := site.BootstrapSystemTables(database, dialect); err != nil {
				t.Fatal(err)
			}
			const siteName = "register-view.example.test"
			files := append(composedRetailFiles(t), contract.PackFile{Path: "views/counter.yaml", ContentType: "view", Content: strings.Replace(registerView, "%s", tc.operation, 1)})
			incoming, scripts, err := site.ParseConfigPack("retail-pos", files)
			if err != nil {
				t.Fatalf("parse package: %v", err)
			}
			store := configstore.NewStore(database, dialect)
			current, err := store.CollectSnapshot(doctype.NewRegistry(), siteName)
			if err != nil {
				t.Fatal(err)
			}
			target, err := mergeManagementPackageSnapshot(current, incoming)
			if err != nil {
				t.Fatalf("merge package: %v", err)
			}
			err = site.ImportConfigFromSnapshotContext(t.Context(), database, doctype.NewRegistry(), "main", siteName, siteName, "install", target, dialect, scripts)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("install err = %v, want %s", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("install package: %v", err)
			}
			views, err := store.LoadViews(siteName)
			if err != nil {
				t.Fatal(err)
			}
			listed := false
			for _, view := range views {
				listed = listed || view.Name == "Counter" && len(view.Resources) == 2 && len(view.Components) == 3 && view.Components[2].Version == 2 && view.Components[2].Context["till_session"] == "session.name"
			}
			if !listed {
				t.Fatalf("register view not listed with its resources for tenant: %+v", views)
			}
			// A later activation revalidates the persisted view.
			persisted := *target
			persisted.Views = views
			if findings := doctype.ValidateSnapshotReferences(&persisted, nil); len(findings) != 0 {
				t.Fatalf("persisted view findings = %+v", findings)
			}
		})
	}
}

// The real pos pack ships its register view: composed with the inventory pack it
// passes reference validation, installs on a fresh tenant and keeps its resources.
func TestPOSPackRegisterViewReachesTenant(t *testing.T) {
	database, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	dialect := db.Resolve("libsql")
	if err := site.BootstrapSystemTables(database, dialect); err != nil {
		t.Fatal(err)
	}
	const siteName = "pos-register.example.test"
	files := composedRetailFiles(t)
	if validation, err := site.ValidatePack(site.PackManifest{SchemaVersion: 1, Name: "retail-pos", Version: "1.0.1"}, files); err != nil || !validation.Valid {
		t.Fatalf("validate pack: %+v err %v", validation, err)
	}
	incoming, scripts, err := site.ParseConfigPack("retail-pos", files)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}
	store := configstore.NewStore(database, dialect)
	current, err := store.CollectSnapshot(doctype.NewRegistry(), siteName)
	if err != nil {
		t.Fatal(err)
	}
	target, err := mergeManagementPackageSnapshot(current, incoming)
	if err != nil {
		t.Fatalf("merge package: %v", err)
	}
	if findings := doctype.ValidateSnapshotReferences(target, nil); len(findings) != 0 {
		t.Fatalf("pack findings = %+v", findings)
	}
	if err := site.ImportConfigFromSnapshotContext(t.Context(), database, doctype.NewRegistry(), "main", siteName, siteName, "install", target, dialect, scripts); err != nil {
		t.Fatalf("install package: %v", err)
	}
	views, err := store.LoadViews(siteName)
	if err != nil {
		t.Fatal(err)
	}
	var register *doctype.View
	for _, view := range views {
		if view.Type == "register" {
			register = view
		}
	}
	if register == nil {
		t.Fatalf("pos register view not listed for tenant: %+v", views)
	}
	types := map[string]doctype.ViewComponent{}
	for _, component := range register.Components {
		types[component.Type] = component
	}
	tender := types["tender_panel"]
	if register.Layout != "register" || len(register.Resources) != 4 || len(register.Components) != 5 ||
		tender.Version != 2 || tender.Bindings["submit_operation_ref"] != "pos.core~pos.checkout~record_sale" ||
		tender.Context["till_session"] != "session.name" || tender.Context["register"] != "session.register" || tender.Context["location"] != "session.location" {
		t.Fatalf("register view lost its shape: %+v", register)
	}
}
