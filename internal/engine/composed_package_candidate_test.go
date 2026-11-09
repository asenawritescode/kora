package engine

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/configstore"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/kernel"
	knet "github.com/asenawritescode/kora/internal/net"
	"github.com/asenawritescode/kora/internal/site"
	"github.com/asenawritescode/kora/internal/storage"
)

// composedPackFiles reads a pack tree and namespaces its files the way Cloud's
// ResolveComposition does for a template component.
func composedPackFiles(t *testing.T, root, releaseID string) []contract.PackFile {
	t.Helper()
	digest := sha256.Sum256([]byte(releaseID))
	ns := hex.EncodeToString(digest[:8])
	var files []contract.PackFile
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
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
		switch dir, rest, nested := strings.Cut(rel, "/"); {
		case contentType == "roles" || contentType == "permissions" || !nested:
			rel = path.Join("components", ns, path.Base(rel))
		default:
			rel = path.Join(dir, "components", ns, rest)
		}
		files = append(files, contract.PackFile{Path: rel, ContentType: contentType, Content: string(body)})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// composedRetailFiles is a template composing the inventory and pos packs.
func composedRetailFiles(t *testing.T) []contract.PackFile {
	t.Helper()
	return append(composedPackFiles(t, filepath.Join("..", "..", "config", "inventory"), "inventory-1.0.1"),
		composedPackFiles(t, filepath.Join("..", "..", "config", "pos"), "pos-1.0.1")...)
}

// installTenantServicesForTest loads the tenant runtime from its database and
// installs the real tenant services, as a runtime generation does.
func installTenantServicesForTest(t *testing.T, info site.DBSiteInfo, database *sql.DB) *knet.LoadedSite {
	t.Helper()
	common := &site.CommonConfig{DBType: info.DBType}
	loaded, err := loadTenantRuntime(context.Background(), info, common, nil, "test", tenantRuntimeDependencies{
		connect:   func(context.Context, *site.Config) (*sql.DB, error) { return database, nil },
		bootstrap: func(context.Context, *sql.DB, db.Dialect) error { return nil },
	})
	if err != nil {
		t.Fatalf("load tenant runtime: %v", err)
	}
	install, err := NewTenantServiceInstaller(TenantServiceConfig{
		Common:         common,
		KernelCommands: kernel.NewCommandRegistry(),
		ResolveStorage: func(*site.Config) (storage.Backend, error) {
			return storage.New(storage.Config{Backend: "local", LocalPath: t.TempDir()})
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := install(info, loaded); err != nil {
		t.Fatalf("install tenant services: %v", err)
	}
	return loaded
}

// A fresh tenant has no active commands or queries. Activation preflight of a
// composed POS+inventory package must bind the package's own local commands
// and queries, not only the commands already active on the tenant.
func TestComposedPackageCandidateBindingsOnFreshTenant(t *testing.T) {
	database, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	dialect := db.Resolve("libsql")
	if err = site.BootstrapSystemTables(context.Background(), database, dialect); err != nil {
		t.Fatal(err)
	}
	const siteName = "composed.example.test"
	info := site.DBSiteInfo{SiteID: siteName, Name: siteName, DBType: "libsql", DBName: "main", Status: "active"}
	loaded := installTenantServicesForTest(t, info, database)

	incoming, _, err := site.ParseConfigPack("retail-pos", composedRetailFiles(t))
	if err != nil {
		t.Fatalf("parse composed package: %v", err)
	}
	current, err := configstore.NewStore(database, dialect).CollectSnapshot(doctype.NewRegistry(), siteName)
	if err != nil {
		t.Fatal(err)
	}
	target, err := mergeManagementPackageSnapshot(current, incoming)
	if err != nil {
		t.Fatalf("merge composed package: %v", err)
	}
	if len(target.Modules) != 4 {
		t.Fatalf("composed target modules = %d, want 4", len(target.Modules))
	}
	tenant := contract.TenantID(siteName)
	packages := []contract.InstalledPackage{{PackageID: "retail-pos", ReleaseID: "retail-pos-1.0.1", Version: "1.0.1", Digest: "sha256:" + strings.Repeat("a", 64)}}
	plan, err := loaded.RuntimeServices.ResolveCandidateModuleBindings(
		contract.BusinessScope{TenantID: tenant, Kind: contract.ScopeKindTenant}, packages, target.Modules, target, nil, 1)
	if err != nil {
		t.Fatalf("candidate bindings for composed package: %v", err)
	}
	if len(plan.Blockers) != 0 {
		t.Fatalf("candidate binding blockers = %+v", plan.Blockers)
	}
}
