package site

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/asenawritescode/kora/configstore"
	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
)

func moduleRevision(t *testing.T, conn *sql.DB, d kdb.Dialect, tenant string) (uint64, int) {
	t.Helper()
	snap, err := configstore.NewModuleStore(conn, d).Load(contract.TenantID(tenant))
	if err != nil {
		t.Fatal(err)
	}
	active := 0
	for _, in := range snap.Installations {
		if in.State == string(contract.ModuleActive) {
			active++
		}
	}
	return snap.Revision, active
}

func versionCount(t *testing.T, conn *sql.DB) int {
	t.Helper()
	var n int
	if err := conn.QueryRow("SELECT COUNT(*) FROM _kora_config_version").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// runImportModules proves modules and the config version commit together.
func runImportModules(t *testing.T, driver string, conn *sql.DB, dbName string) {
	dialect := kdb.Resolve(driver)
	if err := BootstrapSystemTables(conn, dialect); err != nil {
		t.Fatal(err)
	}
	site, pos := "m238.example", filepath.Join("..", "config", "pos")
	// Injected CAS failure: another writer bumps the module revision after read.
	afterModuleRevisionRead = func() {
		tx, _ := conn.Begin()
		if err := (&configstore.Store{DB: conn, Dialect: dialect}).CommitModulesTx(t.Context(), tx, dialect, contract.TenantID(site), nil, "x", 0, nil, nil); err != nil {
			t.Error(err)
		}
		tx.Commit()
	}
	err := ImportConfig(conn, doctype.NewRegistry(), dbName, site, pos, dialect)
	afterModuleRevisionRead = nil
	if err == nil || !strings.Contains(err.Error(), "revision conflict") {
		t.Fatalf("want CAS failure, got %v", err)
	}
	if n := versionCount(t, conn); n != 0 {
		t.Fatalf("config versions after rollback = %d, want 0", n)
	}
	if rev, active := moduleRevision(t, conn, dialect, site); rev != 1 || active != 0 {
		t.Fatalf("rollback left module rev=%d active=%d (only injected bump expected)", rev, active)
	}
	if err := ImportConfig(conn, doctype.NewRegistry(), dbName, site, pos, dialect); err != nil {
		t.Fatal(err)
	}
	rev, active := moduleRevision(t, conn, dialect, site)
	if rev < 2 || active != 4 {
		t.Fatalf("module rev=%d active=%d, want rev>=2 and 4 active (inventory+pos x core/app)", rev, active)
	}
	snap, err := configstore.NewModuleStore(conn, dialect).Load(contract.TenantID(site))
	if err != nil {
		t.Fatal(err)
	}
	ops, qs, ids := 0, 0, []string{}
	for _, m := range snap.Modules {
		ids = append(ids, string(m.ID))
		ops += len(m.PublishedOperations)
		qs += len(m.Queries)
	}
	sort.Strings(ids)
	if strings.Join(ids, ",") != "inventory.app,inventory.core,pos.app,pos.core" || ops != 11 || qs != 15 {
		t.Fatalf("modules %v ops=%d queries=%d, want 4 modules, 11 ops, 15 queries", ids, ops, qs)
	}
	versions := versionCount(t, conn)
	if err := ImportConfig(conn, doctype.NewRegistry(), dbName, site, pos, dialect); err != nil {
		t.Fatal(err)
	}
	if rev2, _ := moduleRevision(t, conn, dialect, site); rev2 != rev {
		t.Fatalf("re-import changed module revision %d -> %d", rev, rev2)
	}
	if n := versionCount(t, conn); n != versions {
		t.Fatalf("re-import created config version: %d -> %d", versions, n)
	}
}

func TestImportConfigCommitsPackModulesAtomicallyLibSQL(t *testing.T) {
	conn, err := sql.Open("sqlite3", fmt.Sprintf("file:%s?_foreign_keys=on", filepath.Join(t.TempDir(), "m.db")))
	if err != nil {
		t.Fatal(err)
	}
	conn.SetMaxOpenConns(1)
	defer conn.Close()
	runImportModules(t, "libsql", conn, "main")
}

func TestPackWithoutManifestRequiresOneWhenItHasQueries(t *testing.T) {
	dir := t.TempDir()
	q := filepath.Join(dir, "queries")
	if err := writeFile(filepath.Join(q, "x.yaml"), "name: x\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := parsePackSnapshot(dir); err == nil {
		t.Fatal("expected error for queries without pack.yaml")
	}
}

func writeFile(path, body string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(body), 0o600)
}
