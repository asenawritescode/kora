//go:build integration

package ai

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/kernel"
	"github.com/asenawritescode/kora/orm"
	"github.com/asenawritescode/kora/outbox"
	ksite "github.com/asenawritescode/kora/site"
	_ "github.com/lib/pq"
	_ "github.com/tursodatabase/libsql-client-go/libsql"
)

// These adapter checks exercise the same AI-to-kernel boundary as the MySQL
// end-to-end test, using independent backend fixtures to catch dialect-specific
// transaction, placeholder, quoting, audit, receipt, or outbox regressions.
func TestAIRecordMutationAdapterUsesKernelAcrossDialects(t *testing.T) {
	for _, tc := range []struct {
		name string
		dsn  string
	}{
		{name: "postgres", dsn: "KORA_AI_MUTATION_POSTGRES_DSN"},
		{name: "libsql", dsn: "KORA_AI_MUTATION_LIBSQL_DSN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dsn := os.Getenv(tc.dsn)
			if dsn == "" {
				t.Skipf("set %s to a disposable %s database", tc.dsn, tc.name)
			}
			database, err := sql.Open(tc.name, dsn)
			if err != nil {
				t.Fatalf("open %s database: %v", tc.name, err)
			}
			database.SetMaxOpenConns(4)
			t.Cleanup(func() { _ = database.Close() })
			if err := database.Ping(); err != nil {
				t.Fatalf("ping %s database: %v", tc.name, err)
			}
			dialect := kdb.Resolve(tc.name)
			if err := ksite.BootstrapSystemTables(database, dialect); err != nil {
				t.Fatalf("bootstrap %s system tables: %v", tc.name, err)
			}

			suffix := fmt.Sprintf("%x", time.Now().UnixNano())
			site := "ai-kernel-" + tc.name + "-" + suffix
			dt := &doctype.DocType{Name: "AITask" + suffix, Fields: []doctype.Field{{Fieldname: "title", Fieldtype: "Data", Reqd: true}}}
			registry := doctype.NewRegistry()
			registry.LoadFull([]*doctype.DocType{dt}, []*doctype.Role{{Name: doctype.AdminRole}}, []*doctype.Permission{{
				Doctype: dt.Name, Role: doctype.AdminRole, Read: true, Create: true, Write: true, Delete: true,
			}})
			for _, statement := range dialect.CreateTable(dt) {
				if _, err := database.Exec(statement); err != nil {
					t.Fatalf("create %s table: %v", tc.name, err)
				}
			}
			t.Cleanup(func() {
				_, _ = database.Exec(kdb.Rebind(dialect, "DELETE FROM _kora_naming_series WHERE doctype = ?"), dt.Name)
				_, _ = database.Exec("DROP TABLE " + dialect.QuoteIdent(dt.RawTableName()))
				for _, table := range []string{"_kora_operation_audit", "_kora_idempotency_receipt", "_kora_outbox"} {
					_, _ = database.Exec("DELETE FROM "+dialect.QuoteIdent(table)+" WHERE site = ?", site)
				}
			})
			tx := &orm.TxManager{
				DB: database, Registry: registry, Dialect: dialect, SiteName: site,
				Context: context.Background(), CurrentUser: "alice@example.test", CurrentUserRole: doctype.AdminRole,
				CurrentUserRoles: []string{doctype.AdminRole}, Outbox: outbox.NewSQLWriter(dialect),
			}

			first, err := executeAIRecordMutationWithKey(tx, registry, kernel.CommandRecordCreate, dt.Name, "", map[string]any{"title": "AI adapter"}, "alice@example.test", []string{doctype.AdminRole}, "ai-cross-dialect-"+suffix, false)
			if err != nil {
				t.Fatalf("AI create on %s: %v", tc.name, err)
			}
			replay, err := executeAIRecordMutationWithKey(tx, registry, kernel.CommandRecordCreate, dt.Name, "", map[string]any{"title": "AI adapter"}, "alice@example.test", []string{doctype.AdminRole}, "ai-cross-dialect-"+suffix, false)
			if err != nil {
				t.Fatalf("AI create replay on %s: %v", tc.name, err)
			}
			if first.Name == "" || replay.Name != first.Name {
				t.Fatalf("%s create/replay names = %q/%q, want same non-empty name", tc.name, first.Name, replay.Name)
			}
			for table, want := range map[string]int{"_kora_operation_audit": 1, "_kora_idempotency_receipt": 1, "_kora_outbox": 1} {
				var count int
				query := "SELECT COUNT(*) FROM " + dialect.QuoteIdent(table) + " WHERE site = ?"
				if err := database.QueryRow(kdb.Rebind(dialect, query), site).Scan(&count); err != nil {
					t.Fatalf("count %s rows on %s: %v", table, tc.name, err)
				}
				if count != want {
					t.Fatalf("%s rows on %s = %d, want %d", table, tc.name, count, want)
				}
			}
		})
	}
}
