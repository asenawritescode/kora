//go:build integration

package site

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"github.com/shopspring/decimal"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/lib/pq"

	"github.com/asenawritescode/kora/configstore"
	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/kernel"
	"github.com/asenawritescode/kora/outbox"
	"github.com/asenawritescode/kora/payment"
	"github.com/asenawritescode/kora/policy"
	"github.com/asenawritescode/kora/print"
	"github.com/asenawritescode/kora/query"
)

// posDB returns an isolated database (MySQL) or schema (Postgres) for one run.
func posDB(t *testing.T, driver, dsn string) (*sql.DB, string) {
	t.Helper()
	admin, err := sql.Open(driver, dsn)
	if err != nil || admin.Ping() != nil {
		t.Fatalf("connect %s: %v", driver, err)
	}
	name := fmt.Sprintf("kora_pos_e2e_%x", time.Now().UnixNano())
	target := dsn
	if driver == "postgres" {
		if _, err := admin.Exec(`CREATE SCHEMA "` + name + `"`); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { admin.Exec(`DROP SCHEMA "` + name + `" CASCADE`); admin.Close() })
		u, _ := url.Parse(dsn)
		q := u.Query()
		q.Set("search_path", name)
		u.RawQuery = q.Encode()
		target = u.String()
	} else {
		if _, err := admin.Exec("CREATE DATABASE `" + name + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci"); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { admin.Exec("DROP DATABASE `" + name + "`"); admin.Close() })
		target = strings.Replace(dsn, "/?", "/"+name+"?", 1)
	}
	conn, err := sql.Open(driver, target)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn, name
}

type fixedProvider struct {
	name  string
	state payment.State
}

func (f fixedProvider) Name() string { return f.name }
func (f fixedProvider) Request(_ context.Context, in payment.Intent) (payment.Outcome, error) {
	return payment.Outcome{State: f.state, ProviderRequestID: in.ID}, nil
}

// posPayments returns a factory that requests an intent through the payment
// service and returns the Sale Payment row that references it.
func posPayments(t *testing.T, conn *sql.DB, dialect kdb.Dialect, site string) func(provider string, amount float64) map[string]any {
	t.Helper()
	reg := payment.NewRegistry()
	for _, p := range []payment.Provider{fixedProvider{"slow", payment.StatePending}, fixedProvider{"broken", payment.StateFailed}} {
		if err := reg.Register(p); err != nil {
			t.Fatal(err)
		}
	}
	svc := &payment.Service{Store: &payment.SQLStore{DB: conn, Dialect: dialect}, Providers: reg}
	n := 0
	return func(provider string, amount float64) map[string]any {
		t.Helper()
		n++
		in, err := svc.Request(context.Background(), site, "Administrator", payment.RequestInput{
			Provider: provider, Amount: decimal.NewFromFloat(amount), Currency: "KES", SourceDoctype: "Sale",
			IdempotencyKey: fmt.Sprintf("pos-%d", n),
		})
		if err != nil {
			t.Fatal(err)
		}
		return map[string]any{"method": "Cash", "amount": amount, "currency": "KES", "reference": in.ID}
	}
}

func TestPOSPackEndToEndAcrossDialects(t *testing.T) {
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
			runPOSEndToEnd(t, tc.driver, conn, name)
		})
	}
}

func runPOSEndToEnd(t *testing.T, driver string, conn *sql.DB, dbName string) {
	ctx := context.Background()
	dialect := kdb.Resolve(driver)
	if err := BootstrapSystemTables(conn, dialect); err != nil {
		t.Fatal(err)
	}
	// Installing the pos pack pulls in inventory through pack.yaml requires.
	reg := doctype.NewRegistry()
	const siteName = "pos-e2e.example.test"
	if err := ImportConfig(conn, reg, dbName, siteName, filepath.Join("..", "config", "pos"), dialect); err != nil {
		t.Fatalf("install pos+inventory: %v", err)
	}
	// Stored print formats carry published operation refs, not authored names.
	snap, _, err := configstore.NewStore(conn, dialect).LoadActiveSnapshot(siteName)
	if err != nil {
		t.Fatal(err)
	}
	refs := map[string]string{}
	for _, raw := range snap.PrintFormats {
		f, _ := print.Decode(raw)
		refs[f.Name] = f.For.Operation
	}
	if refs["sale_receipt"] != "pos.core~pos.checkout~record_sale" || refs["z_report"] != "pos.core~pos.drawer~close_till" {
		t.Fatalf("stored print operation refs: %v", refs)
	}
	cmds := kernel.NewCommandRegistry()
	for _, pack := range []string{"inventory", "pos"} {
		defs, err := kernel.ParseCommandDir(filepath.Join("..", "config", pack, "commands"))
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range defs {
			if err := cmds.Register(d); err != nil {
				t.Fatal(err)
			}
		}
	}
	k := kernel.New(dialect, outbox.NewSQLWriter(dialect))
	k.Commands = cmds

	actor := contract.ActorContext{PrincipalID: "Administrator", PrincipalType: contract.PrincipalHuman, Site: siteName, Roles: []string{doctype.AdminRole}, AuthenticatedAt: time.Now()}
	execution, err := policy.NewVerifiedExecution(contract.TenantID(siteName), actor)
	if err != nil {
		t.Fatal(err)
	}
	run := func(command string, payload any) map[string]any {
		t.Helper()
		raw, _ := json.Marshal(payload)
		res, cerr := k.Execute(ctx, conn, reg, kernel.Operation{Command: command, Payload: raw, Context: kernel.OperationContext{
			Site: siteName, User: "Administrator", Roles: []string{doctype.AdminRole}, Actor: actor,
			Execution: execution, TenantID: contract.TenantID(siteName), Source: kernel.SourceIntegration,
		}})
		if cerr != nil || res.Status != contract.StatusCompleted {
			t.Fatalf("%s: status=%s err=%+v field=%s details=%s", command, res.Status, cerr, cerr.Field, cerr.Details)
		}
		var out map[string]any
		_ = json.Unmarshal(res.Data, &out)
		return out
	}
	create := func(doc string, data map[string]any) string {
		t.Helper()
		raw, _ := json.Marshal(data)
		out := run(kernel.CommandRecordCreate, kernel.RecordCreatePayload{Doctype: doc, Data: raw})
		name, _ := out["name"].(string)
		if name == "" {
			t.Fatalf("create %s returned no name: %v", doc, out)
		}
		return name
	}
	scalar := func(q string, args ...any) string {
		t.Helper()
		var s sql.NullString
		if err := conn.QueryRow(kdb.Rebind(dialect, q), args...).Scan(&s); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return s.String
	}
	num := func(q string, args ...any) float64 {
		t.Helper()
		var f float64
		fmt.Sscan(scalar(q, args...), &f)
		return f
	}
	table := func(name string) string { return dialect.QuoteIdent(reg.Get(name).RawTableName()) }

	// Fixtures are plain records of the generic inventory/pos DocTypes.
	pay := posPayments(t, conn, dialect, siteName)
	loc := create("Location", map[string]any{"location_name": "Main"})
	tea := create("Item", map[string]any{"item_name": "Tea", "uom": "Nos", "price": 100})
	bread := create("Item", map[string]any{"item_name": "Bread", "uom": "Nos", "price": 50})
	register := create("Register", map[string]any{"register_name": "Front", "location": loc, "enabled": 1})
	for item, qty := range map[string]int{tea: 10, bread: 5} {
		create("Stock Balance", map[string]any{"item": item, "location": loc, "quantity": qty, "reorder_level": 4})
	}

	shift := run("pos.open_shift", map[string]any{"data": map[string]any{"register": register}})
	shiftName := fmt.Sprint(shift["steps"].([]any)[0].(map[string]any)["name"])
	if shiftName == "" || shiftName == "<nil>" {
		t.Fatalf("open_shift returned no name: %v", shift)
	}
	run("pos.open_till", map[string]any{"data": map[string]any{"shift": shiftName, "opening_float": 500}})
	till := scalar("SELECT name FROM "+table("Till Session")+" WHERE shift = ?", shiftName)

	sale := run("pos.record_sale", map[string]any{"data": map[string]any{
		"register": register, "till_session": till, "location": loc,
		"items": []map[string]any{
			{"item": tea, "quantity": 2, "unit_price": 100, "amount": 200},
			{"item": bread, "quantity": 2, "unit_price": 50, "amount": 100},
		},
		"currency": "KES", "payments": []map[string]any{pay("cash", 300)},
	}})
	out, _ := sale["output"].(map[string]any)
	ref, _ := out["reference"].(string)
	if ref == "" {
		t.Fatalf("record_sale output has no reference: %v", sale)
	}
	if got := num("SELECT quantity FROM "+table("Stock Balance")+" WHERE item = ?", tea); got != 8 {
		t.Fatalf("tea stock = %v, want 8", got)
	}
	if got := num("SELECT quantity FROM "+table("Stock Balance")+" WHERE item = ?", bread); got != 3 {
		t.Fatalf("bread stock = %v, want 3", got)
	}

	// Receipt output: the pack's print format over the stored sale.
	body, err := os.ReadFile(filepath.Join("..", "config", "pos", "print_formats", "sale_receipt.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	format, err := print.Load(body)
	if err != nil {
		t.Fatal(err)
	}
	rows := func(field, columns string) []any {
		r, err := conn.Query(kdb.Rebind(dialect, "SELECT "+columns+" FROM "+dialect.QuoteIdent(reg.Get("Sale").RawChildTableName(field))+" WHERE parent = ? ORDER BY idx"), ref)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		cols, _ := r.Columns()
		var out []any
		for r.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := r.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			m := map[string]any{}
			for i, c := range cols {
				if b, ok := vals[i].([]byte); ok {
					m[c] = string(b)
				} else {
					m[c] = vals[i]
				}
			}
			out = append(out, m)
		}
		return out
	}
	text, err := format.RenderText(print.Data{
		Doc:    map[string]any{"name": ref, "total": out["total"], "items": rows("items", "item, quantity, amount"), "payments": rows("payments", "method, amount")},
		Tenant: map[string]any{"name": "Shop"}, Now: time.Now(),
	}, print.Options{Currency: "KES", Precision: 2})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{ref, "TOTAL", "Cash"} {
		if !strings.Contains(text, want) {
			t.Fatalf("receipt missing %q:\n%s", want, text)
		}
	}

	// Close the till: 500 float + 300 cash expected, 790 counted -> variance -10.
	run("pos.close_till", map[string]any{"data": map[string]any{"shift": shiftName, "closing_count": 790, "expected_cash": 800}})
	if got := num("SELECT variance FROM "+table("Till Session")+" WHERE name = ?", till); got != -10 {
		t.Fatalf("variance = %v, want -10", got)
	}

	// Published queries from both packs.
	runQuery := func(pack, name string, input map[string]any) *query.Page {
		t.Helper()
		defs, err := query.LoadDir(filepath.Join("..", "config", pack, "queries"), reg)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range defs {
			if d.Name == name {
				page, err := query.Run(ctx, conn, dialect, d, query.Request{FullRead: true, Input: input, Actor: "Administrator", Now: time.Now()})
				if err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				return page
			}
		}
		t.Fatalf("query %s missing", name)
		return nil
	}
	totals := runQuery("pos", "today_totals", map[string]any{"since": time.Now().Add(-time.Hour).UTC().Format("2006-01-02 15:04:05")})
	if len(totals.Rows) != 1 || fmt.Sprint(totals.Rows[0]["sale_count"]) != "1" || !strings.HasPrefix(fmt.Sprint(totals.Rows[0]["total"]), "300") {
		t.Fatalf("today_totals = %v", totals.Rows)
	}
	// Z report: per-method totals read from the Sale Payment child rows (via Sale.payments).
	// Paid: sale1 Cash 300, void-free; the return (Paid, Cash 100) is a separate sale row.
	byMethod := runQuery("pos", "payments_by_method", map[string]any{"till_session": till})
	if len(byMethod.Rows) != 1 || fmt.Sprint(byMethod.Rows[0]["method"]) != "Cash" || fmt.Sprint(byMethod.Rows[0]["payment_count"]) == "0" {
		t.Fatalf("payments_by_method = %v", byMethod.Rows)
	}
	zbody, err := os.ReadFile(filepath.Join("..", "config", "pos", "print_formats", "z_report.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	zf, err := print.Load(zbody)
	if err != nil {
		t.Fatal(err)
	}
	ztxt, err := zf.RenderText(print.Data{
		Doc:    map[string]any{"name": till, "opening_float": 500, "expected_cash": 800, "closing_count": 790, "variance": -10, "payments_by_method": byMethod.Rows},
		Tenant: map[string]any{"name": "Shop"}, Now: time.Now(),
	}, print.Options{Currency: "KES", Precision: 2})
	if err != nil || !strings.Contains(ztxt, "Cash") || !strings.Contains(ztxt, "VARIANCE") {
		t.Fatalf("z report: %v\n%s", err, ztxt)
	}
	low := runQuery("inventory", "low_stock", map[string]any{"location": loc})
	if len(low.Rows) != 1 || fmt.Sprint(low.Rows[0]["item"]) != bread {
		t.Fatalf("low_stock = %v, want only bread (3 <= 4)", low.Rows)
	}

	// Return restocks; void restocks only while the shift is open.
	stock := func(item string) float64 {
		return num("SELECT quantity FROM "+table("Stock Balance")+" WHERE item = ?", item)
	}
	one := []map[string]any{{"item": bread, "quantity": 1, "unit_price": 50, "amount": 50}}
	ret := run("pos.return_sale", map[string]any{"data": map[string]any{
		"register": register, "till_session": till, "location": loc, "return_against": ref,
		"items":    []map[string]any{{"item": tea, "quantity": 1, "unit_price": 100, "amount": 100}},
		"payments": []map[string]any{{"method": "Cash", "amount": 100, "reference": "refund"}},
	}})
	if got := stock(tea); got != 9 {
		t.Fatalf("tea after return = %v, want 9", got)
	}
	if r, _ := ret["output"].(map[string]any); r["refund"] == nil {
		t.Fatalf("return output has no refund: %v", ret)
	}
	newSale := func() string {
		s := run("pos.record_sale", map[string]any{"data": map[string]any{
			"register": register, "till_session": till, "location": loc, "items": one,
			"currency": "KES", "payments": []map[string]any{pay("cash", 50)},
		}})
		o, _ := s["output"].(map[string]any)
		return fmt.Sprint(o["reference"])
	}
	voidAt := func(sale, location string, extra map[string]any) *contract.Error {
		data := map[string]any{"sale": sale, "shift": shiftName, "till_session": till, "register": register, "location": location}
		for k, v := range extra {
			data[k] = v
		}
		raw, _ := json.Marshal(map[string]any{"data": data})
		_, cerr := k.Execute(ctx, conn, reg, kernel.Operation{Command: "pos.void_sale", Payload: raw, Context: kernel.OperationContext{
			Site: siteName, User: "Administrator", Roles: []string{doctype.AdminRole}, Actor: actor,
			Execution: execution, TenantID: contract.TenantID(siteName), Source: kernel.SourceIntegration,
		}})
		return cerr
	}
	void := func(sale string) *contract.Error { return voidAt(sale, loc, nil) }
	s3 := newSale()
	if got := stock(bread); got != 2 {
		t.Fatalf("bread after sale = %v, want 2", got)
	}
	teaBefore := stock(tea)
	if cerr := voidAt(s3, "no-such-location", nil); cerr == nil {
		t.Fatal("void with a location outside the register must be refused")
	}
	if got := stock(bread); got != 2 {
		t.Fatalf("bread after refused void = %v, want 2", got)
	}
	// Forged input lines are ignored: only the stored lines restock.
	forged := map[string]any{"items": []map[string]any{{"item": tea, "quantity": 50, "unit_price": 1, "amount": 50}}}
	if cerr := voidAt(s3, loc, forged); cerr != nil {
		t.Fatalf("void open: %+v", cerr)
	}
	if got := stock(bread); got != 3 {
		t.Fatalf("bread after void = %v, want 3 (stored line)", got)
	}
	if got := stock(tea); got != teaBefore {
		t.Fatalf("tea after forged void = %v, want %v", got, teaBefore)
	}
	if cerr := void(s3); cerr == nil {
		t.Fatal("second void must be refused")
	}
	if got := stock(bread); got != 3 {
		t.Fatalf("bread after second void = %v, want 3", got)
	}
	if st := scalar("SELECT status FROM "+table("Sale")+" WHERE name = ?", s3); st != "Void" {
		t.Fatalf("status = %s", st)
	}
	s4 := newSale()
	run("pos.close_shift", map[string]any{"data": map[string]any{"register": register}})
	if cerr := void(s4); cerr == nil {
		t.Fatal("void after shift close must be refused")
	}
	if got := stock(bread); got != 2 {
		t.Fatalf("bread after refused void = %v, want 2", got)
	}

	// Payment guard: only confirmed, matching, unused intents record a sale.
	trySale := func(pays []map[string]any) (*contract.Error, contract.CommandResult) {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{"data": map[string]any{
			"register": register, "till_session": till, "location": loc, "items": one,
			"currency": "KES", "payments": pays,
		}})
		res, cerr := k.Execute(ctx, conn, reg, kernel.Operation{Command: "pos.record_sale", Payload: raw, Context: kernel.OperationContext{
			Site: siteName, User: "Administrator", Roles: []string{doctype.AdminRole}, Actor: actor,
			Execution: execution, TenantID: contract.TenantID(siteName), Source: kernel.SourceIntegration,
		}})
		return cerr, res
	}
	rejected := func(label string, pays []map[string]any) {
		t.Helper()
		before := stock(bread)
		if cerr, res := trySale(pays); cerr == nil && res.Status == contract.StatusCompleted {
			t.Fatalf("%s payment must be rejected", label)
		}
		if got := stock(bread); got != before {
			t.Fatalf("%s: stock changed %v -> %v", label, before, got)
		}
	}
	rejected("pending", []map[string]any{pay("slow", 50)})
	rejected("failed", []map[string]any{pay("broken", 50)})
	rejected("unknown", []map[string]any{{"method": "Cash", "amount": 50, "currency": "KES", "reference": "NOPE"}})
	wrong := pay("cash", 40)
	wrong["amount"] = 50
	rejected("mismatched", []map[string]any{wrong})
	used := pay("cash", 50)
	if cerr, res := trySale([]map[string]any{used}); cerr != nil || res.Status != contract.StatusCompleted {
		t.Fatalf("confirmed cash sale: %+v", cerr)
	}
	rejected("used", []map[string]any{used})
}
