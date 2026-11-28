//go:build integration

package api

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/asenawritescode/kora/internal/configstore"
	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/disposabledb"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/orm"
	"github.com/asenawritescode/kora/internal/outbox"
	"github.com/asenawritescode/kora/internal/schema"
	"github.com/asenawritescode/kora/internal/script"
	ksite "github.com/asenawritescode/kora/internal/site"
)

// A shop installed before pack ownership keeps its unnamespaced hook rows. Upgrading it
// imports the packs through the config import path (ImportConfigFromPackForTenant), which
// must leave one active hook per sale event, so a sale posts its journal entry exactly once.
func TestPackScriptsSellingPostingAfterUpgrade(t *testing.T) {
	for _, tc := range []struct{ dialect, env string }{
		{"mysql", packScriptsMySQLEnv},
		{"postgres", packScriptsPostgresEnv},
	} {
		t.Run(tc.dialect, func(t *testing.T) {
			dsn := os.Getenv(tc.env)
			if dsn == "" {
				t.Skipf("set %s", tc.env)
			}
			f := openUpgradedPackFixture(t, tc.dialect, dsn)
			customer, product := creditSeed(f)
			sellPostingCurrencies(f)

			var body string
			if err := f.db.QueryRow(kdb.Rebind(kdb.Resolve(f.dialect), `SELECT script FROM _kora_script WHERE name = ?`), "erpsell__Sale_before_save").Scan(&body); err != nil {
				t.Fatalf("read upgraded hook: %v", err)
			}
			if body == "" {
				t.Fatal("upgraded erpsell__Sale_before_save has an empty body: the hook would run as a no-op")
			}
			if got := f.count("_kora_script", "site = ? AND doctype = ? AND event = ? AND is_active = ?", f.site, "Sale", "before_save", true); got != 1 {
				t.Fatalf("active Sale before_save hooks = %d, want 1", got)
			}

			var legacyActive bool
			if err := f.db.QueryRow(kdb.Rebind(kdb.Resolve(f.dialect), `SELECT is_active FROM _kora_script WHERE name = ?`), "Sale_before_save").Scan(&legacyActive); err != nil {
				t.Fatalf("read legacy hook: %v", err)
			}
			if legacyActive {
				t.Fatal("legacy Sale_before_save is still active beside the namespaced hook")
			}

			before := sellPostingSnapshot(f)
			sale := f.create("Sale", sellPostingSaleBody("Paid", customer, product, 3, 100, 0, []any{
				map[string]any{"method": "Cash", "amount": 300},
			}))
			sellPostingEntry(f, "Sale", sale, "Sale")
			sellPostingMoved(f, before, map[string]float64{"Cash": 300, "Sales": -300, "Cost of Sales": 120, "Inventory": -120})
		})
	}
}

// openUpgradedPackFixture is a disposable database holding a pre-ownership shop: legacy
// unowned hook rows with empty bodies, then the books and selling packs imported as an
// upgrade does (doctypes and scripts; the pack's commands need the management path).
func openUpgradedPackFixture(t *testing.T, dialect, dsn string) *packFixture {
	t.Helper()
	ctx := context.Background()
	opened := disposabledb.Open(t, dialect, dsn)
	database := opened.DB
	database.SetMaxOpenConns(4)
	dl := kdb.Resolve(dialect)
	if err := ksite.BootstrapSystemTables(ctx, database, dl); err != nil {
		t.Fatalf("bootstrap system tables: %v", err)
	}
	for _, ddl := range extensibilityScriptTables(dialect) {
		if _, err := database.Exec(ddl); err != nil {
			t.Fatalf("create script tables: %v", err)
		}
	}
	for _, ddl := range configstore.ConfigRevisionSchema(dl) {
		if _, err := database.Exec(ddl); err != nil {
			t.Fatalf("create config revision table: %v", err)
		}
	}
	if err := configstore.SeedConfigRevision(ctx, database, dl); err != nil {
		t.Fatalf("seed config revision: %v", err)
	}

	site := "upgrade-" + dialect
	for _, hook := range []struct{ doctype, event string }{
		{"Sale", "before_save"}, {"Payment", "before_save"}, {"Expense", "before_save"}, {"Cash Movement", "before_save"},
	} {
		legacy := configstore.LegacyScriptName(hook.doctype, hook.event)
		if _, err := database.Exec(kdb.Rebind(dl, `INSERT INTO _kora_script (name, site, script_type, doctype, event, script, is_active) VALUES (?, ?, 'doc_event', ?, ?, '', ?)`), legacy, site, hook.doctype, hook.event, true); err != nil {
			t.Fatalf("insert legacy hook %s: %v", legacy, err)
		}
	}

	reg := doctype.NewRegistry()
	for _, pack := range []struct{ dir, name string }{{creditPackDir, "erpsell"}, {erpAccountingPack, "erp-accounting"}} {
		files := upgradeImportFiles(t, pack.dir)
		if pack.name == "erp-accounting" {
			files = append(files, upgradeCurrencyFile(t))
		}
		if err := ksite.ImportConfigFromPackForTenant(database, reg, opened.Name, site, site, pack.name, files, dl); err != nil {
			t.Fatalf("upgrade import %s: %v", pack.name, err)
		}
	}
	// Each import replaces the registry with its own pack, so reload every DocType the site
	// now holds. The Administrator may do everything, as on a real workspace.
	live, err := configstore.NewStore(database, dl).LoadAll(site)
	if err != nil {
		t.Fatalf("load imported doctypes: %v", err)
	}
	var permissions []*doctype.Permission
	for _, dt := range live {
		permissions = append(permissions, &doctype.Permission{
			Doctype: dt.Name, Role: doctype.AdminRole, Read: true, Write: true, Create: true, Delete: true, Submit: true,
		})
	}
	reg.LoadFull(live, []*doctype.Role{{Name: doctype.AdminRole}}, permissions)
	if err := schema.MigrateSiteFromRegistry(database, opened.Name, reg, dl); err != nil {
		t.Fatalf("migrate pack schema: %v", err)
	}

	writer := outbox.NewSQLWriter(dl)
	store := &script.Store{DB: database, Dialect: dl}
	handler := NewHandler(reg, &orm.TxManager{DB: database, Registry: reg, Dialect: dl, Outbox: writer, SiteName: site})
	handler.SiteOutboxes = map[string]outbox.Writer{site: writer}
	handler.SiteScriptStores = map[string]*script.Store{site: store}
	handler.ScriptRunner = script.NewEmbeddedRunner(script.DefaultEmbeddedConfig())
	return &packFixture{t: t, handler: handler, db: database, reg: reg, site: site, dialect: dialect, roles: []string{doctype.AdminRole}}
}

// upgradeImportFiles reads a pack's doctypes, scripts, roles and permissions as the
// config import path takes them.
func upgradeImportFiles(t *testing.T, dir string) []ksite.PackFile {
	t.Helper()
	var files []ksite.PackFile
	for root, contentType := range map[string]string{"doctypes": "doctype", "scripts": "script"} {
		err := filepath.WalkDir(filepath.Join(dir, root), func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return err
			}
			rel, err := filepath.Rel(dir, path)
			if err != nil {
				return err
			}
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			files = append(files, ksite.PackFile{Path: filepath.ToSlash(rel), ContentType: contentType, Content: string(body)})
			return nil
		})
		if err != nil {
			t.Fatalf("read %s of %s: %v", root, dir, err)
		}
	}
	for name, contentType := range map[string]string{"roles.yaml": "roles", "permissions.yaml": "permissions"} {
		body, err := os.ReadFile(filepath.Join(dir, name))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatalf("read %s of %s: %v", name, dir, err)
		}
		files = append(files, ksite.PackFile{Path: name, ContentType: contentType, Content: string(body)})
	}
	return files
}

// upgradeCurrencyFile is the selling pack's Currency doctype. The books' Account links to it,
// and a pack's own snapshot must hold every doctype it links to.
func upgradeCurrencyFile(t *testing.T) ksite.PackFile {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(creditPackDir, "doctypes", "currency.yaml"))
	if err != nil {
		t.Fatalf("read currency doctype: %v", err)
	}
	return ksite.PackFile{Path: "doctypes/currency.yaml", ContentType: "doctype", Content: string(body)}
}
