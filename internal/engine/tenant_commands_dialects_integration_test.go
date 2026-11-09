//go:build integration

package engine

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/internal/configstore"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/kernel"
	"github.com/asenawritescode/kora/internal/site"
	_ "github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"
)

func TestLiveTenantCommandsLoadFromActiveConfigAcrossDialects(t *testing.T) {
	for _, tc := range []struct{ driver, env string }{
		{"mysql", "KORA_CONFIG_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "KORA_CONFIG_DISPOSABLE_POSTGRES_DSN"},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			dsn := strings.TrimSpace(os.Getenv(tc.env))
			if dsn == "" {
				t.Skipf("set %s to a disposable database", tc.env)
			}
			database, err := sql.Open(tc.driver, dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			dialect := db.Resolve(tc.driver)
			if err := site.BootstrapSystemTables(database, dialect); err != nil {
				t.Fatal(err)
			}
			siteName := fmt.Sprintf("tenant-commands-%d.example.test", time.Now().UnixNano())
			defer database.Exec(db.Rebind(dialect, "DELETE FROM _kora_config_version WHERE site = ?"), siteName)

			ctx := context.Background()
			base := kernel.NewCommandRegistry()
			inherited, err := tenantCommandRegistry(ctx, database, dialect, siteName, base)
			if err != nil {
				t.Fatalf("tenant without configuration: %v", err)
			}
			if len(inherited.List()) != 0 {
				t.Fatalf("unexpected commands before activation: %d", len(inherited.List()))
			}

			def, err := kernel.ParseCommandResource([]byte("name: library.lend_book\nversion: 1\ninput:\n  record: Loan\ntransaction:\n  - create:\n      record: Loan\n      values:\n        book: $input.book\n"))
			if err != nil {
				t.Fatal(err)
			}
			commands, err := kernel.EncodeCommandResources([]*kernel.CommandResource{def})
			if err != nil {
				t.Fatal(err)
			}
			store := configstore.NewStore(database, dialect)
			if _, _, err := store.CreateConfigVersion(siteName, "test", "commands", "Active", &doctype.ConfigSnapshot{Commands: commands}); err != nil {
				t.Fatal(err)
			}
			registry, err := tenantCommandRegistry(ctx, database, dialect, siteName, base)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := registry.Lookup("library.lend_book"); !ok {
				t.Fatal("command from the active configuration was not loaded")
			}
		})
	}
}
