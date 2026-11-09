//go:build integration
// +build integration

package kernel_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/internal/configstore"
	kdb "github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/disposabledb"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/kernel"
	"github.com/asenawritescode/kora/internal/outbox"
	"github.com/asenawritescode/kora/internal/policy"
	"github.com/asenawritescode/kora/internal/schema"
)

// TestPublishedOperationAuthority runs against disposable DBs:
// KORA_OPAUTH_MYSQL_DSN and KORA_OPAUTH_POSTGRES_DSN.
func TestPublishedOperationAuthority(t *testing.T) {
	for _, tc := range []struct{ name, env string }{{"mysql", "KORA_OPAUTH_MYSQL_DSN"}, {"postgres", "KORA_OPAUTH_POSTGRES_DSN"}} {
		t.Run(tc.name, func(t *testing.T) {
			dsn := os.Getenv(tc.env)
			if dsn == "" {
				t.Skipf("set %s", tc.env)
			}
			conn := disposabledb.Open(t, tc.name, dsn).DB
			d := kdb.Resolve(tc.name)
			ddl := d.SystemTableSQL()
			if tc.name == "postgres" {
				ddl = append(ddl, kdb.OutboxTablesPostgres()...)
				ddl = append(ddl, kdb.KernelTablesPostgres()...)
			} else {
				ddl = append(ddl, kdb.OutboxTablesMySQL()...)
				ddl = append(ddl, kdb.KernelTablesMySQL()...)
			}
			for _, s := range append(ddl, configstore.ConfigRevisionSchema(d)...) {
				if strings.HasPrefix(strings.TrimSpace(s), "CREATE TABLE IF NOT EXISTS") {
					if _, err := conn.Exec(s); err != nil {
						t.Fatalf("ddl: %v", err)
					}
				}
			}
			if err := configstore.SeedConfigRevision(context.Background(), conn, d); err != nil {
				t.Fatal(err)
			}
			mk := func(name, owner, mode string) *doctype.DocType {
				return &doctype.DocType{Name: name, Module: "T", ModuleOwnerID: owner, ModuleOwnershipMode: mode,
					Fields: []doctype.Field{{Fieldname: "label", Fieldtype: "Data", Label: "Label"}}}
			}
			reg := doctype.NewRegistry()
			reg.LoadFull([]*doctype.DocType{
				mk("Sale", "pos.core", string(contract.MutationCommands)), mk("Tally", "pos.core", string(contract.MutationCRUD)),
				mk("Stock", "", ""), mk("Foreign", "inv.core", string(contract.MutationCRUD)), mk("ForeignCmd", "inv.core", string(contract.MutationCommands)),
			}, []*doctype.Role{{Name: "Cashier"}}, []*doctype.Permission{
				{Doctype: "module.pos.core.operation.sell", Role: "Cashier", Write: true},
				{Doctype: "module.pos.core.operation.bad", Role: "Cashier", Write: true},
			})
			if err := schema.MigrateSiteFromRegistry(conn, "", reg, d); err != nil {
				t.Fatalf("migrate: %v", err)
			}
			store := &configstore.ModuleStore{DB: conn, Dialect: d}
			if _, err := conn.Exec(store.SchemaStatement()); err != nil {
				t.Fatalf("module snapshot ddl: %v", err)
			}
			snap, _ := json.Marshal(contract.ModuleSnapshot{TenantID: "tenant-op", Revision: 1, Installations: []contract.ModuleInstallation{
				{TenantID: "tenant-op", ModuleID: "pos.core", State: string(contract.ModuleActive), Revision: 1},
				{TenantID: "tenant-op", ModuleID: "inv.core", State: string(contract.ModuleActive), Revision: 1}}})
			for _, tbl := range []string{"tabSale", "tabTally", "tabStock", "tabForeign", "tabForeignCmd", "_kora_module_snapshot"} {
				_, _ = conn.Exec("DELETE FROM " + d.QuoteIdent(tbl))
			}
			if _, err := conn.Exec(kdb.Rebind(d, "INSERT INTO _kora_module_snapshot (tenant_id, revision, snapshot_json) VALUES (?, ?, ?)"), "tenant-op", 1, string(snap)); err != nil {
				t.Fatal(err)
			}
			k := kernel.New(d, outbox.NewSQLWriter(d))
			k.Commands = kernel.NewCommandRegistry()
			k.CommandSchemas = allowAllSchemas{}
			create := func(r string) kernel.CommandStep {
				return kernel.CommandStep{Create: &kernel.StepCreate{Record: r, Values: map[string]string{"label": "$input.label"}}}
			}
			register := func(op string, steps ...kernel.CommandStep) *kernel.CommandResource {
				c := &kernel.CommandResource{Name: op, Namespace: "pos", Version: 1, ModuleOwnerID: "pos.core", Capability: "pos.sale", Operation: op,
					Input: kernel.CommandInput{Record: "Sale"}, InputSchema: contract.SchemaRef{ID: "in", Version: 1}, OutputSchema: contract.SchemaRef{ID: "out", Version: 1}, Steps: steps}
				if err := k.Commands.Register(c); err != nil {
					t.Fatal(err)
				}
				return c
			}
			sell := register("sell", create("Sale"), create("Tally"), create("Stock"))
			bad := register("bad", create("Stock"), create("ForeignCmd"))
			scope := contract.BusinessScope{TenantID: "tenant-op", Kind: contract.ScopeKindTenant}
			run := func(c *kernel.CommandResource, role string, published bool) *contract.Error {
				exe, err := policy.NewVerifiedExecution("tenant-op", contract.ActorContext{PrincipalID: "u1", PrincipalType: contract.PrincipalHuman, Site: "s", Roles: []string{role}})
				if err != nil {
					t.Fatal(err)
				}
				ctx := kernel.OperationContext{Site: "s", TenantID: "tenant-op", BusinessScope: scope, Execution: exe, Source: kernel.SourceUI, User: "u1", UserRole: role, Roles: []string{role}}
				if published {
					me, err := contract.NewCommandModuleExecution(scope, "pos.core", "pos.core", 1, c.FullName(), c.FullName())
					if err != nil {
						t.Fatal(err)
					}
					ctx.ModuleExecution = &me
				}
				payload, _ := json.Marshal(map[string]any{"data": map[string]any{"label": "x"}})
				_, execErr := k.Execute(context.Background(), conn, reg, kernel.Operation{Command: c.FullName(), Payload: payload, Context: ctx})
				return execErr
			}
			count := func(table string) (n int) {
				_ = conn.QueryRow("SELECT COUNT(*) FROM " + d.QuoteIdent("tab"+table)).Scan(&n)
				return
			}
			if execErr := run(sell, "Cashier", true); execErr != nil {
				t.Fatalf("operation-grant-only caller must run published op: %v", execErr)
			}
			if count("Sale") != 1 || count("Tally") != 1 || count("Stock") != 1 {
				t.Fatalf("inner writes missing: %d/%d/%d", count("Sale"), count("Tally"), count("Stock"))
			}
			for name, res := range map[string]*contract.Error{
				"direct route": run(sell, "Cashier", false), "other module's commands_only step": run(bad, "Cashier", true),
			} {
				if res == nil {
					t.Fatalf("%s must be denied", name)
				}
			}
			if count("Sale") != 1 || count("Stock") != 1 || count("ForeignCmd") != 0 {
				t.Fatalf("denied runs wrote rows: %s", fmt.Sprint(count("Sale"), count("Stock"), count("ForeignCmd")))
			}
		})
	}
}

type allowAllSchemas struct{}

func (allowAllSchemas) Registered(contract.SchemaRef) bool                 { return true }
func (allowAllSchemas) Validate(contract.SchemaRef, json.RawMessage) error { return nil }
