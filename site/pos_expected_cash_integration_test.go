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

	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/kernel"
	"github.com/asenawritescode/kora/outbox"
	"github.com/asenawritescode/kora/policy"
	"github.com/asenawritescode/kora/print"
	"github.com/asenawritescode/kora/query"
)

func TestPOSExpectedCashAcrossDialects(t *testing.T) {
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
			runPOSExpectedCash(t, tc.driver, conn, name)
		})
	}
}

func runPOSExpectedCash(t *testing.T, driver string, conn *sql.DB, dbName string) {
	ctx := context.Background()
	dialect := kdb.Resolve(driver)
	if err := BootstrapSystemTables(conn, dialect); err != nil {
		t.Fatal(err)
	}
	reg := doctype.NewRegistry()
	const siteName = "pos-cash.example.test"
	if err := ImportConfig(conn, reg, dbName, siteName, filepath.Join("..", "config", "pos"), dialect); err != nil {
		t.Fatal(err)
	}
	cmds := kernel.NewCommandRegistry()
	for _, pack := range []string{"inventory", "pos"} {
		defs, err := kernel.ParseCommandDir(filepath.Join("..", "config", pack, "commands"))
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range defs {
			d.InputSchema = contract.SchemaRef{ID: "pos.cash." + d.Name + ".in", Version: 1}
			d.OutputSchema = contract.SchemaRef{ID: "pos.cash." + d.Name + ".out", Version: 1}
			if err := cmds.Register(d); err != nil {
				t.Fatal(err)
			}
		}
	}
	k := kernel.New(dialect, outbox.NewSQLWriter(dialect))
	k.Commands = cmds
	k.CommandSchemas = allowAllPOSSchemas{}
	actor := contract.ActorContext{PrincipalID: "Administrator", PrincipalType: contract.PrincipalHuman, Site: siteName, Roles: []string{doctype.AdminRole}, AuthenticatedAt: time.Now()}
	execution, err := policy.NewVerifiedExecution(contract.TenantID(siteName), actor)
	if err != nil {
		t.Fatal(err)
	}
	scope := contract.BusinessScope{TenantID: contract.TenantID(siteName), Kind: contract.ScopeKindTenant}
	exec := func(command string, payload any) (map[string]any, *contract.Error) {
		raw, _ := json.Marshal(payload)
		oc := kernel.OperationContext{
			Site: siteName, User: "Administrator", UserRole: doctype.AdminRole, Roles: []string{doctype.AdminRole}, Actor: actor,
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
	str := func(query string, args ...any) string {
		t.Helper()
		var s sql.NullString
		if err := conn.QueryRow(kdb.Rebind(dialect, query), args...).Scan(&s); err != nil {
			t.Fatalf("%s: %v", query, err)
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
	shift := fmt.Sprint(must("pos.open_shift", map[string]any{"data": map[string]any{"register": register}})["steps"].([]any)[0].(map[string]any)["name"])
	must("pos.open_till", map[string]any{"data": map[string]any{"shift": shift, "opening_float": 500, "expected_cash": 9999}})
	till := str("SELECT name FROM "+table("Till Session")+" WHERE shift = ?", shift)
	pay := posPayments(t, conn, dialect, siteName)
	expected := func() float64 {
		var f float64
		fmt.Sscan(str("SELECT expected_cash FROM "+table("Till Session")+" WHERE name = ?", till), &f)
		return f
	}
	want := func(label string, v float64) {
		t.Helper()
		if got := expected(); got != v {
			t.Fatalf("%s: expected_cash = %v, want %v", label, got, v)
		}
	}
	want("open (sent expected_cash ignored)", 500)
	items := func(qty int) []map[string]any {
		return []map[string]any{{"item": tea, "quantity": qty, "unit_price": 100, "amount": 100 * qty}}
	}
	sell := func(qty int, payments ...map[string]any) (map[string]any, *contract.Error) {
		return exec("pos.record_sale", map[string]any{"data": map[string]any{
			"register": register, "till_session": till, "location": loc, "items": items(qty), "currency": "KES", "payments": payments}})
	}
	ref := func(out map[string]any) string { return fmt.Sprint(out["output"].(map[string]any)["reference"]) }
	out, cerr := sell(3, pay("cash", 300))
	if cerr != nil {
		t.Fatal(cerr)
	}
	sale := ref(out)
	// Walk-in sale (no customer): the link-picker title must be non-empty.
	if tf := reg.Get("Sale").TitleField; tf != "register" || str("SELECT "+dialect.QuoteIdent(tf)+" FROM "+table("Sale")+" WHERE name = ?", sale) == "" {
		t.Fatalf("walk-in sale title field %q blank", tf)
	}
	if _, cerr = sell(2, func() map[string]any { p := pay("cash", 200); p["method"] = "Card"; return p }()); cerr != nil {
		t.Fatal(cerr)
	}
	want("cash + card sale", 800)
	ret := func(against string, qty int) *contract.Error {
		_, cerr := exec("pos.return_sale", map[string]any{"data": map[string]any{
			"register": register, "till_session": till, "location": loc, "return_against": against, "items": items(qty),
			"payments": []map[string]any{{"method": "Cash", "amount": 100 * qty, "reference": "refund"}}}})
		return cerr
	}
	if cerr = ret(sale, 1); cerr != nil {
		t.Fatal(cerr)
	}
	want("cash return of 100", 700)
	out, cerr = sell(1, pay("cash", 100))
	if cerr != nil {
		t.Fatal(cerr)
	}
	void := func(s string) *contract.Error {
		_, cerr := exec("pos.void_sale", map[string]any{"data": map[string]any{
			"shift": shift, "till_session": till, "register": register, "location": loc, "sale": s}})
		return cerr
	}
	want("cash sale of 100", 800)
	if cerr = void(ref(out)); cerr != nil {
		t.Fatal(cerr)
	}
	want("void cash sale", 700)

	// Close: count 690 against expected 700 -> variance -10; reference is the till name.
	closeOut := must("pos.close_till", map[string]any{"data": map[string]any{"shift": shift, "closing_count": 690, "expected_cash": 1}})
	if got := fmt.Sprint(closeOut["output"].(map[string]any)["reference"]); got != till {
		t.Fatalf("close reference = %q, want %q", got, till)
	}
	if v := str("SELECT variance FROM "+table("Till Session")+" WHERE name = ?", till); v != "-10" && !strings.HasPrefix(v, "-10.0") {
		t.Fatalf("variance = %q, want -10", v)
	}
	want("close keeps expected", 700)
	if s := str("SELECT status FROM "+table("Till Session")+" WHERE name = ?", till); s != "Closed" {
		t.Fatalf("status = %q", s)
	}
	counts := func() string {
		return str("SELECT COUNT(*) FROM "+table("Sale")) + "/" + str("SELECT COUNT(*) FROM "+table("Stock Entry"))
	}
	before := counts()
	if _, cerr = exec("pos.close_till", map[string]any{"data": map[string]any{"shift": shift, "closing_count": 1}}); cerr == nil {
		t.Fatal("second close_till accepted")
	}
	if _, cerr = sell(1, pay("cash", 100)); cerr == nil {
		t.Fatal("sale on closed till accepted")
	}
	if cerr = ret(sale, 1); cerr == nil {
		t.Fatal("return on closed till accepted")
	}
	if cerr = void(sale); cerr == nil {
		t.Fatal("void on closed till accepted")
	}
	if counts() != before {
		t.Fatalf("refused commands wrote data: %s -> %s", before, counts())
	}
	want("after refusals", 700)

	// Z report: run the format's published queries on this database and render it.
	qdefs, err := query.LoadDir(filepath.Join("..", "config", "pos", "queries"), reg)
	if err != nil {
		t.Fatal(err)
	}
	rows := func(name string) []map[string]any {
		t.Helper()
		for _, d := range qdefs {
			if d.Name == name {
				pg, err := query.Run(ctx, conn, dialect, d, query.Request{FullRead: true, Input: map[string]any{"till_session": till}, Actor: "Administrator", Now: time.Now()})
				if err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				return pg.Rows
			}
		}
		t.Fatalf("query %s missing", name)
		return nil
	}
	var rendered []string
	for _, file := range []string{"z_report.yaml", "z_report_reprint.yaml"} {
		body, err := os.ReadFile(filepath.Join("..", "config", "pos", "print_formats", file))
		if err != nil {
			t.Fatal(err)
		}
		f, err := print.Load(body)
		if err != nil {
			t.Fatal(err)
		}
		data := print.Data{Doc: map[string]any{"name": till}, Tenant: map[string]any{"name": "Shop"}, Now: time.Unix(0, 0).UTC(), Queries: map[string][]map[string]any{}}
		for _, col := range []string{"opening_float", "expected_cash", "closing_count", "variance"} {
			var v float64
			fmt.Sscan(str("SELECT "+col+" FROM "+table("Till Session")+" WHERE name = ?", till), &v)
			data.Doc[col] = v
		}
		for key, spec := range f.Queries {
			_, qn, _ := spec.Ref()
			data.Queries[key] = rows(qn)
		}
		txt, err := f.RenderText(data, print.Options{Currency: "KES", Precision: 2})
		if err != nil {
			t.Fatal(err)
		}
		rendered = append(rendered, txt)
	}
	if rendered[0] != rendered[1] {
		t.Fatal("reprint differs from operation print")
	}
	// Voided sale is excluded; lines are method, count, total.
	flat := strings.Join(strings.Fields(strings.ReplaceAll(rendered[0], "-", "")), " ")
	for _, w := range []string{"SALES Card 1 KES 200.00 Cash 1 KES 300.00 REFUNDS Cash 1 KES 100.00", "Opening float KES 500.00 Expected cash KES 700.00 Counted KES 690.00 VARIANCE KES 10.00"} {
		if !strings.Contains(flat, w) {
			t.Fatalf("z report missing %q:\n%s", w, rendered[0])
		}
	}
	if strings.Contains(rendered[0], "KES 400.00") {
		t.Fatalf("void leaked into totals:\n%s", rendered[0])
	}
}
