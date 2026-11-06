//go:build integration

package site

import (
	"context"
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
)

// TestPOSCashierTransactsOnlyOnOwnTill drives every till-taking POS operation
// through the published-operation route as different actors: a Cashier may
// only act on a till/shift they opened (open_session records them), Owner,
// Manager and Administrator may act on any, and tills opened the old way (open_shift +
// open_till, no cashier on the Till Session) are Owner/Manager/Administrator only.
func TestPOSCashierTransactsOnlyOnOwnTill(t *testing.T) {
	dsn := os.Getenv("KORA_POS_E2E_MYSQL_DSN")
	if dsn == "" {
		t.Skip("set KORA_POS_E2E_MYSQL_DSN to a disposable mysql server")
	}
	conn, dbName := posDB(t, "mysql", dsn)
	ctx := context.Background()
	dialect := kdb.Resolve("mysql")
	if err := BootstrapSystemTables(conn, dialect); err != nil {
		t.Fatal(err)
	}
	reg := doctype.NewRegistry()
	const siteName = "pos-till-owner.example.test"
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
			d.InputSchema = contract.SchemaRef{ID: "pos.owner." + d.Name + ".in", Version: 1}
			d.OutputSchema = contract.SchemaRef{ID: "pos.owner." + d.Name + ".out", Version: 1}
			if err := cmds.Register(d); err != nil {
				t.Fatal(err)
			}
		}
	}
	k := kernel.New(dialect, outbox.NewSQLWriter(dialect))
	k.Commands = cmds
	k.CommandSchemas = allowAllPOSSchemas{}
	scope := contract.BusinessScope{TenantID: contract.TenantID(siteName), Kind: contract.ScopeKindTenant}

	run := func(user string, roles []string, command string, payload any) (map[string]any, *contract.Error) {
		t.Helper()
		actor := contract.ActorContext{PrincipalID: user, PrincipalType: contract.PrincipalHuman, Site: siteName, Roles: roles, AuthenticatedAt: time.Now()}
		execution, err := policy.NewVerifiedExecution(contract.TenantID(siteName), actor)
		if err != nil {
			t.Fatal(err)
		}
		oc := kernel.OperationContext{Site: siteName, User: user, UserRole: roles[0], Roles: roles, Actor: actor,
			Execution: execution, TenantID: contract.TenantID(siteName), BusinessScope: scope, Source: kernel.SourceIntegration}
		if strings.HasPrefix(command, "pos.") {
			me, merr := contract.NewCommandModuleExecution(scope, "pos.core", "pos.core", 1, command, command)
			if merr != nil {
				t.Fatal(merr)
			}
			oc.ModuleExecution = &me
		}
		raw, _ := json.Marshal(payload)
		res, cerr := k.Execute(ctx, conn, reg, kernel.Operation{Command: command, Payload: raw, Context: oc})
		var out map[string]any
		_ = json.Unmarshal(res.Data, &out)
		return out, cerr
	}
	admin := func(command string, payload any) map[string]any {
		t.Helper()
		out, cerr := run("Administrator", posAdminRoles, command, payload)
		if cerr != nil {
			t.Fatalf("%s: %+v", command, cerr)
		}
		return out
	}
	create := func(doc string, data map[string]any) string {
		raw, _ := json.Marshal(data)
		return fmt.Sprint(admin(kernel.CommandRecordCreate, kernel.RecordCreatePayload{Doctype: doc, Data: raw})["name"])
	}
	output := func(out map[string]any, key string) string {
		return fmt.Sprint(out["output"].(map[string]any)[key])
	}

	loc := create("Location", map[string]any{"location_name": "Main"})
	tea := create("Item", map[string]any{"item_name": "Tea", "uom": "Nos", "price": 100})
	create("Stock Balance", map[string]any{"item": tea, "location": loc, "quantity": 500, "reorder_level": 1})
	regOf := map[string]string{}
	for _, n := range []string{"A", "B", "C", "Old"} {
		regOf[n] = create("Register", map[string]any{"register_name": n, "location": loc, "enabled": 1})
	}
	cashierRoles, managerRoles, ownerRoles := []string{"Cashier"}, []string{"Manager"}, []string{"Owner"}
	open := func(user, register string) (till, shift string) {
		out, cerr := run(user, cashierRoles, "pos.open_session", map[string]any{"data": map[string]any{"register": regOf[register], "location": loc, "opening_float": 100}})
		if cerr != nil {
			t.Fatalf("open_session %s: %+v", user, cerr)
		}
		return output(out, "reference"), output(out, "shift")
	}
	tillA, shiftA := open("alice", "A")
	tillB, shiftB := open("bob", "B")
	_, shiftC := open("carol", "C")
	// The old way: no cashier is recorded on the Till Session.
	shiftOld := fmt.Sprint(admin("pos.open_shift", map[string]any{"data": map[string]any{"register": regOf["Old"]}})["steps"].([]any)[0].(map[string]any)["name"])
	admin("pos.open_till", map[string]any{"data": map[string]any{"shift": shiftOld, "opening_float": 100}})
	var tillOld string
	if err := conn.QueryRow(kdb.Rebind(dialect, "SELECT name FROM "+dialect.QuoteIdent(reg.Get("Till Session").RawTableName())+" WHERE shift = ?"), shiftOld).Scan(&tillOld); err != nil {
		t.Fatal(err)
	}
	pay := posPayments(t, conn, dialect, siteName)

	tillRegister := map[string]string{tillA: regOf["A"], tillB: regOf["B"], tillOld: regOf["Old"]}
	tillShift := map[string]string{tillA: shiftA, tillB: shiftB, tillOld: shiftOld}
	saleOn := func(till string) map[string]any {
		return map[string]any{"data": map[string]any{
			"register": tillRegister[till], "till_session": till, "location": loc, "currency": "KES",
			"items":    []map[string]any{{"item": tea, "quantity": 3, "unit_price": 100, "amount": 300}},
			"payments": []map[string]any{pay("cash", 300)}}}
	}
	const refused = "belongs to another cashier"
	type actor struct {
		user  string
		roles []string
	}
	alice, bob := actor{"alice", cashierRoles}, actor{"bob", cashierRoles}
	mgr, owner := actor{"mgr", managerRoles}, actor{"owner", ownerRoles}
	adminOnly := actor{"shopowner", posAdminRoles}

	// record_sale: whose till, who acts, what happens.
	sales := map[string]string{}
	for _, tc := range []struct {
		name string
		who  actor
		till string
		ok   bool
	}{
		{"cashier on own till", alice, tillA, true},
		{"another cashier's till", alice, tillB, false},
		{"other cashier on first till", bob, tillA, false},
		{"old-style till, cashier", alice, tillOld, false},
		{"manager on a cashier's till", mgr, tillA, true},
		{"owner on a cashier's till", owner, tillB, true},
		{"manager on old-style till", mgr, tillOld, true},
		{"owner on old-style till", owner, tillOld, true},
		{"administrator only, a cashier's till", adminOnly, tillB, true},
		{"administrator only, old-style till", adminOnly, tillOld, true},
	} {
		t.Run("record_sale/"+tc.name, func(t *testing.T) {
			out, cerr := run(tc.who.user, tc.who.roles, "pos.record_sale", saleOn(tc.till))
			checkTillGuard(t, cerr, tc.ok, refused)
			if tc.ok {
				sales[tc.name] = output(out, "reference")
			}
		})
	}

	// return_sale and void_sale act on a till's own sale.
	returnOn := func(sale, till string) map[string]any {
		return map[string]any{"data": map[string]any{
			"register": tillRegister[till], "till_session": till, "location": loc, "return_against": sale,
			"items":    []map[string]any{{"item": tea, "quantity": 1, "unit_price": 100, "amount": 100}},
			"payments": []map[string]any{{"method": "Cash", "amount": 100, "reference": "refund"}}}}
	}
	for _, tc := range []struct {
		name string
		who  actor
		sale string
		till string
		ok   bool
	}{
		{"another cashier", bob, sales["cashier on own till"], tillA, false},
		{"own till", alice, sales["cashier on own till"], tillA, true},
		{"manager", mgr, sales["cashier on own till"], tillA, true},
		{"old-style till, cashier", alice, sales["manager on old-style till"], tillOld, false},
		{"old-style till, manager", mgr, sales["manager on old-style till"], tillOld, true},
		{"administrator only, a cashier's till", adminOnly, sales["cashier on own till"], tillA, true},
	} {
		t.Run("return_sale/"+tc.name, func(t *testing.T) {
			_, cerr := run(tc.who.user, tc.who.roles, "pos.return_sale", returnOn(tc.sale, tc.till))
			checkTillGuard(t, cerr, tc.ok, refused)
		})
	}
	voidOn := func(sale, till string) map[string]any {
		return map[string]any{"data": map[string]any{"sale": sale, "shift": tillShift[till], "till_session": till, "register": tillRegister[till], "location": loc}}
	}
	voidable := func(till string) string {
		out, cerr := run("Administrator", posAdminRoles, "pos.record_sale", saleOn(till))
		if cerr != nil {
			t.Fatalf("seed sale: %+v", cerr)
		}
		return output(out, "reference")
	}
	for _, tc := range []struct {
		name string
		who  actor
		till string
		ok   bool
	}{
		{"another cashier", bob, tillA, false},
		{"own till", alice, tillA, true},
		{"manager", mgr, tillA, true},
		{"owner", owner, tillB, true},
		{"old-style till, cashier", alice, tillOld, false},
		{"old-style till, owner", owner, tillOld, true},
		{"administrator only, a cashier's till", adminOnly, tillB, true},
	} {
		t.Run("void_sale/"+tc.name, func(t *testing.T) {
			sale := voidable(tc.till)
			_, cerr := run(tc.who.user, tc.who.roles, "pos.void_sale", voidOn(sale, tc.till))
			checkTillGuard(t, cerr, tc.ok, refused)
		})
	}

	// close_till (by shift), then close_shift (by register).
	closeTill := func(shift string) map[string]any {
		return map[string]any{"data": map[string]any{"shift": shift, "closing_count": 100}}
	}
	for _, tc := range []struct {
		name  string
		who   actor
		shift string
		ok    bool
	}{
		{"another cashier", bob, shiftA, false},
		{"old-style till, cashier", alice, shiftOld, false},
		{"own till", alice, shiftA, true},
		{"manager on a cashier's till", mgr, shiftB, true},
		{"manager on old-style till", mgr, shiftOld, true},
		{"administrator only, a cashier's till", adminOnly, shiftC, true},
	} {
		t.Run("close_till/"+tc.name, func(t *testing.T) {
			_, cerr := run(tc.who.user, tc.who.roles, "pos.close_till", closeTill(tc.shift))
			checkTillGuard(t, cerr, tc.ok, refused)
		})
	}
	// open_shift records the cashier on the Shift (here the administrator), so
	// a Cashier cannot close it, while Manager/Owner can.
	for _, tc := range []struct {
		name     string
		who      actor
		register string
		ok       bool
	}{
		{"another cashier", bob, "A", false},
		{"cashier on a shift they did not open", alice, "Old", false},
		{"own shift", alice, "A", true},
		{"manager on a cashier's shift", mgr, "B", true},
		{"owner on a shift opened by someone else", owner, "Old", true},
		{"administrator only, a cashier's shift", adminOnly, "C", true},
	} {
		t.Run("close_shift/"+tc.name, func(t *testing.T) {
			_, cerr := run(tc.who.user, tc.who.roles, "pos.close_shift", map[string]any{"data": map[string]any{"register": regOf[tc.register]}})
			checkTillGuard(t, cerr, tc.ok, "belongs to another cashier")
		})
	}
}

func checkTillGuard(t *testing.T, cerr *contract.Error, ok bool, refusal string) {
	t.Helper()
	switch {
	case ok && cerr != nil:
		t.Fatalf("refused: %+v", cerr)
	case !ok && (cerr == nil || cerr.Type != contract.CodeConflict || !strings.Contains(cerr.Message, refusal)):
		t.Fatalf("err = %+v, want a conflict containing %q", cerr, refusal)
	}
}
