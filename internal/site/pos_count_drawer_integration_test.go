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

const countMismatchMessage = "Counted notes and coins don't add up to the total."

// TestPOSCountDrawerAcrossDialects opens and closes a till with note and coin
// count lines. A count that does not add up is refused, count lines are
// stored in their own child table, and a session without lines is not checked.
func TestPOSCountDrawerAcrossDialects(t *testing.T) {
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
			runPOSCountDrawer(t, tc.driver, conn, name)
		})
	}
}

func runPOSCountDrawer(t *testing.T, driver string, conn *sql.DB, dbName string) {
	ctx := context.Background()
	dialect := kdb.Resolve(driver)
	if err := BootstrapSystemTables(ctx, conn, dialect); err != nil {
		t.Fatal(err)
	}
	reg := doctype.NewRegistry()
	const siteName = "pos-count-drawer.example.test"
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
			d.InputSchema = contract.SchemaRef{ID: "pos.count-drawer." + d.Name + ".in", Version: 1}
			d.OutputSchema = contract.SchemaRef{ID: "pos.count-drawer." + d.Name + ".out", Version: 1}
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
	childTable := func(field string) string {
		return dialect.QuoteIdent(reg.Get("Till Session").RawChildTableName(field))
	}
	str := func(query string, args ...any) string {
		t.Helper()
		var s sql.NullString
		if err := conn.QueryRow(kdb.Rebind(dialect, query), args...).Scan(&s); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		return s.String
	}
	num := func(query string, args ...any) float64 {
		t.Helper()
		var f float64
		if _, err := fmt.Sscan(str(query, args...), &f); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
		return f
	}
	id := func(doc string, data map[string]any) string {
		raw, _ := json.Marshal(data)
		return fmt.Sprint(must(kernel.CommandRecordCreate, kernel.RecordCreatePayload{Doctype: doc, Data: raw})["name"])
	}
	loc := id("Location", map[string]any{"location_name": "Main"})
	register := id("Register", map[string]any{"register_name": "Front", "location": loc, "enabled": 1})
	// open returns the shift and till of a fresh till with the given payload.
	open := func(data map[string]any) (string, string, *contract.Error) {
		data["register"], data["location"] = register, loc
		out, cerr := exec("pos.open_session", map[string]any{"data": data})
		if cerr != nil {
			return "", "", cerr
		}
		output := out["output"].(map[string]any)
		return fmt.Sprint(output["shift"]), fmt.Sprint(output["reference"]), nil
	}
	closeTill := func(shift string, data map[string]any) *contract.Error {
		data["shift"] = shift
		_, cerr := exec("pos.close_session", map[string]any{"data": data})
		return cerr
	}
	refused := func(cerr *contract.Error) bool {
		return cerr != nil && strings.Contains(string(cerr.Details), countMismatchMessage)
	}
	lines := func(pairs ...int) []map[string]any {
		var out []map[string]any
		for i := 0; i < len(pairs); i += 2 {
			out = append(out, map[string]any{"denomination": pairs[i], "quantity": pairs[i+1]})
		}
		return out
	}

	// Open: 450 counted against a 500 float is refused and stores nothing.
	if _, _, cerr := open(map[string]any{"opening_float": 500, "opening_counts": lines(200, 2, 50, 1)}); !refused(cerr) {
		t.Fatalf("open with mismatched lines: got %+v, want %q", cerr, countMismatchMessage)
	}
	// Open: 200x2 and 100x1 make 500.
	shift, till, cerr := open(map[string]any{"opening_float": 500, "opening_counts": lines(200, 2, 100, 1)})
	if cerr != nil {
		t.Fatalf("open with matching lines: %v %s", cerr, cerr.Details)
	}
	if n := num("SELECT COUNT(*) FROM "+childTable("opening_counts")+" WHERE parent = ?", till); n != 2 {
		t.Fatalf("opening lines stored = %v, want 2", n)
	}
	if amt := num("SELECT SUM(amount) FROM "+childTable("opening_counts")+" WHERE parent = ?", till); amt != 500 {
		t.Fatalf("opening lines amount = %v, want 500", amt)
	}

	// Close: 450 counted against a 500 expected is refused and stays Open.
	if closeErr := closeTill(shift, map[string]any{"closing_count": 500, "closing_counts": lines(100, 4, 50, 1)}); !refused(closeErr) {
		t.Fatalf("close with mismatched lines: got %+v, want %q", closeErr, countMismatchMessage)
	}
	if s := str("SELECT status FROM "+table("Till Session")+" WHERE name = ?", till); s != "Open" {
		t.Fatalf("refused close left status %q, want Open", s)
	}

	// Close: 200x2, 100x1 make 500 and are stored.
	if closeErr := closeTill(shift, map[string]any{"closing_count": 500, "closing_counts": lines(200, 2, 100, 1)}); closeErr != nil {
		t.Fatalf("close with matching lines: %+v", closeErr)
	}
	if n := num("SELECT COUNT(*) FROM "+childTable("closing_counts")+" WHERE parent = ?", till); n != 2 {
		t.Fatalf("closing lines stored = %v, want 2", n)
	}
	if amt := num("SELECT SUM(amount) FROM "+childTable("closing_counts")+" WHERE parent = ?", till); amt != 500 {
		t.Fatalf("closing lines amount = %v, want 500", amt)
	}
	// The opening lines are untouched by the close.
	if n := num("SELECT COUNT(*) FROM "+childTable("opening_counts")+" WHERE parent = ?", till); n != 2 {
		t.Fatalf("opening lines after close = %v, want 2", n)
	}
	if amt := num("SELECT SUM(quantity) FROM "+childTable("opening_counts")+" WHERE parent = ?", till); amt != 3 {
		t.Fatalf("opening quantity after close = %v, want 3", amt)
	}

	// No lines means no check: a 300 float and a 310 count close with no rows.
	shift, till, cerr = open(map[string]any{"opening_float": 300})
	if cerr != nil {
		t.Fatalf("open without lines: %+v", cerr)
	}
	if closeErr := closeTill(shift, map[string]any{"closing_count": 310}); closeErr != nil {
		t.Fatalf("close without lines: %+v", closeErr)
	}
	if n := num("SELECT COUNT(*) FROM "+childTable("closing_counts")+" WHERE parent = ?", till); n != 0 {
		t.Fatalf("closing lines without a count = %v, want 0", n)
	}
}
