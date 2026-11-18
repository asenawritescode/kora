//go:build integration

package site

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shopspring/decimal"

	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/kernel"
	"github.com/asenawritescode/kora/internal/outbox"
	"github.com/asenawritescode/kora/internal/policy"
	"github.com/asenawritescode/kora/internal/query"
)

func TestPOSXReportAcrossDialects(t *testing.T) {
	for _, tc := range []struct{ driver, env string }{
		{"mysql", "KORA_POS_E2E_MYSQL_DSN"},
		{"postgres", "KORA_POS_E2E_POSTGRES_DSN"},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			dsn := os.Getenv(tc.env)
			if dsn == "" {
				t.Skipf("set %s", tc.env)
			}
			conn, name := posDB(t, tc.driver, dsn)
			runPOSXReport(t, tc.driver, conn, name)
		})
	}
}

// runPOSXReport runs one till session through paid, held, returned and voided
// sales plus cash in and out, then reads the X report queries. Held sales are
// listed apart and never counted as sales.
func runPOSXReport(t *testing.T, driver string, conn *sql.DB, dbName string) {
	ctx := context.Background()
	dialect := kdb.Resolve(driver)
	if err := BootstrapSystemTables(ctx, conn, dialect); err != nil {
		t.Fatal(err)
	}
	reg := doctype.NewRegistry()
	const siteName = "pos-xreport.example.test"
	if err := ImportConfig(conn, reg, dbName, siteName, filepath.Join("..", "..", "config", "pos"), dialect); err != nil {
		t.Fatal(err)
	}
	cmds := kernel.NewCommandRegistry()
	for _, pack := range []string{"inventory", "pos"} {
		defs, err := kernel.ParseCommandDir(filepath.Join("..", "..", "config", pack, "commands"))
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range defs {
			d.InputSchema = contract.SchemaRef{ID: "pos.xreport." + d.Name + ".in", Version: 1}
			d.OutputSchema = contract.SchemaRef{ID: "pos.xreport." + d.Name + ".out", Version: 1}
			if err := cmds.Register(d); err != nil {
				t.Fatal(err)
			}
		}
	}
	k := kernel.New(dialect, outbox.NewSQLWriter(dialect))
	k.Commands = cmds
	k.CommandSchemas = allowAllPOSSchemas{}
	actor := contract.ActorContext{PrincipalID: "Administrator", PrincipalType: contract.PrincipalHuman, Site: siteName, Roles: posAdminRoles, AuthenticatedAt: time.Now()}
	execution, err := policy.NewVerifiedExecution(contract.TenantID(siteName), actor)
	if err != nil {
		t.Fatal(err)
	}
	scope := contract.BusinessScope{TenantID: contract.TenantID(siteName), Kind: contract.ScopeKindTenant}
	exec := func(command string, payload any) (map[string]any, *contract.Error) {
		raw, _ := json.Marshal(payload)
		oc := kernel.OperationContext{
			Site: siteName, User: "Administrator", UserRole: doctype.AdminRole, Roles: posAdminRoles, Actor: actor,
			Execution: execution, TenantID: contract.TenantID(siteName), BusinessScope: scope, Source: kernel.SourceIntegration,
		}
		if strings.HasPrefix(command, "pos.") {
			me, merr := contract.NewCommandModuleExecution(scope, "pos.core", "pos.core", 1, command, command)
			if merr != nil {
				t.Fatal(merr)
			}
			oc.ModuleExecution = &me
		}
		res, cerr := k.Execute(ctx, conn, reg, kernel.Operation{Command: command, Payload: raw, Context: oc})
		var out map[string]any
		_ = json.Unmarshal(res.Data, &out)
		return out, cerr
	}
	must := func(command string, payload any) map[string]any {
		t.Helper()
		out, cerr := exec(command, payload)
		if cerr != nil {
			t.Fatalf("%s: %+v", command, cerr)
		}
		return out
	}
	table := func(n string) string { return dialect.QuoteIdent(reg.Get(n).RawTableName()) }
	str := func(q string, args ...any) string {
		t.Helper()
		var s sql.NullString
		if err := conn.QueryRow(kdb.Rebind(dialect, q), args...).Scan(&s); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return s.String
	}
	id := func(doc string, data map[string]any) string {
		raw, _ := json.Marshal(data)
		return fmt.Sprint(must(kernel.CommandRecordCreate, kernel.RecordCreatePayload{Doctype: doc, Data: raw})["name"])
	}
	loc := id("Location", map[string]any{"location_name": "Main"})
	register := id("Register", map[string]any{"register_name": "Front", "location": loc, "enabled": 1})
	tea := id("Item", map[string]any{"item_name": "Tea", "uom": "Nos", "price": 100})
	id("Stock Balance", map[string]any{"item": tea, "location": loc, "quantity": 50, "reorder_level": 1})
	opened := must("pos.open_session", map[string]any{"data": map[string]any{"register": register, "location": loc, "opening_float": 500}})
	shift := fmt.Sprint(opened["steps"].([]any)[0].(map[string]any)["name"])
	till := str("SELECT name FROM "+table("Till Session")+" WHERE shift = ?", shift)
	pay := posPayments(t, conn, dialect, siteName)
	items := func(qty int) []map[string]any {
		return []map[string]any{{"item": tea, "quantity": qty, "unit_price": 100, "amount": 100 * qty}}
	}
	sell := func(qty int, payments ...map[string]any) string {
		t.Helper()
		out, cerr := exec("pos.record_sale", map[string]any{"data": map[string]any{
			"register": register, "till_session": till, "location": loc, "items": items(qty), "currency": "KES", "payments": payments}})
		if cerr != nil {
			t.Fatalf("record_sale: %+v", cerr)
		}
		return fmt.Sprint(out["output"].(map[string]any)["reference"])
	}

	// Paid: 3 teas for cash (300) and 2 for card (200).
	cashSale := sell(3, pay("cash", 300))
	cardPay := pay("cash", 200)
	cardPay["method"] = "Card"
	sell(2, cardPay)

	// Held: one tea under a reserved number. It is not a sale.
	must("pos.reserve_numbers", map[string]any{"data": map[string]any{"till_session": till}})
	var prefix string
	var first int64
	if err := conn.QueryRow(kdb.Rebind(dialect, "SELECT prefix, first_number FROM _kora_number_block WHERE owner = ?"), till).Scan(&prefix, &first); err != nil {
		t.Fatal(err)
	}
	must("pos.hold_sale", map[string]any{"data": map[string]any{
		"register": register, "till_session": till, "location": loc, "sale_number": fmt.Sprintf("%s%05d", prefix, first),
		"held_name": "07:55 AM", "held_at": "2026-11-18 07:55:00", "held_by": "Administrator",
		"items": items(1)}})

	// Return: one tea of the cash sale, refunded in cash (100).
	must("pos.return_sale", map[string]any{"data": map[string]any{
		"register": register, "till_session": till, "location": loc, "return_against": cashSale, "items": items(1),
		"payments": []map[string]any{{"method": "Cash", "amount": 100, "currency": "KES"}}}})

	// Void: a cash sale of 100 voided in the same session.
	voided := sell(1, pay("cash", 100))
	must("pos.void_sale", map[string]any{"data": map[string]any{
		"shift": shift, "till_session": till, "register": register, "location": loc, "sale": voided}})

	// Cash in 50, cash out 30.
	for _, m := range []map[string]any{
		{"direction": "In", "reason": "Change from bank", "amount": 50},
		{"direction": "Out", "reason": "Transport", "amount": 30},
	} {
		m["till_session"] = till
		m["note"] = "integration"
		must("pos.record_cash_movement", map[string]any{"data": m})
	}

	// Read the queries as the Owner (full read) and as a Cashier who is not
	// this till's cashier (owner-scoped rows: none).
	def := func(name string) *query.Definition {
		t.Helper()
		defs, err := query.LoadDir(filepath.Join("..", "..", "config", "pos", "queries"), reg)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range defs {
			if d.Name == name {
				return d
			}
		}
		t.Fatalf("query %s missing", name)
		return nil
	}
	run := func(name string, req query.Request) []map[string]any {
		t.Helper()
		req.Input = map[string]any{"till_session": till}
		req.Now = time.Now()
		pg, err := query.Run(ctx, conn, dialect, def(name), req)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return pg.Rows
	}
	num := func(v any) decimal.Decimal {
		t.Helper()
		d, err := decimal.NewFromString(fmt.Sprint(v))
		if err != nil {
			t.Fatalf("not a number %v: %v", v, err)
		}
		return d
	}
	owner := query.Request{FullRead: true, Actor: "Administrator"}

	// x_report: one row per status. Paid is the sales figure; Held sits apart.
	byStatus := map[string]map[string]any{}
	for _, r := range run("x_report", owner) {
		byStatus[fmt.Sprint(r["status"])] = r
	}
	wantStatus := map[string][2]string{"Paid": {"2", "500"}, "Return": {"1", "100"}, "Void": {"1", "100"}, "Held": {"1", "100"}}
	if len(byStatus) != len(wantStatus) {
		t.Fatalf("x_report statuses = %v, want %v", byStatus, wantStatus)
	}
	for status, want := range wantStatus {
		row, ok := byStatus[status]
		if !ok {
			t.Fatalf("x_report has no %s row: %v", status, byStatus)
		}
		if !num(row["sale_count"]).Equal(decimal.RequireFromString(want[0])) || !num(row["total"]).Equal(decimal.RequireFromString(want[1])) {
			t.Fatalf("x_report %s = count %v total %v, want %s / %s", status, row["sale_count"], row["total"], want[0], want[1])
		}
		if fmt.Sprint(row["owner"]) != "Administrator" {
			t.Fatalf("x_report %s owner = %v", status, row["owner"])
		}
	}

	// x_report_session: the header. Start time is set on the row.
	header := run("x_report_session", owner)
	if len(header) != 1 || fmt.Sprint(header[0]["cashier"]) != "Administrator" || fmt.Sprint(header[0]["register"]) != register ||
		fmt.Sprint(header[0]["status"]) != "Open" || fmt.Sprint(header[0]["shift"]) != shift || fmt.Sprint(header[0]["creation"]) == "" {
		t.Fatalf("x_report_session = %v", header)
	}

	// expected_cash (reused, not duplicated): 500 + cash sale 300 - cash refund 100
	// + cash in 50 - cash out 30 = 720. The voided cash sale nets to zero.
	breakdown := run("expected_cash", owner)
	if len(breakdown) != 1 {
		t.Fatalf("expected_cash rows = %d", len(breakdown))
	}
	for field, want := range map[string]string{
		"opening_float": "500", "cash_sale_count": "1", "cash_sale_total": "300",
		"cash_refund_count": "1", "cash_refund_total": "100", "payout_count": "1",
		"payout_total": "30", "payin_count": "1", "payin_total": "50", "expected_cash": "720",
	} {
		if !num(breakdown[0][field]).Equal(decimal.RequireFromString(want)) {
			t.Fatalf("expected_cash.%s = %v, want %s", field, breakdown[0][field], want)
		}
	}

	// By method, only the methods used. Paid: Cash 300 and Card 200; refunds: Cash 100.
	methods := map[string]string{}
	for _, r := range run("payments_by_method", owner) {
		methods[fmt.Sprint(r["method"])] = fmt.Sprintf("%v/%v", r["payment_count"], num(r["total"]))
	}
	if methods["Cash"] != "1/300" || methods["Card"] != "1/200" || len(methods) != 2 {
		t.Fatalf("payments_by_method = %v", methods)
	}
	refunds := run("refunds_by_method", owner)
	if len(refunds) != 1 || fmt.Sprint(refunds[0]["method"]) != "Cash" || !num(refunds[0]["total"]).Equal(decimal.NewFromInt(100)) {
		t.Fatalf("refunds_by_method = %v", refunds)
	}

	// Owner-scoped access: a cashier who is not this till's owner sees no rows.
	other := query.Request{Actor: "cashier-2"}
	if rows := run("x_report", other); len(rows) != 0 {
		t.Fatalf("x_report leaked %d rows to another cashier", len(rows))
	}
	if rows := run("x_report_session", other); len(rows) != 0 {
		t.Fatalf("x_report_session leaked %d rows to another cashier", len(rows))
	}
}
