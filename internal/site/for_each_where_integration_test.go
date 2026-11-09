//go:build integration

package site

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
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

const whereInputCommand = `
name: till.cash_in
namespace: loop
version: 1
input:
  record: Till
transaction:
  - for_each: $input.payments
    where: {method: Cash}
    update:
      record: Till
      name: $input.till
      adjust:
        expected_cash: {increase: $item.amount}
`

const whereStoredCommand = `
name: till.cash_stored
namespace: loop
version: 1
input:
  record: Till
require:
  - exists:
      record: Pay Sale
      where:
        name: $input.sale
    as: original
transaction:
  - for_each: $steps.original.payments
    where: {method: Cash}
    update:
      record: Till
      name: $input.till
      adjust:
        expected_cash: {increase: $item.amount}
`

// TestForEachWhereAcrossDialects proves a filtered for_each adjusts the till
// only for matching input rows and matching stored child rows.
func TestForEachWhereAcrossDialects(t *testing.T) {
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
			runWhere(t, tc.driver, conn, name)
		})
	}
}

func runWhere(t *testing.T, driver string, conn *sql.DB, dbName string) {
	dialect := kdb.Resolve(driver)
	const siteName = "where.example.test"
	if err := BootstrapSystemTables(context.Background(), conn, dialect); err != nil {
		t.Fatal(err)
	}
	reg := doctype.NewRegistry()
	reg.LoadFull([]*doctype.DocType{
		{Name: "Pay Line", IsChildTable: true, Fields: []doctype.Field{
			{Fieldname: "method", Fieldtype: "Data"}, {Fieldname: "amount", Fieldtype: "Currency"},
		}},
		{Name: "Pay Sale", Fields: []doctype.Field{
			{Fieldname: "title", Fieldtype: "Data"}, {Fieldname: "payments", Fieldtype: "Table", Options: "Pay Line"},
		}},
		{Name: "Till", Fields: []doctype.Field{
			{Fieldname: "title", Fieldtype: "Data"}, {Fieldname: "expected_cash", Fieldtype: "Currency"},
		}},
	}, []*doctype.Role{{Name: doctype.AdminRole}}, []*doctype.Permission{
		{Doctype: "Pay Sale", Role: doctype.AdminRole, Read: true, Create: true, Write: true},
		{Doctype: "Till", Role: doctype.AdminRole, Read: true, Create: true, Write: true},
	})
	if err := schema.MigrateSiteFromRegistry(conn, dbName, reg, dialect); err != nil {
		t.Fatal(err)
	}
	k := kernel.New(dialect, outbox.NewSQLWriter(dialect))
	k.Commands = kernel.NewCommandRegistry()
	for _, c := range []string{whereInputCommand, whereStoredCommand} {
		def, err := kernel.ParseCommandResource([]byte(c))
		if err != nil {
			t.Fatal(err)
		}
		if err := k.Commands.Register(def); err != nil {
			t.Fatal(err)
		}
	}
	run := func(command string, payload any) map[string]any {
		actor := contract.ActorContext{PrincipalID: doctype.AdminRole, PrincipalType: contract.PrincipalHuman, Site: siteName, Roles: []string{doctype.AdminRole}, AuthenticatedAt: time.Now()}
		execution, err := policy.NewVerifiedExecution(contract.TenantID(siteName), actor)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(payload)
		res, cerr := k.Execute(context.Background(), conn, reg, kernel.Operation{Command: command, Payload: raw, Context: kernel.OperationContext{
			Site: siteName, User: doctype.AdminRole, Roles: []string{doctype.AdminRole}, Actor: actor, Execution: execution, TenantID: contract.TenantID(siteName), Source: kernel.SourceIntegration,
		}})
		if cerr != nil || res.Status != contract.StatusCompleted {
			t.Fatalf("%s: %+v %+v", command, res, cerr)
		}
		var out map[string]any
		_ = json.Unmarshal(res.Data, &out)
		return out
	}
	cash := func(till string) float64 {
		var v float64
		if err := conn.QueryRow(kdb.Rebind(dialect, `SELECT expected_cash FROM `+dialect.QuoteIdent("tabTill")+` WHERE name = ?`), till).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	rows := []any{
		map[string]any{"method": "Cash", "amount": 10.5}, map[string]any{"method": "Card", "amount": 100},
		map[string]any{"method": "Cash", "amount": 4},
	}
	till := run(kernel.CommandRecordCreate, kernel.RecordCreatePayload{Doctype: "Till", Data: json.RawMessage(`{"title":"t","expected_cash":0}`)})["name"].(string)
	run("loop.till.cash_in", map[string]any{"data": map[string]any{"till": till, "payments": rows}})
	if got := cash(till); got != 14.5 {
		t.Fatalf("input where: expected_cash = %v, want 14.5 (Cash rows only)", got)
	}
	run("loop.till.cash_in", map[string]any{"data": map[string]any{"till": till, "payments": []any{rows[1]}}})
	if got := cash(till); got != 14.5 {
		t.Fatalf("no match must not change cash, got %v", got)
	}
	data, _ := json.Marshal(map[string]any{"title": "s", "payments": rows})
	sale := run(kernel.CommandRecordCreate, kernel.RecordCreatePayload{Doctype: "Pay Sale", Data: data})["name"].(string)
	run("loop.till.cash_stored", map[string]any{"data": map[string]any{"till": till, "sale": sale}})
	if got := cash(till); got != 29 {
		t.Fatalf("stored where: expected_cash = %v, want 29", got)
	}
}
