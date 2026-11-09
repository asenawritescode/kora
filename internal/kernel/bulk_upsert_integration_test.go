//go:build integration

package kernel_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/configstore"
	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/kernel"
	"github.com/asenawritescode/kora/internal/outbox"
	"github.com/asenawritescode/kora/internal/policy"
	"github.com/asenawritescode/kora/internal/schema"
)

// TestBulkUpsert runs against disposable DBs: KORA_UPSERT_MYSQL_DSN (database
// in the DSN) and KORA_UPSERT_POSTGRES_DSN.
func TestBulkUpsert(t *testing.T) {
	for _, tc := range []struct{ name, env, dialect string }{
		{"mysql", "KORA_UPSERT_MYSQL_DSN", "mysql"}, {"postgres", "KORA_UPSERT_POSTGRES_DSN", "postgres"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dsn := os.Getenv(tc.env)
			if dsn == "" {
				t.Skipf("set %s", tc.env)
			}
			driver := tc.dialect
			conn, err := sql.Open(driver, dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			d := kdb.Resolve(tc.dialect)
			ddl := d.SystemTableSQL()
			if tc.dialect == "postgres" {
				ddl = append(ddl, kdb.OutboxTablesPostgres()...)
				ddl = append(ddl, kdb.KernelTablesPostgres()...)
			} else {
				ddl = append(ddl, kdb.OutboxTablesMySQL()...)
				ddl = append(ddl, kdb.KernelTablesMySQL()...)
			}
			for _, s := range append(ddl, configstore.ConfigRevisionSchema(d)...) {
				if strings.HasPrefix(strings.TrimSpace(s), "CREATE TABLE IF NOT EXISTS") {
					if _, err := conn.Exec(s); err != nil {
						t.Fatalf("ddl: %v", err)
					}
				}
			}
			if err := configstore.SeedConfigRevision(context.Background(), conn, d); err != nil {
				t.Fatal(err)
			}
			dt := &doctype.DocType{Name: "Supplier", Module: "T", Fields: []doctype.Field{
				{Fieldname: "code", Fieldtype: "Data", Label: "Code", Unique: true},
				{Fieldname: "label", Fieldtype: "Data", Label: "Label"},
				{Fieldname: "note", Fieldtype: "Data", Label: "Note"},
				{Fieldname: "grp", Fieldtype: "Data", Label: "Grp"}, // not unique: lets a test seed duplicates
			}}
			reg := doctype.NewRegistry()
			reg.LoadFull([]*doctype.DocType{dt}, []*doctype.Role{{Name: "Administrator"}, {Name: "Creator"}}, []*doctype.Permission{
				{Doctype: "Supplier", Role: "Administrator", Read: true, Write: true, Create: true},
				{Doctype: "Supplier", Role: "Creator", Read: true, Create: true},
			})
			if err := schema.MigrateSiteFromRegistry(conn, "", reg, d); err != nil {
				t.Fatalf("migrate: %v", err)
			}
			k := kernel.New(d, outbox.NewSQLWriter(d))
			run := func(role, id string, keys []string, dry bool, rows ...string) []kernel.BulkRowResult {
				exe, err := policy.NewVerifiedExecution("tenant-up", contract.ActorContext{PrincipalID: "u1", PrincipalType: contract.PrincipalHuman, Site: "s", Roles: []string{role}})
				if err != nil {
					t.Fatal(err)
				}
				base := kernel.Operation{Command: kernel.CommandRecordCreate, Context: kernel.OperationContext{Site: "s", TenantID: "tenant-up", Execution: exe, Source: kernel.SourceUI, User: "u1", UserRole: role, Roles: []string{role}}}
				var br []kernel.BulkRow
				for i, r := range rows {
					p, _ := json.Marshal(map[string]any{"doctype": "Supplier", "data": json.RawMessage(r)})
					br = append(br, kernel.BulkRow{Index: i + 1, Payload: p})
				}
				return k.ExecuteBulk(context.Background(), conn, reg, base, br, kernel.BulkOptions{ImportID: id, DryRun: dry, KeyFields: keys})
			}
			count := func() (n int) {
				_ = conn.QueryRow("SELECT COUNT(*) FROM " + d.QuoteIdent("tabSupplier")).Scan(&n)
				return
			}
			label := func(code string) (s string) {
				_ = conn.QueryRow(kdb.Rebind(d, "SELECT label FROM "+d.QuoteIdent("tabSupplier")+" WHERE code = ?"), code).Scan(&s)
				return
			}
			key := []string{"code"}
			okAll := func(rs []kernel.BulkRowResult) {
				t.Helper()
				for _, r := range rs {
					if r.Error != nil {
						t.Fatalf("row %d: %v", r.Index, r.Error)
					}
				}
			}
			var first []string
			for i := 0; i < 50; i++ {
				first = append(first, fmt.Sprintf(`{"code":"C%d","label":"v1","note":"keep"}`, i))
			}
			okAll(run("Administrator", "imp1", key, false, first...))
			if count() != 50 {
				t.Fatalf("created %d, want 50", count())
			}
			var second []string
			for i := 0; i < 50; i++ {
				second = append(second, fmt.Sprintf(`{"code":"C%d","label":"v2"}`, i))
			}
			okAll(run("Administrator", "imp2", key, false, second...))
			if count() != 50 || label("C7") != "v2" {
				t.Fatalf("existing keys must update: count=%d label=%q", count(), label("C7"))
			}
			var note string
			_ = conn.QueryRow(kdb.Rebind(d, "SELECT note FROM "+d.QuoteIdent("tabSupplier")+" WHERE code = ?"), "C7").Scan(&note)
			if note != "keep" {
				t.Fatalf("unmapped field changed: %q", note)
			}
			// replay: same import_id re-executes nothing, even though row 1 of imp1 would now be an update
			replay := run("Administrator", "imp1", key, false, first...)
			okAll(replay)
			if label("C7") != "v2" || count() != 50 {
				t.Fatal("replay re-executed rows")
			}
			// ambiguous: non-unique key matching two records
			for _, c := range []string{"a", "b"} {
				if _, err := conn.Exec(kdb.Rebind(d, "INSERT INTO "+d.QuoteIdent("tabSupplier")+" (name, code, grp, owner, creation, modified, modified_by, doc_status, idx) VALUES (?, ?, 'dup', 'u', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, 'u', 0, 0)"), "SUP-"+c, c); err != nil {
					t.Fatal(err)
				}
			}
			amb := run("Administrator", "imp3", []string{"grp"}, false, `{"grp":"dup","label":"x"}`, `{"grp":"solo","label":"y"}`)
			if amb[0].Error == nil || amb[0].Error.Type != contract.CodeConflict || !strings.Contains(amb[0].Error.Message, "import.key_ambiguous") || amb[1].Error != nil {
				t.Fatalf("ambiguous results: %+v %+v", amb[0].Error, amb[1].Error)
			}
			// dry run writes nothing
			before := count()
			okAll(run("Administrator", "imp4", key, true, `{"code":"NEW","label":"d"}`, `{"code":"C1","label":"dry"}`))
			if count() != before || label("C1") != "v2" {
				t.Fatal("dry run wrote")
			}
			// create-only role cannot update
			if rs := run("Creator", "imp5", key, false, `{"code":"C1","label":"nope"}`); rs[0].Error == nil || rs[0].Error.Type != contract.CodePermissionDenied || label("C1") != "v2" {
				t.Fatalf("missing write: %+v", rs[0].Error)
			}
		})
	}
}
