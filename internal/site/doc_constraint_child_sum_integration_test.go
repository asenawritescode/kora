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

const countSessionYAML = `name: Count Session
module: Count Test
title_field: label
fields:
  - fieldname: label
    fieldtype: Data
    label: Label
  - fieldname: closing_count
    fieldtype: Currency
    label: Closing Count
  - fieldname: closing_counts
    fieldtype: Table
    options: Count Line
    label: Closing Counts
doc_constraints:
  - type: predicate
    predicate: '(== closing_count (sum "closing_counts" "amount"))'
    condition: '(> (count "closing_counts") 0)'
    message: Counted cash must equal the count lines.
`

const countLineYAML = `name: Count Line
module: Count Test
is_child_table: true
fields:
  - fieldname: denomination
    fieldtype: Currency
    label: Denomination
  - fieldname: quantity
    fieldtype: Int
    label: Quantity
  - fieldname: amount
    fieldtype: Currency
    label: Amount
    computed: "denomination * quantity"
`

const openCountYAML = `name: open_count
namespace: count
version: 1
permission: create
input:
  record: Count Session
transaction:
  - as: session
    create:
      record: Count Session
      values:
        label: $input.label
output:
  reference: $steps.session.name
`

const setCountYAML = `name: set_count
namespace: count
version: 1
permission: write
input:
  record: Count Session
transaction:
  - update:
      record: Count Session
      name: $input.session
      values:
        closing_count: $input.closing_count
        closing_counts: $input.closing_counts
output:
  reference: $input.session
`

// TestDocConstraintChildSumAcrossDialects writes child rows through an update
// and checks that a predicate over their computed amounts accepts a matching
// total and refuses a mismatched one, on MySQL and Postgres.
func TestDocConstraintChildSumAcrossDialects(t *testing.T) {
	for _, tc := range []struct{ driver, env string }{
		{"mysql", "KORA_DOCTYPE_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "KORA_DOCTYPE_DISPOSABLE_POSTGRES_DSN"},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			dsn := os.Getenv(tc.env)
			if dsn == "" {
				t.Skipf("set %s", tc.env)
			}
			conn, name := posDB(t, tc.driver, dsn)
			runDocConstraintChildSum(t, tc.driver, conn, name)
		})
	}
}

func runDocConstraintChildSum(t *testing.T, driver string, conn *sql.DB, dbName string) {
	ctx := context.Background()
	dialect := kdb.Resolve(driver)
	if err := BootstrapSystemTables(ctx, conn, dialect); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for path, body := range map[string]string{
		"doctypes/count_session.yaml": countSessionYAML,
		"doctypes/count_line.yaml":    countLineYAML,
		"commands/open_count.yaml":    openCountYAML,
		"commands/set_count.yaml":     setCountYAML,
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, path)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, path), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	reg := doctype.NewRegistry()
	const siteName = "doc-constraint-child-sum.example.test"
	if err := ImportConfig(conn, reg, dbName, siteName, dir, dialect); err != nil {
		t.Fatal(err)
	}
	cmds := kernel.NewCommandRegistry()
	defs, err := kernel.ParseCommandDir(filepath.Join(dir, "commands"))
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range defs {
		d.InputSchema = contract.SchemaRef{ID: "count.test." + d.Name + ".in", Version: 1}
		d.OutputSchema = contract.SchemaRef{ID: "count.test." + d.Name + ".out", Version: 1}
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
		res, cerr := k.Execute(ctx, conn, reg, kernel.Operation{Command: command, Payload: raw, Context: oc})
		var out map[string]any
		_ = json.Unmarshal(res.Data, &out)
		return out, cerr
	}
	out, cerr := exec("count.open_count", map[string]any{"data": map[string]any{"label": "Drawer"}})
	if cerr != nil {
		t.Fatalf("open_count: %+v", cerr)
	}
	session := fmt.Sprint(out["output"].(map[string]any)["reference"])
	lines := []any{
		map[string]any{"denomination": 200, "quantity": 1},
		map[string]any{"denomination": 100, "quantity": 1},
		map[string]any{"denomination": 50, "quantity": 1},
	}
	set := func(counted any, rows []any) *contract.Error {
		data := map[string]any{"session": session, "closing_count": counted}
		if rows != nil {
			data["closing_counts"] = rows
		}
		_, e := exec("count.set_count", map[string]any{"data": data})
		return e
	}
	if e := set(999, lines); e == nil || !strings.Contains(string(e.Details), "Counted cash must equal the count lines.") {
		t.Fatalf("mismatched total accepted: %+v", e)
	}
	if e := set(350, lines); e != nil {
		t.Fatalf("matching total refused: %+v", e)
	}
	child := reg.Get("Count Session").RawChildTableName("closing_counts")
	var stored float64
	if err := conn.QueryRow(kdb.Rebind(dialect, "SELECT COALESCE(SUM(amount), 0) FROM "+dialect.QuoteIdent(child)+" WHERE parent = ?"), session).Scan(&stored); err != nil {
		t.Fatalf("sum child amounts: %v", err)
	}
	if stored != 350 {
		t.Fatalf("stored child amount sum = %v, want 350", stored)
	}
	if e := set(999, nil); e == nil {
		t.Fatal("total that no longer matches the stored count lines accepted")
	}
	if e := set(350, nil); e != nil {
		t.Fatalf("matching total with stored lines refused: %+v", e)
	}
}
