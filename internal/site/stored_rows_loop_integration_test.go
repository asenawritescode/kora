//go:build integration

package site

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/kernel"
	"github.com/asenawritescode/kora/internal/outbox"
	"github.com/asenawritescode/kora/internal/policy"
	"github.com/asenawritescode/kora/internal/schema"
)

const storedRowsCommand = `
name: sale.restock
namespace: loop
version: 1
input:
  record: Sale
require:
  - exists:
      record: Sale
      where:
        name: $input.sale
    as: original
transaction:
  - for_each: $steps.original.items
    create:
      record: Restock
      values:
        sale: $steps.original.name
        item: $item.item
        qty: $item.qty
        note: $item.note
`

// TestForEachStoredRowsAcrossDialects proves for_each over a guarded record's
// stored child rows writes one step per row inside the command transaction.
func TestForEachStoredRowsAcrossDialects(t *testing.T) {
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
			runStoredRows(t, tc.driver, conn, name)
		})
	}
}

func runStoredRows(t *testing.T, driver string, conn *sql.DB, dbName string) {
	dialect := kdb.Resolve(driver)
	const siteName = "loop.example.test"
	if err := BootstrapSystemTables(context.Background(), conn, dialect); err != nil {
		t.Fatal(err)
	}
	reg := doctype.NewRegistry()
	reg.LoadFull([]*doctype.DocType{
		{Name: "Sale Line", IsChildTable: true, Fields: []doctype.Field{
			{Fieldname: "item", Fieldtype: "Data"}, {Fieldname: "qty", Fieldtype: "Int"}, {Fieldname: "note", Fieldtype: "Data"},
		}},
		{Name: "Sale", Fields: []doctype.Field{
			{Fieldname: "title", Fieldtype: "Data"}, {Fieldname: "items", Fieldtype: "Table", Options: "Sale Line"},
		}},
		{Name: "Restock", Fields: []doctype.Field{
			{Fieldname: "sale", Fieldtype: "Data"}, {Fieldname: "item", Fieldtype: "Data"}, {Fieldname: "qty", Fieldtype: "Int"}, {Fieldname: "note", Fieldtype: "Data"},
		}},
	}, []*doctype.Role{{Name: doctype.AdminRole}, {Name: "Clerk"}}, []*doctype.Permission{
		{Doctype: "Sale", Role: doctype.AdminRole, Read: true, Create: true, Write: true},
		{Doctype: "Restock", Role: doctype.AdminRole, Read: true, Create: true, Write: true},
		{Doctype: "Restock", Role: "Clerk", Read: true, Create: true},
	})
	if err := schema.MigrateSiteFromRegistry(conn, dbName, reg, dialect); err != nil {
		t.Fatal(err)
	}
	k := kernel.New(dialect, outbox.NewSQLWriter(dialect))
	k.Commands = kernel.NewCommandRegistry()
	for _, c := range []string{storedRowsCommand, strings.NewReplacer("sale.restock", "sale.badfield", "original.items", "original.title").Replace(storedRowsCommand)} {
		def, err := kernel.ParseCommandResource([]byte(c))
		if err != nil {
			t.Fatal(err)
		}
		if err := k.Commands.Register(def); err != nil {
			t.Fatal(err)
		}
	}
	run := func(role, command string, payload any) (contract.CommandResult, *contract.Error) {
		actor := contract.ActorContext{PrincipalID: role, PrincipalType: contract.PrincipalHuman, Site: siteName, Roles: []string{role}, AuthenticatedAt: time.Now()}
		execution, err := policy.NewVerifiedExecution(contract.TenantID(siteName), actor)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(payload)
		return k.Execute(context.Background(), conn, reg, kernel.Operation{Command: command, Payload: raw, Context: kernel.OperationContext{
			Site: siteName, User: role, Roles: []string{role}, Actor: actor, Execution: execution, TenantID: contract.TenantID(siteName), Source: kernel.SourceIntegration,
		}})
	}
	scalar := func(q string, args ...any) (n int) {
		if err := conn.QueryRow(kdb.Rebind(dialect, q), args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	createSale := func(items []any) string {
		data, _ := json.Marshal(map[string]any{"title": "s", "items": items})
		res, cerr := run(doctype.AdminRole, kernel.CommandRecordCreate, kernel.RecordCreatePayload{Doctype: "Sale", Data: data})
		if cerr != nil || res.Status != contract.StatusCompleted {
			t.Fatalf("create sale: %+v %+v", res, cerr)
		}
		var out map[string]any
		_ = json.Unmarshal(res.Data, &out)
		return out["name"].(string)
	}
	count := func(sale string) int {
		return scalar(`SELECT COUNT(*) FROM `+dialect.QuoteIdent("tabRestock")+` WHERE sale = ?`, sale)
	}
	sale := createSale([]any{map[string]any{"item": "A", "qty": 2, "note": "n"}, map[string]any{"item": "B", "qty": 5}, map[string]any{"item": "C", "qty": 1}})
	res, cerr := run(doctype.AdminRole, "loop.sale.restock", map[string]any{"data": map[string]any{"sale": sale, "items": []any{map[string]any{"item": "X"}}}})
	if cerr != nil || res.Status != contract.StatusCompleted {
		t.Fatalf("restock: %+v %+v", res, cerr)
	}
	if n := count(sale); n != 3 {
		t.Fatalf("restock rows = %d, want one per stored row (3)", n)
	}
	if n := scalar(`SELECT COUNT(*) FROM `+dialect.QuoteIdent("tabRestock")+` WHERE sale = ? AND note IS NULL`, sale); n != 2 {
		t.Fatalf("null notes = %d, want 2", n)
	}
	// A caller without read on the guard record is denied and nothing is written.
	denied := createSale([]any{map[string]any{"item": "D", "qty": 1}})
	if res, cerr = run("Clerk", "loop.sale.restock", map[string]any{"data": map[string]any{"sale": denied}}); res.Status == contract.StatusCompleted || count(denied) != 0 {
		t.Fatalf("clerk without read: %+v %+v rows=%d", res, cerr, count(denied))
	}
	// A non-Table field fails validation before any write.
	if res, cerr = run(doctype.AdminRole, "loop.sale.badfield", map[string]any{"data": map[string]any{"sale": denied}}); res.Status == contract.StatusCompleted || count(denied) != 0 {
		t.Fatalf("non-table field: %+v %+v", res, cerr)
	}
}
