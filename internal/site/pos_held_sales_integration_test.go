//go:build integration

package site

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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

// TestPOSHeldSalesAcrossDialects holds sales under reserved numbers: replays are
// safe, a till holds 20 at most, delete voids a held sale, and held sales take no
// stock, no cash and no place in the sales lists.
func TestPOSHeldSalesAcrossDialects(t *testing.T) {
	for _, tc := range []struct{ driver, env string }{
		{"mysql", "KORA_SITE_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "KORA_SITE_DISPOSABLE_POSTGRES_DSN"},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			dsn := os.Getenv(tc.env)
			if dsn == "" {
				t.Skipf("set %s", tc.env)
			}
			conn, name := posDB(t, tc.driver, dsn)
			runPOSHeldSales(t, tc.driver, conn, name)
		})
	}
}

func runPOSHeldSales(t *testing.T, driver string, conn *sql.DB, dbName string) {
	ctx := context.Background()
	dialect := kdb.Resolve(driver)
	if err := BootstrapSystemTables(ctx, conn, dialect); err != nil {
		t.Fatal(err)
	}
	reg := doctype.NewRegistry()
	const siteName = "pos-held.example.test"
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
			d.InputSchema = contract.SchemaRef{ID: "pos.held." + d.Name + ".in", Version: 1}
			d.OutputSchema = contract.SchemaRef{ID: "pos.held." + d.Name + ".out", Version: 1}
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
	// exec runs one POS command; key, when set, is the device's idempotency key.
	exec := func(command, key string, payload any) (map[string]any, *contract.Error) {
		raw, _ := json.Marshal(payload)
		oc := kernel.OperationContext{
			Site: siteName, User: "Administrator", UserRole: doctype.AdminRole, Roles: posAdminRoles, Actor: actor,
			Execution: execution, TenantID: contract.TenantID(siteName), BusinessScope: scope, Source: kernel.SourceIntegration,
			IdempotencyKey: key,
		}
		me, merr := contract.NewCommandModuleExecution(scope, "pos.core", "pos.core", 1, command, command)
		if merr != nil {
			t.Fatal(merr)
		}
		oc.ModuleExecution = &me
		res, cerr := k.Execute(ctx, conn, reg, kernel.Operation{Command: command, Payload: raw, Context: oc})
		var out map[string]any
		_ = json.Unmarshal(res.Data, &out)
		return out, cerr
	}
	must := func(command string, payload any) map[string]any {
		t.Helper()
		out, cerr := exec(command, "", payload)
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
	must("pos.reserve_numbers", map[string]any{"data": map[string]any{"till_session": till}})
	// The block the till reserved: number n is first+n under its prefix.
	var prefix string
	var first int64
	if err := conn.QueryRow(kdb.Rebind(dialect, "SELECT prefix, first_number FROM _kora_number_block WHERE owner = ?"), till).Scan(&prefix, &first); err != nil {
		t.Fatal(err)
	}
	number := func(n int) string { return fmt.Sprintf("%s%05d", prefix, first+int64(n)) }
	hold := func(key string, n int) *contract.Error {
		_, cerr := exec("pos.hold_sale", key, map[string]any{"data": map[string]any{
			"register": register, "till_session": till, "location": loc, "sale_number": number(n),
			"held_name": "07:55 AM", "held_at": "2026-11-18 07:55:00", "held_by": "Joseph K.",
			"items": []map[string]any{{"item": tea, "quantity": 1, "unit_price": 100, "amount": 100}},
		}})
		return cerr
	}
	// money reads a stored amount as a plain decimal, whatever the dialect's scale.
	money := func(q string, args ...any) string {
		d, err := decimal.NewFromString(str(q, args...))
		if err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return d.String()
	}
	stock := func() string {
		return money("SELECT quantity FROM "+table("Stock Balance")+" WHERE item = ? AND location = ?", tea, loc)
	}
	expected := func() string {
		return money("SELECT expected_cash FROM "+table("Till Session")+" WHERE name = ?", till)
	}
	status := func(n int) string {
		return str("SELECT status FROM "+table("Sale")+" WHERE name = ?", number(n))
	}
	cashRow := func(q string) map[string]any {
		t.Helper()
		defs, err := query.LoadDir(filepath.Join("..", "..", "config", "pos", "queries"), reg)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range defs {
			if d.Name == q {
				pg, err := query.Run(ctx, conn, dialect, d, query.Request{FullRead: true, Input: map[string]any{"till_session": till, "register": register}, Actor: "Administrator", Now: time.Now()})
				if err != nil {
					t.Fatalf("%s: %v", q, err)
				}
				return map[string]any{"rows": pg.Rows}
			}
		}
		t.Fatalf("query %s missing", q)
		return nil
	}

	// Hold 0: a held sale takes no stock, no cash, and is Held.
	if cerr := hold("key-0", 0); cerr != nil {
		t.Fatalf("hold: %+v", cerr)
	}
	if status(0) != "Held" || stock() != "50" || expected() != "500" {
		t.Fatalf("after hold: status=%s stock=%s expected=%s", status(0), stock(), expected())
	}
	// Replay of the same device intent is safe: same result, one row.
	if cerr := hold("key-0", 0); cerr != nil {
		t.Fatalf("replay of the same intent: %+v", cerr)
	}
	if n := str("SELECT COUNT(*) FROM "+table("Sale")+" WHERE name = ?", number(0)); n != "1" {
		t.Fatalf("replay wrote %s rows", n)
	}
	// Holding the same number again with a new intent is an edit of that held sale.
	if cerr := hold("key-edit", 0); cerr != nil {
		t.Fatalf("re-hold of a held sale: %+v", cerr)
	}
	if n := str("SELECT COUNT(*) FROM "+table("Sale")+" WHERE name = ?", number(0)); n != "1" {
		t.Fatalf("re-hold wrote %s rows", n)
	}
	// The limit: twenty held on this till, the twenty-first is refused.
	for n := 1; n < 20; n++ {
		if cerr := hold(fmt.Sprintf("key-%d", n), n); cerr != nil {
			t.Fatalf("hold %d: %+v", n, cerr)
		}
	}
	// Exactly twenty held: editing one of them is allowed, a twenty-first is not.
	if cerr := hold("key-edit-20", 0); cerr != nil {
		t.Fatalf("edit at exactly twenty: %+v", cerr)
	}
	if cerr := hold("key-20", 20); cerr == nil || cerr.Type != contract.CodeConflict {
		t.Fatalf("twenty-first held sale = %+v, want a conflict", cerr)
	}
	if rows := cashRow("held_sales")["rows"].([]map[string]any); len(rows) != 20 {
		t.Fatalf("held_sales lists %d, want 20", len(rows))
	}
	// Delete voids one held sale; a second delete is refused; the till can hold again.
	deleteSale := func(n int) *contract.Error {
		_, cerr := exec("pos.delete_held_sale", "", map[string]any{"data": map[string]any{"sale": number(n), "till_session": till}})
		return cerr
	}
	if cerr := deleteSale(0); cerr != nil {
		t.Fatalf("delete: %+v", cerr)
	}
	if status(0) != "Void" {
		t.Fatalf("deleted sale status = %s, want Void", status(0))
	}
	if cerr := deleteSale(0); cerr == nil {
		t.Fatal("a voided sale was deleted again")
	}
	if cerr := hold("key-20", 20); cerr != nil {
		t.Fatalf("hold after delete: %+v", cerr)
	}
	// Held to Paid: recording a held sale's number pays it with the items it is given now.
	bread := id("Item", map[string]any{"item_name": "Bread", "uom": "Nos", "price": 50})
	id("Stock Balance", map[string]any{"item": bread, "location": loc, "quantity": 10, "reorder_level": 1})
	payCash := posPayments(t, conn, dialect, siteName)
	pay := func(n int, key string) *contract.Error {
		_, cerr := exec("pos.record_sale", key, map[string]any{"data": map[string]any{
			"register": register, "till_session": till, "location": loc, "sale_number": number(n), "currency": "KES",
			"items":    []map[string]any{{"item": bread, "quantity": 2, "unit_price": 50, "amount": 100}},
			"payments": []map[string]any{payCash("cash", 100)},
		}})
		return cerr
	}
	if cerr := pay(1, "pay-1"); cerr != nil {
		t.Fatalf("pay held sale: %+v", cerr)
	}
	if status(1) != "Paid" {
		t.Fatalf("paid held sale status = %s", status(1))
	}
	// Facts: Held rows get no facts, and the paid sale is not excluded, so it gets its facts.
	if fx := reg.Get("Sale").Facts; fx == nil || !slices.Contains(fx.ExcludeStatus, "Held") || slices.Contains(fx.ExcludeStatus, "Paid") {
		t.Fatalf("Sale facts exclude %v, want Held excluded and Paid kept", reg.Get("Sale").Facts)
	}
	if n := str("SELECT COUNT(*) FROM "+dialect.QuoteIdent("tabSale__items")+" WHERE parent = ? AND item = ?", number(1), tea); n != "0" {
		t.Fatalf("the held item survived the payment: %s rows", n)
	}
	if n := str("SELECT COUNT(*) FROM "+dialect.QuoteIdent("tabSale__items")+" WHERE parent = ? AND item = ?", number(1), bread); n != "1" {
		t.Fatalf("paid sale has %s bread lines, want 1", n)
	}
	if got := money("SELECT quantity FROM "+table("Stock Balance")+" WHERE item = ? AND location = ?", bread, loc); got != "8" {
		t.Fatalf("bread stock after the sale = %s, want 8", got)
	}
	if got := money("SELECT quantity FROM "+table("Stock Balance")+" WHERE item = ? AND location = ?", tea, loc); got != "50" {
		t.Fatalf("paying moved tea stock to %s", got)
	}
	if expected() != "600" {
		t.Fatalf("expected cash after the cash payment = %s, want 600", expected())
	}
	if cerr := pay(1, "pay-again"); cerr == nil {
		t.Fatal("a paid sale was paid again")
	}
	if cerr := hold("key-after-pay", 1); cerr == nil {
		t.Fatal("a paid sale was held again")
	}

	// Held sales sit outside the sales lists and cash.
	if rows := cashRow("held_sales")["rows"].([]map[string]any); len(rows) != 19 {
		t.Fatalf("held_sales after the payment lists %d, want 19", len(rows))
	}
	if stock() != "50" {
		t.Fatalf("held sales moved tea stock: %s", stock())
	}
	// The one voided row is the deleted held sale; no held sale is listed.
	for _, row := range cashRow("recent_sales")["rows"].([]map[string]any) {
		if row["status"] == "Held" {
			t.Fatalf("recent_sales lists a held sale: %v", row)
		}
	}
	// Totals count the one paid sale (100), never the held ones.
	if rows := cashRow("today_totals")["rows"].([]map[string]any); len(rows) != 1 || fmt.Sprint(rows[0]["sale_count"]) != "1" {
		t.Fatalf("today_totals = %v, want the one paid sale", rows)
	}
}
