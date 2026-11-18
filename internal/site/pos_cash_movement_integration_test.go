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
	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/kernel"
	"github.com/asenawritescode/kora/internal/outbox"
	"github.com/asenawritescode/kora/internal/policy"
)

// TestPOSCashMovementAcrossDialects records pay-ins and pay-outs on an open
// till and checks expected_cash and the E5 counters on both dialects. A
// pay-out over the limit is refused with the owner-approval message in the
// error details.
func TestPOSCashMovementAcrossDialects(t *testing.T) {
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
			runPOSCashMovement(t, tc.driver, conn, name)
		})
	}
}

func runPOSCashMovement(t *testing.T, driver string, conn *sql.DB, dbName string) {
	ctx := context.Background()
	dialect := kdb.Resolve(driver)
	if err := BootstrapSystemTables(ctx, conn, dialect); err != nil {
		t.Fatal(err)
	}
	reg := doctype.NewRegistry()
	const siteName = "pos-cash-movement.example.test"
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
			d.InputSchema = contract.SchemaRef{ID: "pos.cash-movement." + d.Name + ".in", Version: 1}
			d.OutputSchema = contract.SchemaRef{ID: "pos.cash-movement." + d.Name + ".out", Version: 1}
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
	opened := must("pos.open_session", map[string]any{"data": map[string]any{"register": register, "location": loc, "opening_float": 500}})
	shift := fmt.Sprint(opened["steps"].([]any)[0].(map[string]any)["name"])
	till := str("SELECT name FROM "+table("Till Session")+" WHERE shift = ?", shift)

	cell := func(column string) string {
		return str("SELECT "+column+" FROM "+table("Till Session")+" WHERE name = ?", till)
	}
	number := func(column string) float64 {
		var f float64
		if _, err := fmt.Sscan(cell(column), &f); err != nil {
			t.Fatalf("%s: %v", column, err)
		}
		return f
	}
	want := func(label, column string, v float64) {
		t.Helper()
		if got := number(column); got != v {
			t.Fatalf("%s: %s = %v, want %v", label, column, got, v)
		}
	}
	move := func(direction, reason string, amount int) (map[string]any, *contract.Error) {
		return exec("pos.record_cash_movement", map[string]any{"data": map[string]any{
			"till_session": till, "direction": direction, "reason": reason, "amount": amount, "note": "integration"}})
	}

	want("open", "expected_cash", 500)
	if _, cerr := move("In", "Change from bank", 200); cerr != nil {
		t.Fatalf("pay-in: %+v", cerr)
	}
	want("after pay-in", "expected_cash", 700)
	want("after pay-in", "payin_count", 1)
	want("after pay-in", "payin_total", 200)

	if _, cerr := move("Out", "Transport", 300); cerr != nil {
		t.Fatalf("pay-out under limit: %+v", cerr)
	}
	want("after pay-out", "expected_cash", 400)
	want("after pay-out", "payout_count", 1)
	want("after pay-out", "payout_total", 300)

	_, cerr := move("Out", "Stock purchase", 1500)
	if cerr == nil || !strings.Contains(string(cerr.Details), "Needs owner approval (coming soon)") {
		t.Fatalf("pay-out over limit not refused with owner-approval message: %+v", cerr)
	}
	want("after refused pay-out", "expected_cash", 400)
	want("after refused pay-out", "payout_count", 1)

	if _, cerr := move("In", "Other", 1500); cerr != nil {
		t.Fatalf("pay-in over the pay-out limit must still be allowed: %+v", cerr)
	}
	want("after large pay-in", "expected_cash", 1900)
	want("after large pay-in", "payin_count", 2)
	want("after large pay-in", "payin_total", 1700)

	if n := str("SELECT COUNT(*) FROM "+table("Cash Movement")+" WHERE till_session = ?", till); n != "3" {
		t.Fatalf("cash movements recorded = %s, want 3", n)
	}
}
