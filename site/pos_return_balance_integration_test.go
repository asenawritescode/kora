//go:build integration

package site

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/kernel"
	"github.com/asenawritescode/kora/outbox"
	"github.com/asenawritescode/kora/policy"
)

func TestPOSReturnBalancesAcrossDialects(t *testing.T) {
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
			runPOSReturnBalances(t, tc.driver, conn, name)
		})
	}
}

func runPOSReturnBalances(t *testing.T, driver string, conn *sql.DB, dbName string) {
	ctx := context.Background()
	dialect := kdb.Resolve(driver)
	if err := BootstrapSystemTables(conn, dialect); err != nil {
		t.Fatal(err)
	}
	reg := doctype.NewRegistry()
	const siteName = "pos-return.example.test"
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
			if err := cmds.Register(d); err != nil {
				t.Fatal(err)
			}
		}
	}
	k := kernel.New(dialect, outbox.NewSQLWriter(dialect))
	k.Commands = cmds
	actor := contract.ActorContext{PrincipalID: "cashier-1", PrincipalType: contract.PrincipalHuman, Site: siteName, Roles: []string{doctype.AdminRole}, AuthenticatedAt: time.Now()}
	execution, err := policy.NewVerifiedExecution(contract.TenantID(siteName), actor)
	if err != nil {
		t.Fatal(err)
	}
	exec := func(command, key string, payload any) (contract.CommandResult, *contract.Error) {
		raw, _ := json.Marshal(payload)
		return k.Execute(ctx, conn, reg, kernel.Operation{Command: command, Payload: raw, Context: kernel.OperationContext{
			Site: siteName, User: "cashier-1", Roles: []string{doctype.AdminRole}, Actor: actor, IdempotencyKey: key,
			Execution: execution, TenantID: contract.TenantID(siteName), Source: kernel.SourceIntegration,
		}})
	}
	must := func(command, key string, payload any) map[string]any {
		t.Helper()
		res, cerr := exec(command, key, payload)
		if cerr != nil || res.Status != contract.StatusCompleted {
			t.Fatalf("%s: status=%s err=%+v", command, res.Status, cerr)
		}
		var out map[string]any
		_ = json.Unmarshal(res.Data, &out)
		return out
	}
	q := func(query string, args ...any) string {
		t.Helper()
		var s sql.NullString
		if err := conn.QueryRow(kdb.Rebind(dialect, query), args...).Scan(&s); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		return s.String
	}
	table := func(n string) string { return dialect.QuoteIdent(reg.Get(n).RawTableName()) }
	id := func(doc string, data map[string]any) string {
		raw, _ := json.Marshal(data)
		return fmt.Sprint(must(kernel.CommandRecordCreate, "", kernel.RecordCreatePayload{Doctype: doc, Data: raw})["name"])
	}
	loc := id("Location", map[string]any{"location_name": "Main"})
	otherLoc := id("Location", map[string]any{"location_name": "Other"})
	register := id("Register", map[string]any{"register_name": "Front", "location": loc, "enabled": 1})
	tea := id("Item", map[string]any{"item_name": "Tea", "uom": "Nos", "price": 19.99})
	id("Stock Balance", map[string]any{"item": tea, "location": loc, "quantity": 20, "reorder_level": 1})
	shift := fmt.Sprint(must("pos.open_shift", "", map[string]any{"data": map[string]any{"register": register}})["steps"].([]any)[0].(map[string]any)["name"])
	must("pos.open_till", "", map[string]any{"data": map[string]any{"shift": shift, "opening_float": 0}})
	till := q("SELECT name FROM "+table("Till Session")+" WHERE shift = ?", shift)

	sell := func(qty int) string {
		out := must("pos.record_sale", "", map[string]any{"data": map[string]any{
			"register": register, "till_session": till, "location": loc,
			"items":    []map[string]any{{"item": tea, "quantity": qty, "unit_price": 19.99, "amount": 19.99 * float64(qty)}},
			"payments": []map[string]any{{"method": "Cash", "amount": 19.99 * float64(qty)}},
		}})
		return fmt.Sprint(out["output"].(map[string]any)["reference"])
	}
	ret := func(sale, key, reg, location string, qty int, price float64) (contract.CommandResult, *contract.Error) {
		return exec("pos.return_sale", key, map[string]any{"data": map[string]any{
			"register": reg, "till_session": till, "location": location, "return_against": sale,
			"items":    []map[string]any{{"item": tea, "quantity": qty, "unit_price": price, "amount": price * float64(qty)}},
			"payments": []map[string]any{{"method": "Cash", "amount": price * float64(qty)}},
		}})
	}
	stock := func() string { return q("SELECT quantity FROM "+table("Stock Balance")+" WHERE item = ?", tea) }
	counts := func() string {
		return q("SELECT COUNT(*) FROM "+table("Sale")) + "/" + q("SELECT COUNT(*) FROM "+table("Stock Entry")) + "/" + stock()
	}
	remaining := func(sale string) string {
		return q("SELECT remaining_quantity FROM "+table("Sale Line Balance")+" WHERE sale = ?", sale)
	}
	refuse := func(label string, want contract.Code, before string, res contract.CommandResult, cerr *contract.Error) {
		t.Helper()
		if cerr == nil || cerr.Type != want {
			t.Fatalf("%s: want %s, got %+v status=%s", label, want, cerr, res.Status)
		}
		if got := counts(); got != before {
			t.Fatalf("%s wrote data: %s -> %s", label, before, got)
		}
	}
	num := func(s string) float64 { var f float64; fmt.Sscan(s, &f); return f }

	s := sell(3)
	if num(stock()) != 17 || num(remaining(s)) != 3 {
		t.Fatalf("after sale stock=%s remaining=%s", stock(), remaining(s))
	}
	// 1. partial then repeat returns restore stock and drain the balance.
	for _, n := range []int{2, 1} {
		if _, cerr := ret(s, "", register, loc, n, 19.99); cerr != nil {
			t.Fatalf("return %d: %+v", n, cerr)
		}
	}
	if num(stock()) != 20 || num(remaining(s)) != 0 {
		t.Fatalf("stock=%s remaining=%s", stock(), remaining(s))
	}
	// 2. a further return is refused.
	before := counts()
	res, cerr := ret(s, "", register, loc, 1, 19.99)
	refuse("extra return", contract.CodeConflict, before, res, cerr)

	// 3. single over-return and 4. price mismatch write nothing.
	s2 := sell(3)
	before = counts()
	res, cerr = ret(s2, "", register, loc, 4, 19.99)
	refuse("over-return", contract.CodeConflict, before, res, cerr)
	res, cerr = ret(s2, "", register, loc, 1, 20.00)
	refuse("price mismatch", contract.CodeNotFound, before, res, cerr)
	// 7. location not the register's.
	res, cerr = ret(s2, "", register, otherLoc, 1, 19.99)
	if cerr == nil || counts() != before {
		t.Fatalf("wrong location must be refused: %+v", cerr)
	}

	// 6. replay does not decrement twice.
	first, cerr := ret(s2, "ret-key", register, loc, 1, 19.99)
	if cerr != nil {
		t.Fatal(cerr)
	}
	replay, cerr := ret(s2, "ret-key", register, loc, 1, 19.99)
	if cerr != nil || string(replay.Data) != string(first.Data) || num(remaining(s2)) != 2 {
		t.Fatalf("replay: err=%+v remaining=%s", cerr, remaining(s2))
	}

	// 5. two concurrent returns of 2 against the remaining 2: exactly one commits.
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, cerr := ret(s2, "", register, loc, 2, 19.99); cerr == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if ok != 1 || num(remaining(s2)) != 0 {
		t.Fatalf("concurrent commits=%d remaining=%s", ok, remaining(s2))
	}
}
