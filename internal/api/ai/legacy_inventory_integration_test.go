//go:build integration

package ai

import (
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	kdb "github.com/asenawritescode/kora/internal/db"
	_ "github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"
	_ "github.com/tursodatabase/libsql-client-go/libsql"
)

func TestLiveAIRunStatusInventoryAcrossDialects(t *testing.T) {
	for _, test := range []struct {
		name   string
		dsnEnv string
	}{
		{name: "mysql", dsnEnv: "KORA_AI_INVENTORY_DISPOSABLE_MYSQL_DSN"},
		{name: "postgres", dsnEnv: "KORA_AI_INVENTORY_DISPOSABLE_POSTGRES_DSN"},
		{name: "libsql", dsnEnv: "KORA_AI_INVENTORY_DISPOSABLE_LIBSQL_DSN"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dsn := os.Getenv(test.dsnEnv)
			if dsn == "" {
				t.Skipf("set %s to a disposable %s database", test.dsnEnv, test.name)
			}
			database, err := sql.Open(test.name, dsn)
			if err != nil {
				t.Fatalf("open disposable %s database: %v", test.name, err)
			}
			database.SetMaxOpenConns(1)
			t.Cleanup(func() { _ = database.Close() })
			if err := database.PingContext(t.Context()); err != nil {
				t.Fatalf("ping disposable %s database: %v", test.name, err)
			}
			dialect := kdb.Resolve(test.name)
			ctx := WithDialect(t.Context(), dialect)
			if err := EnsureAIRunTables(ctx, database, dialect); err != nil {
				t.Fatalf("ensure %s AI run table: %v", test.name, err)
			}

			suffix := fmt.Sprintf("%x", time.Now().UnixNano())
			siteID := "inventory-site-" + suffix
			selected := RunRecord{ID: "inventory-run-" + suffix, Site: siteID, Status: "planning", InputMessage: "disposable inventory fixture"}
			otherSite := RunRecord{ID: "inventory-other-site-run-" + suffix, Site: "other-" + suffix, Status: "completed", InputMessage: "disposable inventory fixture"}
			if err := UpsertRun(ctx, database, selected); err != nil {
				t.Fatalf("insert selected %s run: %v", test.name, err)
			}
			if err := UpsertRun(ctx, database, otherSite); err != nil {
				t.Fatalf("insert other-site %s run: %v", test.name, err)
			}
			t.Cleanup(func() {
				_, _ = database.Exec(`DELETE FROM _kora_ai_run WHERE id = `+dialect.Placeholder(1), selected.ID)
				_, _ = database.Exec(`DELETE FROM _kora_ai_run WHERE id = `+dialect.Placeholder(1), otherSite.ID)
			})

			inventory, err := InspectRunStatusInventory(ctx, database, dialect, siteID)
			if err != nil {
				t.Fatalf("inspect %s AI run statuses: %v", test.name, err)
			}
			if !inventory.TablePresent || len(inventory.Statuses) != 1 || inventory.Statuses[0] != (RunStatusCount{Status: "planning", Rows: 1}) {
				t.Fatalf("site-scoped %s status inventory = %#v", test.name, inventory)
			}
		})
	}
}
