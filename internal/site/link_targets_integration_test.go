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

var linkTargetFiles = map[string]string{
	"doctypes/dining_table.yaml": `name: Dining Table
module: Link Test
title_field: table_no
fields:
  - fieldname: table_no
    fieldtype: Data
    label: Table
`,
	"doctypes/menu_item.yaml": `name: Menu Item
module: Link Test
title_field: item_name
fields:
  - fieldname: item_name
    fieldtype: Data
    label: Item
`,
	"doctypes/kitchen_order.yaml": `name: Kitchen Order
module: Link Test
fields:
  - fieldname: dining_table
    fieldtype: Link
    options: Dining Table
    label: Table
  - fieldname: lines
    fieldtype: Table
    options: Kitchen Order Line
    label: Lines
`,
	"doctypes/kitchen_order_line.yaml": `name: Kitchen Order Line
module: Link Test
is_child_table: true
fields:
  - fieldname: menu_item
    fieldtype: Link
    options: Menu Item
    label: Menu item
  - fieldname: quantity
    fieldtype: Int
    label: Quantity
`,
	"commands/add_item.yaml": `name: add_item
namespace: links
version: 1
permission: create
input:
  record: Menu Item
transaction:
  - as: item
    create:
      record: Menu Item
      values:
        item_name: $input.item_name
output:
  reference: $steps.item.name
`,
	"commands/add_table.yaml": `name: add_table
namespace: links
version: 1
permission: create
input:
  record: Dining Table
transaction:
  - as: table
    create:
      record: Dining Table
      values:
        table_no: $input.table_no
output:
  reference: $steps.table.name
`,
	"commands/order.yaml": `name: order
namespace: links
version: 1
permission: create
input:
  record: Kitchen Order
transaction:
  - as: order
    create:
      record: Kitchen Order
      values:
        dining_table: $input.dining_table
        lines: $input.lines
output:
  reference: $steps.order.name
`,
	"commands/seat_and_order.yaml": `name: seat_and_order
namespace: links
version: 1
permission: create
input:
  record: Kitchen Order
transaction:
  - as: table
    create:
      record: Dining Table
      values:
        table_no: $input.table_no
  - as: order
    create:
      record: Kitchen Order
      values:
        dining_table: $steps.table.name
        lines: $input.lines
output:
  reference: $steps.order.name
`,
}

// TestLinkTargetsMustExistAcrossDialects checks that a Link on a record or on
// one of its child rows must name an existing record, and that a record written
// earlier in the same operation counts, on MySQL and Postgres.
func TestLinkTargetsMustExistAcrossDialects(t *testing.T) {
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
			runLinkTargets(t, tc.driver, conn, name)
		})
	}
}

func runLinkTargets(t *testing.T, driver string, conn *sql.DB, dbName string) {
	ctx := context.Background()
	dialect := kdb.Resolve(driver)
	if err := BootstrapSystemTables(ctx, conn, dialect); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for path, body := range linkTargetFiles {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, path)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, path), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	reg := doctype.NewRegistry()
	const siteName = "link-targets.example.test"
	if err := ImportConfig(conn, reg, dbName, siteName, dir, dialect); err != nil {
		t.Fatal(err)
	}
	cmds := kernel.NewCommandRegistry()
	defs, err := kernel.ParseCommandDir(filepath.Join(dir, "commands"))
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range defs {
		d.InputSchema = contract.SchemaRef{ID: "links.test." + d.Name + ".in", Version: 1}
		d.OutputSchema = contract.SchemaRef{ID: "links.test." + d.Name + ".out", Version: 1}
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
	run := func(command string, data map[string]any) (string, *contract.Error) {
		raw, _ := json.Marshal(map[string]any{"data": data})
		oc := kernel.OperationContext{
			Site: siteName, User: "Administrator", UserRole: doctype.AdminRole, Roles: posAdminRoles, Actor: actor,
			Execution: execution, TenantID: contract.TenantID(siteName), BusinessScope: scope, Source: kernel.SourceIntegration,
		}
		res, cerr := k.Execute(ctx, conn, reg, kernel.Operation{Command: "links." + command, Payload: raw, Context: oc})
		var out map[string]any
		_ = json.Unmarshal(res.Data, &out)
		output, _ := out["output"].(map[string]any)
		return fmt.Sprint(output["reference"]), cerr
	}
	refused := func(what string, e *contract.Error) {
		t.Helper()
		if e == nil || !strings.Contains(string(e.Details), "no longer exists") {
			t.Fatalf("%s: want a no-longer-exists refusal, got %+v details=%s", what, e, func() string { if e == nil { return "" }; return string(e.Details) }())
		}
	}

	item, e := run("add_item", map[string]any{"item_name": "Pilau"})
	if e != nil {
		t.Fatalf("add item: %+v", e)
	}
	table, e := run("add_table", map[string]any{"table_no": "T1"})
	if e != nil {
		t.Fatalf("add table: %+v", e)
	}
	line := func(menuItem string) []any { return []any{map[string]any{"menu_item": menuItem, "quantity": 1}} }

	_, e = run("order", map[string]any{"dining_table": "NOPE", "lines": line(item)})
	refused("missing parent link", e)
	_, e = run("order", map[string]any{"dining_table": table, "lines": line("NOPE")})
	refused("missing child-row link", e)
	if _, e = run("order", map[string]any{"dining_table": table, "lines": line(item)}); e != nil {
		t.Fatalf("existing links refused: %+v", e)
	}
	if _, e = run("order", map[string]any{"dining_table": "", "lines": line(item)}); e != nil {
		t.Fatalf("empty link refused: %+v", e)
	}
	if _, e = run("seat_and_order", map[string]any{"table_no": "T2", "lines": line(item)}); e != nil {
		t.Fatalf("link to a record created earlier in the same operation refused: %+v", e)
	}
}
