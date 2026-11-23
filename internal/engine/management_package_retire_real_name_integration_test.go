//go:build integration

package engine

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/configstore"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/site"
)

const erpsellPackRoot = "../../config/_parked/erpsell"

// erpsellReleaseFiles reads the current erp-sell pack the way the publish step does:
// one PackFile per file, typed by its path, pack.yaml included.
func erpsellReleaseFiles(t *testing.T) []contract.PackFile {
	t.Helper()
	return packTreeFiles(t, erpsellPackRoot)
}

// packTreeFiles reads one pack tree as PackFiles, pack.yaml included.
func packTreeFiles(t *testing.T, root string) []contract.PackFile {
	t.Helper()
	var files []contract.PackFile
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		contentType, ok := contract.PackageFileType(rel)
		if !ok {
			return nil
		}
		body, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		files = append(files, contract.PackFile{Path: rel, ContentType: contentType, Content: string(body)})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// parseViewFile parses a real view file with the production view parser.
func parseViewFile(t *testing.T, path string) *doctype.View {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	v, err := doctype.ParseViewYAML(body)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return v
}

// The shop's stored view is named by its YAML name: ("Owner Dashboard"), and pack.yaml retires
// it by that name. The upgrade must drop it and keep the other views, including a restaurant
// composition's "Menu and Tables".
func TestUpgradeDropsRealStoredOwnerDashboardName(t *testing.T) {
	for _, dc := range []struct{ driver, env string }{
		{"mysql", "KORA_MANAGEMENT_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "KORA_MANAGEMENT_DISPOSABLE_POSTGRES_DSN"},
	} {
		t.Run(dc.driver, func(t *testing.T) {
			dsn := strings.TrimSpace(os.Getenv(dc.env))
			if dsn == "" {
				t.Skipf("set %s to a disposable %s server", dc.env, dc.driver)
			}
			conn, info, cleanup := openManagementPackageTestDatabase(t, dc.driver, dsn)
			defer cleanup()
			dialect := db.Resolve(dc.driver)
			if err := site.BootstrapSystemTables(context.Background(), conn, dialect); err != nil {
				t.Fatal(err)
			}
			// The shop as installed on 2.1.0: the restaurant composition (erp-sell + menu-tables,
			// parsed from the pack trees) plus the real 2.1.0 owner view, which the current pack no longer ships.
			erp, _, err := site.ParseConfigPack("erpsell", erpsellReleaseFiles(t))
			if err != nil {
				t.Fatal(err)
			}
			menu, _, err := site.ParseConfigPack("menu-tables", packTreeFiles(t, "../../config/_parked/menu-tables"))
			if err != nil {
				t.Fatal(err)
			}
			installed, err := mergeManagementPackageSnapshot(erp, menu, packageAuthority{})
			if err != nil {
				t.Fatal(err)
			}
			installed.Views = append(installed.Views, parseViewFile(t, "testdata/erpsell_2_1_0_owner_dashboard.yaml"))
			if installed.Views[len(installed.Views)-1].Name != "Owner Dashboard" {
				t.Fatalf("fixture view name = %q, want the stored name %q", installed.Views[len(installed.Views)-1].Name, "Owner Dashboard")
			}
			if err = site.ImportConfigFromSnapshotContext(t.Context(), conn, doctype.NewRegistry(), info.DBName, "r2.example", "r2.example", "install", installed, dialect, nil); err != nil {
				t.Fatal(err)
			}
			release, _, err := site.ParseConfigPack("erpsell", erpsellReleaseFiles(t))
			if err != nil {
				t.Fatal(err)
			}
			if len(release.RetiredViews) != 1 || release.RetiredViews[0] != "Owner Dashboard" {
				t.Fatalf("release retires %v, want [Owner Dashboard]", release.RetiredViews)
			}
			merged, err := mergeManagementPackageSnapshot(installed, release, packageAuthority{})
			if err != nil {
				t.Fatal(err)
			}
			if err = site.ImportConfigFromSnapshotContext(t.Context(), conn, doctype.NewRegistry(), info.DBName, "r2.example", "r2.example", "upgrade", merged, dialect, nil); err != nil {
				t.Fatal(err)
			}
			got, _, err := configstore.NewStore(conn, dialect).LoadActiveSnapshot("r2.example")
			if err != nil {
				t.Fatal(err)
			}
			names := make([]string, 0, len(got.Views))
			for _, v := range got.Views {
				names = append(names, v.Name)
			}
			sort.Strings(names)
			if want := "Menu and Tables,POS Register"; strings.Join(names, ",") != want {
				t.Fatalf("views after upgrade = %v, want %s", names, want)
			}
		})
	}
}
