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
)

func TestPOSCommandFeaturesAcrossDialects(t *testing.T) {
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
			runPOSCommandFeatures(t, tc.driver, conn, name)
		})
	}
}

func runPOSCommandFeatures(t *testing.T, driver string, conn *sql.DB, dbName string) {
	ctx := context.Background()
	dialect := kdb.Resolve(driver)
	if err := BootstrapSystemTables(conn, dialect); err != nil {
		t.Fatal(err)
	}
	reg := doctype.NewRegistry()
	const siteName = "pos-cmd.example.test"
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
		out := must(kernel.CommandRecordCreate, "", kernel.RecordCreatePayload{Doctype: doc, Data: raw})
		return fmt.Sprint(out["name"])
	}
	loc := id("Location", map[string]any{"location_name": "Main"})
	register := id("Register", map[string]any{"register_name": "Front", "location": loc, "enabled": 1})

	// #94: $actor and $now are resolved at runtime and persisted.
	before := time.Now().Add(-time.Minute)
	first := must("pos.open_shift", "open-1", map[string]any{"data": map[string]any{"register": register}})
	shift := fmt.Sprint(first["steps"].([]any)[0].(map[string]any)["name"])
	if got := q("SELECT cashier FROM "+table("Shift")+" WHERE name = ?", shift); got != "cashier-1" {
		t.Fatalf("$actor cashier = %q", got)
	}
	startedAt := q("SELECT started_at FROM "+table("Shift")+" WHERE name = ?", shift)
	if startedAt == "" {
		t.Fatal("$now started_at empty")
	}
	if strings.Contains(startedAt, "0000-00-00") || time.Now().Before(before) {
		t.Fatalf("started_at = %q", startedAt)
	}

	// #94: replay with the same idempotency key returns the same result and $now.
	time.Sleep(1100 * time.Millisecond)
	replay := must("pos.open_shift", "open-1", map[string]any{"data": map[string]any{"register": register}})
	if fmt.Sprint(replay["steps"].([]any)[0].(map[string]any)["name"]) != shift {
		t.Fatalf("replay created another shift: %v", replay)
	}
	if n := q("SELECT COUNT(*) FROM " + table("Shift")); n != "1" {
		t.Fatalf("shifts after replay = %s, want 1", n)
	}
	if got := q("SELECT started_at FROM "+table("Shift")+" WHERE name = ?", shift); got != startedAt {
		t.Fatalf("replay changed $now: %q -> %q", startedAt, got)
	}

	// #110: a second open shift on the same register is refused, nothing written.
	if _, cerr := exec("pos.open_shift", "open-2", map[string]any{"data": map[string]any{"register": register}}); cerr == nil || !strings.Contains(cerr.Message, "already has an open shift") {
		t.Fatalf("second open_shift err = %+v", cerr)
	}
	if n := q("SELECT COUNT(*) FROM " + table("Shift")); n != "1" {
		t.Fatalf("shifts after rejected open = %s, want 1", n)
	}

	// #111: open_till, then close_till records variance = counted - expected.
	must("pos.open_till", "", map[string]any{"data": map[string]any{"shift": shift, "opening_float": 500}})
	if _, cerr := exec("pos.open_till", "", map[string]any{"data": map[string]any{"shift": shift, "opening_float": 1}}); cerr == nil {
		t.Fatal("second open_till for the same shift was accepted")
	}
	must("pos.close_till", "", map[string]any{"data": map[string]any{"shift": shift, "closing_count": 495, "expected_cash": 800}})
	if got := q("SELECT variance FROM "+table("Till Session")+" WHERE shift = ?", shift); got != "-5" && !strings.HasPrefix(got, "-5.0") {
		t.Fatalf("variance = %q, want -5", got)
	}

	// close_shift: $now on update, and the register can open a new shift.
	must("pos.close_shift", "", map[string]any{"data": map[string]any{"register": register}})
	if got := q("SELECT ended_at FROM "+table("Shift")+" WHERE name = ?", shift); got == "" {
		t.Fatal("close_shift did not set ended_at via $now")
	}
	must("pos.open_shift", "open-3", map[string]any{"data": map[string]any{"register": register}})
}
