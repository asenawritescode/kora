//go:build integration

package site

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
)

// Cloud install (ParseConfigPack) must produce the modules `kora config import`
// does, and they must commit to a real database.
func TestParseConfigPackModulesMatchImportAcrossDialects(t *testing.T) {
	const doctypeYAML = "name: Stock\nmodule: Inventory\nfields:\n  - fieldname: title\n    fieldtype: Data\n"
	const packYAML = "name: shop\nversion: \"1.2\"\n"
	dir := filepath.Join(t.TempDir(), "shop")
	if err := os.MkdirAll(filepath.Join(dir, "doctypes"), 0o755); err != nil {
		t.Fatal(err)
	}
	for p, c := range map[string]string{"pack.yaml": packYAML, "doctypes/stock.yaml": doctypeYAML} {
		if err := os.WriteFile(filepath.Join(dir, p), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	imported, err := parsePackSnapshot(dir)
	if err != nil {
		t.Fatal(err)
	}
	installed, _, err := ParseConfigPack("shop", []PackFile{
		{Path: "pack.yaml", Content: packYAML, ContentType: "pack"},
		{Path: "doctypes/stock.yaml", Content: doctypeYAML, ContentType: "doctype"},
	})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := contract.CanonicalManifestJSON(imported.Modules)
	b, _ := contract.CanonicalManifestJSON(installed.Modules)
	if len(installed.Modules) != 2 || !bytes.Equal(a, b) {
		t.Fatalf("install modules differ from import:\nimport  %s\ninstall %s", a, b)
	}
	for _, tc := range []struct{ driver, env string }{
		{"mysql", "KORA_POS_E2E_MYSQL_DSN"},
		{"postgres", "KORA_POS_E2E_POSTGRES_DSN"},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			dsn := os.Getenv(tc.env)
			if dsn == "" {
				t.Skipf("set %s to a disposable %s server", tc.env, tc.driver)
			}
			conn, name := posDB(t, tc.driver, dsn)
			dialect := kdb.Resolve(tc.driver)
			if err := BootstrapSystemTables(conn, dialect); err != nil {
				t.Fatal(err)
			}
			if err := ImportConfigFromSnapshotContext(context.Background(), conn, doctype.NewRegistry(), name, "m240.example", "m240.example", "install", installed, dialect, nil); err != nil {
				t.Fatal(err)
			}
			if _, active := moduleRevision(t, conn, dialect, "m240.example"); active != 2 {
				t.Fatalf("active modules = %d, want 2", active)
			}
		})
	}
}
