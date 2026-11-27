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

func TestPOSCloseReasonAcrossDialects(t *testing.T) {
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
			runPOSCloseReason(t, tc.driver, conn, name)
		})
	}
}

// isReasonRequired reports whether a close was refused for the missing reason.
func isReasonRequired(cerr *contract.Error) bool {
	return cerr != nil && cerr.Type == contract.CodeValidationFailed && strings.Contains(string(cerr.Details), "Pick a reason before closing.")
}

// runPOSCloseReason opens a 500 float per case and closes it with a counted
// amount, checking when close_session asks for a reason and what it stores.
func runPOSCloseReason(t *testing.T, driver string, conn *sql.DB, dbName string) {
	ctx := context.Background()
	dialect := kdb.Resolve(driver)
	if err := BootstrapSystemTables(ctx, conn, dialect); err != nil {
		t.Fatal(err)
	}
	reg := doctype.NewRegistry()
	const siteName = "pos-close-reason.example.test"
	if err := ImportConfig(conn, reg, dbName, siteName, filepath.Join("..", "..", "config", "pos"), dialect); err != nil {
		t.Fatal(err)
	}
	cmds := kernel.NewCommandRegistry()
	defs, err := kernel.ParseCommandDir(filepath.Join("..", "..", "config", "pos", "commands"))
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range defs {
		d.InputSchema = contract.SchemaRef{ID: "pos.close." + d.Name + ".in", Version: 1}
		d.OutputSchema = contract.SchemaRef{ID: "pos.close." + d.Name + ".out", Version: 1}
		if regErr := cmds.Register(d); regErr != nil {
			t.Fatal(regErr)
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
		out, cerr := exec(kernel.CommandRecordCreate, kernel.RecordCreatePayload{Doctype: doc, Data: raw})
		if cerr != nil {
			t.Fatalf("create %s: %+v", doc, cerr)
		}
		return fmt.Sprint(out["name"])
	}
	loc := id("Location", map[string]any{"location_name": "Main"})
	register := id("Register", map[string]any{"register_name": "Front", "location": loc, "enabled": 1})
	// open returns the shift and till of a fresh 500 float.
	open := func() (string, string) {
		t.Helper()
		out, cerr := exec("pos.open_session", map[string]any{"data": map[string]any{"register": register, "location": loc, "opening_float": 500}})
		if cerr != nil {
			t.Fatalf("open_session: %+v", cerr)
		}
		shift := fmt.Sprint(out["steps"].([]any)[0].(map[string]any)["name"])
		return shift, str("SELECT name FROM "+table("Till Session")+" WHERE shift = ?", shift)
	}
	closeWith := func(shift string, data map[string]any) *contract.Error {
		data["shift"] = shift
		_, cerr := exec("pos.close_session", map[string]any{"data": data})
		return cerr
	}

	// Expected is 500 with no sales. A difference of exactly KES 100 is not over the threshold.
	shift, _ := open()
	if cerr := closeWith(shift, map[string]any{"closing_count": 400}); cerr != nil {
		t.Fatalf("difference of -100 without reason: %+v", cerr)
	}

	// A difference over KES 100 needs a reason.
	shift, till := open()
	cerr := closeWith(shift, map[string]any{"closing_count": 350})
	if !isReasonRequired(cerr) {
		t.Fatalf("difference of -150 without reason: got %+v, want \"Pick a reason before closing.\"", cerr)
	}
	if s := str("SELECT status FROM "+table("Till Session")+" WHERE name = ?", till); s != "Open" {
		t.Fatalf("rejected close left status %q, want Open", s)
	}
	if cerr := closeWith(shift, map[string]any{"closing_count": 350, "difference_reason": "Sale not rung up", "difference_note": "Forgot to ring one tea", "left_in_drawer": 200}); cerr != nil {
		t.Fatalf("close with reason: %+v", cerr)
	}
	if got := str("SELECT difference_reason FROM "+table("Till Session")+" WHERE name = ?", till); got != "Sale not rung up" {
		t.Fatalf("difference_reason = %q", got)
	}
	if got := str("SELECT difference_note FROM "+table("Till Session")+" WHERE name = ?", till); got != "Forgot to ring one tea" {
		t.Fatalf("difference_note = %q", got)
	}
	if got := str("SELECT left_in_drawer FROM "+table("Till Session")+" WHERE name = ?", till); !strings.HasPrefix(got, "200") {
		t.Fatalf("left_in_drawer = %q, want 200", got)
	}
	if got := str("SELECT taken_out FROM "+table("Till Session")+" WHERE name = ?", till); !strings.HasPrefix(got, "150") {
		t.Fatalf("taken_out = %q, want 150 (counted 350 less left 200)", got)
	}

	// A surplus of exactly KES 100 is not over the threshold; one of 200 is.
	shift, _ = open()
	if cerr := closeWith(shift, map[string]any{"closing_count": 600}); cerr != nil {
		t.Fatalf("difference of +100 without reason: %+v", cerr)
	}
	shift, _ = open()
	if cerr := closeWith(shift, map[string]any{"closing_count": 700}); !isReasonRequired(cerr) {
		t.Fatalf("difference of +200 without reason: got %+v", cerr)
	}
}
