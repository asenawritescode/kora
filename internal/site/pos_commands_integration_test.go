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
	"github.com/asenawritescode/kora/internal/query"
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
	if err := BootstrapSystemTables(context.Background(), conn, dialect); err != nil {
		t.Fatal(err)
	}
	reg := doctype.NewRegistry()
	const siteName = "pos-cmd.example.test"
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
			d.InputSchema = contract.SchemaRef{ID: "pos.test." + d.Name + ".in", Version: 1}
			d.OutputSchema = contract.SchemaRef{ID: "pos.test." + d.Name + ".out", Version: 1}
			if err := cmds.Register(d); err != nil {
				t.Fatal(err)
			}
		}
	}
	k := kernel.New(dialect, outbox.NewSQLWriter(dialect))
	k.Commands = cmds
	k.CommandSchemas = allowAllPOSSchemas{}
	actor := contract.ActorContext{PrincipalID: "cashier-1", PrincipalType: contract.PrincipalHuman, Site: siteName, Roles: posAdminRoles, AuthenticatedAt: time.Now()}
	execution, err := policy.NewVerifiedExecution(contract.TenantID(siteName), actor)
	if err != nil {
		t.Fatal(err)
	}
	exec := func(command, key string, payload any) (contract.CommandResult, *contract.Error) {
		raw, _ := json.Marshal(payload)
		scope := contract.BusinessScope{TenantID: contract.TenantID(siteName), Kind: contract.ScopeKindTenant}
		oc := kernel.OperationContext{
			Site: siteName, User: "cashier-1", Roles: posAdminRoles, Actor: actor, IdempotencyKey: key,
			Execution: execution, TenantID: contract.TenantID(siteName), BusinessScope: scope, Source: kernel.SourceIntegration,
		}
		if strings.HasPrefix(command, "pos.") {
			// pos.core owns these DocTypes; run its commands as the module.
			me, merr := contract.NewCommandModuleExecution(scope, "pos.core", "pos.core", 1, command, command)
			if merr != nil {
				t.Fatal(merr)
			}
			oc.ModuleExecution = &me
		}
		return k.Execute(ctx, conn, reg, kernel.Operation{Command: command, Payload: raw, Context: oc})
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
	first := must("pos.open_session", "open-1", map[string]any{"data": map[string]any{"register": register, "location": loc, "opening_float": 500}})
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
	replay := must("pos.open_session", "open-1", map[string]any{"data": map[string]any{"register": register, "location": loc, "opening_float": 500}})
	if fmt.Sprint(replay["steps"].([]any)[0].(map[string]any)["name"]) != shift {
		t.Fatalf("replay created another shift: %v", replay)
	}
	if n := q("SELECT COUNT(*) FROM " + table("Shift")); n != "1" {
		t.Fatalf("shifts after replay = %s, want 1", n)
	}
	if got := q("SELECT started_at FROM "+table("Shift")+" WHERE name = ?", shift); got != startedAt {
		t.Fatalf("replay changed $now: %q -> %q", startedAt, got)
	}

	// #110: a second open on the same register is refused, nothing written.
	if _, cerr := exec("pos.open_session", "open-2", map[string]any{"data": map[string]any{"register": register, "location": loc, "opening_float": 1}}); cerr == nil || !strings.Contains(cerr.Message, "already has an open shift") {
		t.Fatalf("second open_session err = %+v", cerr)
	}
	if n := q("SELECT COUNT(*) FROM " + table("Shift")); n != "1" {
		t.Fatalf("shifts after rejected open = %s, want 1", n)
	}

	// #111: close_session records variance = counted - expected (expected starts at the float).
	must("pos.close_session", "", map[string]any{"data": map[string]any{"shift": shift, "closing_count": 495}})
	if got := q("SELECT variance FROM "+table("Till Session")+" WHERE shift = ?", shift); got != "-5" && !strings.HasPrefix(got, "-5.0") {
		t.Fatalf("variance = %q, want -5", got)
	}

	// close_session: $now on the shift update, and the register can open a new shift.
	if got := q("SELECT ended_at FROM "+table("Shift")+" WHERE name = ?", shift); got == "" {
		t.Fatal("close_session did not set ended_at via $now")
	}
	// E1: the next session on the register stores its opening note, and
	// last_close reads the session that closed before it.
	must("pos.open_session", "", map[string]any{"data": map[string]any{"register": register, "location": loc, "opening_float": 480, "opening_note": "Short 20 from the bag"}})
	session := q("SELECT shift FROM "+table("Till Session")+" WHERE register = ? AND status = ?", register, "Open")
	if got := q("SELECT opening_note FROM "+table("Till Session")+" WHERE shift = ?", session); got != "Short 20 from the bag" {
		t.Fatalf("opening_note = %q", got)
	}
	must("pos.close_session", "", map[string]any{"data": map[string]any{"shift": session, "closing_count": 470, "left_in_drawer": 200}})
	defs, err := query.LoadDir(filepath.Join("..", "..", "config", "pos", "queries"), reg)
	if err != nil {
		t.Fatal(err)
	}
	var lastClose *query.Definition
	for _, d := range defs {
		if d.Name == "last_close" {
			lastClose = d
		}
	}
	page, err := query.Run(ctx, conn, dialect, lastClose, query.Request{FullRead: true, Now: time.Now(), Input: map[string]any{"register": register}})
	if err != nil || len(page.Rows) != 1 {
		t.Fatalf("last_close = %v, %v", page, err)
	}
	row := page.Rows[0]
	// The next till opens from what the last cashier left, not what they counted.
	if fmt.Sprint(row["cashier"]) != "cashier-1" || !strings.HasPrefix(fmt.Sprint(row["closing_count"]), "470") || !strings.HasPrefix(fmt.Sprint(row["left_in_drawer"]), "200") || fmt.Sprint(row["closed_at"]) == "" {
		t.Fatalf("last_close row = %v", row)
	}

	must("pos.open_session", "open-4", map[string]any{"data": map[string]any{"register": register, "location": loc, "opening_float": 0}})
}
