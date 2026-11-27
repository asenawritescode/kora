//go:build integration

package site

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/facts"
	"github.com/asenawritescode/kora/internal/kernel"
	"github.com/asenawritescode/kora/internal/metrics"
	"github.com/asenawritescode/kora/internal/outbox"
	"github.com/asenawritescode/kora/internal/policy"
	"github.com/asenawritescode/kora/internal/tenanttime"
	"github.com/asenawritescode/kora/internal/titles"
)

// A drill row and a record title show the name of the record a Link title names, followed through the chain:
// a Sale's title is its Register's name, and a Till Session's is its Shift's Register's name.
func TestPOSLinkTitlesAcrossDialects(t *testing.T) {
	for _, tc := range []struct{ driver, env string }{
		{"mysql", "KORA_SITE_DISPOSABLE_MYSQL_DSN"},
		{"postgres", "KORA_SITE_DISPOSABLE_POSTGRES_DSN"},
	} {
		t.Run(tc.driver, func(t *testing.T) {
			dsn := os.Getenv(tc.env)
			if dsn == "" {
				t.Skipf("set %s to a disposable %s server", tc.env, tc.driver)
			}
			conn, name := posDB(t, tc.driver, dsn)
			ctx := context.Background()
			dialect := kdb.Resolve(tc.driver)
			if err := BootstrapSystemTables(ctx, conn, dialect); err != nil {
				t.Fatal(err)
			}
			const siteName = "pos-link-titles.example.test"
			reg := doctype.NewRegistry()
			if err := ImportConfig(conn, reg, name, siteName, filepath.Join("..", "..", "config", "pos"), dialect); err != nil {
				t.Fatal(err)
			}
			const login = "cashier@example.test"
			if _, err := conn.ExecContext(ctx, kdb.Rebind(dialect,
				"INSERT INTO _kora_user (name, site, email, password_hash, full_name, enabled) VALUES (?, ?, ?, ?, ?, ?)"),
				login, siteName, login, "x", "Cashier", 1); err != nil {
				t.Fatal(err)
			}
			cmds := kernel.NewCommandRegistry()
			for _, pack := range []string{"inventory", "pos"} {
				defs, err := kernel.ParseCommandDir(filepath.Join("..", "..", "config", pack, "commands"))
				if err != nil {
					t.Fatal(err)
				}
				for _, d := range defs {
					d.InputSchema = contract.SchemaRef{ID: "pos.titles." + d.Name + ".in", Version: 1}
					d.OutputSchema = contract.SchemaRef{ID: "pos.titles." + d.Name + ".out", Version: 1}
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
				me, merr := contract.NewCommandModuleExecution(scope, "pos.core", "pos.core", 1, command, command)
				if merr != nil {
					t.Fatal(merr)
				}
				oc.ModuleExecution = &me
				raw, _ := json.Marshal(payload)
				res, cerr := k.Execute(ctx, conn, reg, kernel.Operation{Command: command, Payload: raw, Context: oc})
				var out map[string]any
				_ = json.Unmarshal(res.Data, &out)
				return out, cerr
			}
			create := func(doc string, data map[string]any) string {
				raw, _ := json.Marshal(data)
				out, cerr := run("Administrator", posAdminRoles, kernel.CommandRecordCreate, kernel.RecordCreatePayload{Doctype: doc, Data: raw})
				if cerr != nil {
					t.Fatalf("create %s: %+v", doc, cerr)
				}
				return fmt.Sprint(out["name"])
			}
			loc := create("Location", map[string]any{"location_name": "Main"})
			tea := create("Item", map[string]any{"item_name": "Tea", "uom": "Nos", "price": 100})
			create("Stock Balance", map[string]any{"item": tea, "location": loc, "quantity": 500, "reorder_level": 1})
			register := create("Register", map[string]any{"register_name": "R1", "location": loc, "enabled": 1})

			out, cerr := run(login, []string{"Cashier"}, "pos.open_session", map[string]any{"data": map[string]any{"register": register, "location": loc, "opening_float": 100}})
			if cerr != nil {
				t.Fatalf("open_session: %+v", cerr)
			}
			till := fmt.Sprint(out["output"].(map[string]any)["reference"])
			pay := posPayments(t, conn, dialect, siteName)
			if _, cerr := run(login, []string{"Cashier"}, "pos.record_sale", map[string]any{"data": map[string]any{
				"register": register, "till_session": till, "location": loc, "currency": "KES",
				"items":    []map[string]any{{"item": tea, "quantity": 3, "unit_price": 100, "amount": 300}},
				"payments": []map[string]any{pay("cash", 300)}}}); cerr != nil {
				t.Fatalf("record_sale: %+v", cerr)
			}

			sale := reg.Get("Sale")
			factReg := doctype.NewRegistry()
			factReg.Register(sale)
			feedSaleEvents(t, conn, dialect, factReg)
			refresher := &facts.Refresher{DB: conn, Dialect: dialect, Registry: factReg, Admission: posFactsLane{}, Owner: "pos-link-titles"}
			if _, err := refresher.RefreshDue(ctx); err != nil {
				t.Fatalf("refresh: %v", err)
			}

			defs, err := metrics.ParseDirectory(filepath.Join("..", "..", "config", "pos", "metrics"))
			if err != nil {
				t.Fatal(err)
			}
			engine := metrics.Engine{DB: conn, Dialect: dialect, Site: siteName, Catalog: metrics.NewCatalog(defs, reg.All())}
			now := time.Now().UTC()
			from, to := now.AddDate(0, 0, -1), now.AddDate(0, 0, 1)
			page, err := engine.DrillRecords(ctx, metrics.DrillRequest{Metric: "sales",
				From: tenanttime.Date{Year: from.Year(), Month: from.Month(), Day: from.Day()},
				To:   tenanttime.Date{Year: to.Year(), Month: to.Month(), Day: to.Day()}},
				metrics.Scope{}, tenanttime.Default(), "")
			if err != nil {
				t.Fatalf("drill sales: %v", err)
			}
			if len(page.Records) == 0 {
				t.Fatal("drill sales lists no record")
			}
			if got := page.Records[0].Title; got != "R1" {
				t.Fatalf("sale drill row title = %q, want the register's name R1", got)
			}

			opts := titles.Options{DB: conn, Dialect: dialect, Link: func(n string) *doctype.DocType {
				if t := reg.Get(n); titles.Readable(t) {
					return t
				}
				return nil
			}}
			got, err := titles.Resolve(ctx, opts, reg.Get("Till Session"), []string{till})
			if err != nil {
				t.Fatalf("resolve till session title: %v", err)
			}
			if got[till] != "R1" {
				t.Fatalf("till session title = %q, want its shift's register R1", got[till])
			}
		})
	}
}
