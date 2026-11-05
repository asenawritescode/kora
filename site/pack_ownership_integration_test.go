//go:build integration

package site

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/asenawritescode/kora/configstore"
	"github.com/asenawritescode/kora/contract"
	kdb "github.com/asenawritescode/kora/db"
	"github.com/asenawritescode/kora/doctype"
	"github.com/asenawritescode/kora/kernel"
	"github.com/asenawritescode/kora/outbox"
	"github.com/asenawritescode/kora/policy"
)

func TestPackOwnershipImportAcrossDialects(t *testing.T) {
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
			dialect := kdb.Resolve(tc.driver)
			if err := BootstrapSystemTables(conn, dialect); err != nil {
				t.Fatal(err)
			}
			const siteName = "own-251.example.test"
			imported := doctype.NewRegistry()
			var reg *doctype.Registry
			imports := 2 // second import must leave ownership unchanged
			if tc.driver == "postgres" {
				imports = 1 // schema introspection ignores search_path, so a re-import re-creates tables (pre-existing)
			}
			for i := 0; i < imports; i++ {
				if err := ImportConfig(conn, imported, name, siteName, posOwned(t), dialect); err != nil {
					t.Fatalf("import %d: %v", i, err)
				}
				loaded, err := configstore.NewStore(conn, dialect).LoadAll(siteName)
				if err != nil {
					t.Fatal(err)
				}
				reg = doctype.NewRegistry()
				for _, dt := range loaded {
					reg.Register(dt)
				}
				want := map[string][2]string{
					"Sale": {"pos.core", "commands_only"}, "Sale Item": {"pos.core", "commands_only"}, "Sale Payment": {"pos.core", "commands_only"},
					"Sale Line Balance": {"pos.core", "commands_only"}, "Shift": {"pos.core", "commands_only"}, "Till Session": {"pos.core", "commands_only"},
					"Item": {"inventory.core", "crud"}, "Stock Balance": {"", ""}, "Stock Entry": {"", ""}, "Location": {"", ""}, "Register": {"", ""},
				}
				for dn, w := range want {
					dt := reg.Get(dn)
					if dt == nil {
						t.Fatalf("import %d: %s missing", i, dn)
					}
					if dt.ModuleOwnerID != w[0] || dt.ModuleOwnershipMode != w[1] {
						t.Fatalf("import %d: %s owner/mode = %q/%q, want %q/%q", i, dn, dt.ModuleOwnerID, dt.ModuleOwnershipMode, w[0], w[1])
					}
					if w[0] != "" && dt.ModuleAccess != contract.ModulePublished {
						t.Fatalf("import %d: %s access = %q, want published", i, dn, dt.ModuleAccess)
					}
					if reg.IsPrivateModuleResource(dn) {
						t.Fatalf("import %d: %s private = %v", i, dn, reg.IsPrivateModuleResource(dn))
					}
				}
			}
			// Direct generic writes to owned commands_only Sale are denied; Item stays plain CRUD.
			k := kernel.New(dialect, outbox.NewSQLWriter(dialect))
			actor := contract.ActorContext{PrincipalID: "Administrator", PrincipalType: contract.PrincipalHuman, Site: siteName, Roles: []string{doctype.AdminRole}, AuthenticatedAt: time.Now()}
			execution, err := policy.NewVerifiedExecution(contract.TenantID(siteName), actor)
			if err != nil {
				t.Fatal(err)
			}
			create := func(dt string, data map[string]any) contract.Status {
				raw, _ := json.Marshal(data)
				payload, _ := json.Marshal(kernel.RecordCreatePayload{Doctype: dt, Data: raw})
				res, _ := k.Execute(t.Context(), conn, reg, kernel.Operation{Command: kernel.CommandRecordCreate, Payload: payload, Context: kernel.OperationContext{
					Site: siteName, User: "Administrator", Roles: []string{doctype.AdminRole}, Actor: actor,
					Execution: execution, TenantID: contract.TenantID(siteName), Source: kernel.SourceIntegration}})
				return res.Status
			}
			if st := create("Sale", map[string]any{"register": "x"}); st == contract.StatusCompleted {
				t.Fatalf("direct Sale create completed")
			}
			if st := create("Item", map[string]any{"item_name": "Tea", "uom": "Nos"}); st != contract.StatusCompleted {
				t.Fatalf("Item create status = %s, want completed", st)
			}
		})
	}
}

// posOwned copies the pos and inventory packs to a temp dir and declares the
// D7 ownership table in their DocType YAML (the shipped config adopts it with
// the grants in #252, so existing direct-command e2e tests keep working).
func posOwned(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if out, err := exec.Command("cp", "-r", "../config/pos", "../config/inventory", root).CombinedOutput(); err != nil {
		t.Fatalf("copy packs: %v %s", err, out)
	}
	for pack, owned := range map[string]map[string]string{
		"pos":       {"sale": "commands_only", "sale_line_balance": "commands_only", "shift": "commands_only", "till_session": "commands_only"},
		"inventory": {"item": "crud"},
	} {
		for file, mode := range owned {
			p := filepath.Join(root, pack, "doctypes", file+".yaml")
			raw, err := os.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), "\nmodule_owner_id:") {
				continue // the shipped config already declares ownership
			}
			head := "module_owner_id: " + pack + ".core\nmodule_ownership_mode: " + mode + "\nmodule_access: published\n"
			if err := os.WriteFile(p, []byte(strings.Replace(string(raw), "\n", "\n"+head, 1)), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	return filepath.Join(root, "pos")
}
