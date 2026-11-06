package doctype

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestPOSRolePermissions(t *testing.T) {
	dir := filepath.Join("..", "config", "pos")
	roles, err := ParseRolesFile(filepath.Join(dir, "roles.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	perms, err := ParsePermissionsFile(filepath.Join(dir, "permissions.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	invPerms, err := ParsePermissionsFile(filepath.Join("..", "config", "inventory", "permissions.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	perms = append(perms, invPerms...)
	pm := NewPermissionMatrix()
	for _, r := range roles {
		pm.AddRole(r)
	}
	for _, p := range perms {
		pm.SetPermission(p)
	}
	tests := []struct {
		role, dt, op string
		want         bool
	}{
		{"Cashier", "Sale", "read", true},
		{"Cashier", "Sale", "create", false},
		{"Cashier", "Sale", "write", false},
		{"Cashier", "Sale", "delete", false},
		{"Cashier", "Sale Line Balance", "read", true},
		{"Cashier", "Sale Line Balance", "write", false},
		{"Cashier", "Shift", "read", true},
		{"Cashier", "Shift", "write", false},
		{"Cashier", "Till Session", "read", true},
		{"Cashier", "Till Session", "create", false},
		{"Cashier", "Till Session", "write", false},
		{"Cashier", "Register", "read", true},
		{"Cashier", "Register", "write", false},
		{"Cashier", "Stock Entry", "create", false},
		{"Cashier", "Stock Balance", "write", false},
		{"Cashier", "Item", "read", false}, // Item carries cost
		{"Manager", "Sale", "read", true},
		{"Manager", "Sale", "write", false},
		{"Manager", "Till Session", "write", false},
		{"Owner", "Sale", "read", true},
		{"Owner", "Sale", "delete", false}, // corrections go through void_sale/return_sale
		{"Owner", "Till Session", "write", false},
		{"Owner", "Item", "read", true},
		{"Owner", "Item", "create", true},
		{"Owner", "Location", "create", true},
		{"Owner", "Item Category", "create", true},
		{"Manager", "Item Category", "read", true},
		{"Manager", "Item Category", "write", false},
		{"Cashier", "Item Category", "write", false},
		{"Owner", "Stock Balance", "create", true},
		// Managers must see stock (item_stock/low_stock read Stock Balance): read-only.
		{"Manager", "Stock Balance", "read", true},
		{"Manager", "Stock Balance", "create", false},
		{"Manager", "Stock Balance", "write", false},
		{"Manager", "Stock Balance", "delete", false},
		{"Owner", "Stock Entry", "create", true},
		// #397: the Inventory Manager maintains the catalogue, so may import it; nobody else on the till side may.
		{"Inventory Manager", "Item", "import", true},
		{"Inventory Manager", "Item Category", "import", true},
		{"Inventory Manager", "Item", "create", true},
		{"Inventory Manager", "Item Category", "read", true},
		{"Inventory Manager", "Location", "import", false},
		{"Inventory Manager", "Stock Balance", "import", false},
		{"Inventory Manager", "Stock Entry", "import", false},
		{"Cashier", "Item", "import", false},
		{"Cashier", "Item Category", "import", false},
		{"Manager", "Item", "import", false},
		{"Manager", "Item Category", "import", false},
	}
	for _, tc := range tests {
		if got := pm.Can(tc.role, tc.dt, tc.op); got != tc.want {
			t.Errorf("%s %s %s = %v, want %v", tc.role, tc.op, tc.dt, got, tc.want)
		}
	}
	posOps := []string{"open_session", "close_session", "open_shift", "close_shift", "open_till", "close_till", "record_sale", "return_sale", "void_sale"}
	invOps := []string{"receive_stock", "adjust_stock"}
	posQ := []string{"closed_session", "my_registers", "my_session", "open_till", "payments_by_method", "refunds_by_method", "recent_sales", "sale_items", "sale_payments", "sale_lines", "today_totals"}
	invQ := []string{"item_search", "item_categories", "item_stock", "low_stock"}
	grant := func(role, owner, kind string, names []string, op string, want bool) {
		for _, n := range names {
			if got := pm.Can(role, "module."+owner+"."+kind+"."+n, op); got != want {
				t.Errorf("%s %s module.%s.%s.%s = %v, want %v", role, op, owner, kind, n, got, want)
			}
		}
	}
	grant("Owner", "pos.core", "operation", posOps, "write", true)
	grant("Owner", "pos.core", "query", posQ, "read", true)
	grant("Owner", "inventory.core", "operation", invOps, "write", true)
	grant("Owner", "inventory.core", "query", invQ, "read", true)
	grant("Manager", "pos.core", "operation", posOps, "write", true)
	grant("Manager", "pos.core", "query", posQ, "read", true)
	grant("Manager", "inventory.core", "query", invQ, "read", true)
	grant("Manager", "inventory.core", "operation", invOps, "write", false)
	grant("Cashier", "pos.core", "operation", posOps, "write", true)
	grant("Cashier", "pos.core", "query", []string{"open_till", "my_session", "my_registers", "sale_lines", "my_closed_session", "my_sales"}, "read", true)
	grant("Cashier", "pos.core", "query", []string{"closed_session"}, "read", false)
	// my_session is owner-scoped by cashier for every role (#326).
	for _, role := range []string{"Cashier", "Manager", "Owner"} {
		if owner, _ := pm.RowScopeFor([]string{role}, "module.pos.core.query.my_session", "read"); !owner {
			t.Errorf("%s my_session grant must be if_owner", role)
		}
	}
	// #384: every role sees exactly one closed-till query. Manager and Owner read
	// any till (closed_session, not owner-scoped); a Cashier reads only their own
	// (my_closed_session, if_owner). No role holds both, so the catalog picks.
	for role, want := range map[string]string{"Cashier": "my_closed_session", "Manager": "closed_session", "Owner": "closed_session"} {
		other := "my_closed_session"
		if want == other {
			other = "closed_session"
		}
		if !pm.Can(role, "module.pos.core.query."+want, "read") || pm.Can(role, "module.pos.core.query."+other, "read") {
			t.Errorf("%s must read %s and not %s", role, want, other)
		}
		if owner, _ := pm.RowScopeFor([]string{role}, "module.pos.core.query."+want, "read"); owner != (role == "Cashier") {
			t.Errorf("%s %s if_owner = %v", role, want, owner)
		}
	}
	// my_registers lists the pack's registers for everyone: not owner-scoped.
	for _, role := range []string{"Cashier", "Manager", "Owner"} {
		if owner, _ := pm.RowScopeFor([]string{role}, "module.pos.core.query.my_registers", "read"); owner {
			t.Errorf("%s my_registers grant must not be if_owner", role)
		}
	}
	// sale_lines is a plain read grant for every till role: a return starts from a receipt (#329).
	for _, role := range []string{"Cashier", "Manager", "Owner"} {
		if owner, _ := pm.RowScopeFor([]string{role}, "module.pos.core.query.sale_lines", "read"); owner {
			t.Errorf("%s sale_lines grant must not be if_owner", role)
		}
	}
	grant("Cashier", "pos.core", "query", []string{"recent_sales", "today_totals", "payments_by_method", "refunds_by_method", "sale_items", "sale_payments"}, "read", false)
	// Sale detail (#91): Owner/Manager read items and payments of any sale and
	// keep recent_sales; only the Cashier holds my_sales, if_owner. Nobody but
	// the Cashier gets my_sales, so the catalog shows each role one sales list.
	for _, role := range []string{"Cashier", "Manager", "Owner"} {
		wantMy := role == "Cashier"
		if pm.Can(role, "module.pos.core.query.my_sales", "read") != wantMy || pm.Can(role, "module.pos.core.query.recent_sales", "read") == wantMy {
			t.Errorf("%s sales list: my_sales=%v recent_sales=%v", role, pm.Can(role, "module.pos.core.query.my_sales", "read"), pm.Can(role, "module.pos.core.query.recent_sales", "read"))
		}
		if owner, _ := pm.RowScopeFor([]string{role}, "module.pos.core.query.my_sales", "read"); owner != wantMy {
			t.Errorf("%s my_sales if_owner = %v, want %v", role, owner, wantMy)
		}
		for _, q := range []string{"sale_items", "sale_payments"} {
			if owner, _ := pm.RowScopeFor([]string{role}, "module.pos.core.query."+q, "read"); owner {
				t.Errorf("%s %s must not be if_owner", role, q)
			}
		}
	}
	// Cashier holds no read on the Sale children themselves.
	for _, dt := range []string{"Sale Item", "Sale Payment"} {
		if pm.Can("Cashier", dt, "read") {
			t.Errorf("Cashier must not read %s", dt)
		}
	}
	grant("Cashier", "inventory.core", "query", []string{"item_search", "item_categories"}, "read", true)
	grant("Cashier", "inventory.core", "query", []string{"item_stock", "low_stock"}, "read", false)
	grant("Cashier", "inventory.core", "operation", invOps, "write", false)
	grant("Inventory Manager", "inventory.core", "operation", invOps, "write", true)
	grant("Inventory Manager", "inventory.core", "query", invQ, "read", true)
	grant("Inventory Manager", "pos.core", "operation", posOps, "write", false)
	for _, n := range []string{"Cashier", "Manager", "Owner"} {
		if pm.GetRole(n) == nil {
			t.Errorf("missing role %s", n)
		}
	}
}

func TestPOSPackOwnershipAndItemSearchHidesCost(t *testing.T) {
	for _, tc := range []struct{ file, owner, mode string }{
		{"pos/doctypes/sale.yaml", "pos.core", "commands_only"},
		{"pos/doctypes/sale_item.yaml", "pos.core", "commands_only"},
		{"pos/doctypes/sale_payment.yaml", "pos.core", "commands_only"},
		{"pos/doctypes/sale_line_balance.yaml", "pos.core", "commands_only"},
		{"pos/doctypes/shift.yaml", "pos.core", "commands_only"},
		{"pos/doctypes/till_session.yaml", "pos.core", "commands_only"},
		{"inventory/doctypes/item.yaml", "inventory.core", "crud"},
		// #406: owned like Item, so the item_categories query grant covers its source.
		{"inventory/doctypes/item_category.yaml", "inventory.core", "crud"},
		{"pos/doctypes/register.yaml", "", ""},
		{"inventory/doctypes/stock_balance.yaml", "", ""},
		{"inventory/doctypes/stock_entry.yaml", "", ""},
		{"inventory/doctypes/location.yaml", "", ""},
	} {
		body, err := os.ReadFile(filepath.Join("..", "config", tc.file))
		if err != nil {
			t.Fatal(err)
		}
		var dt DocType
		if err := yaml.Unmarshal(body, &dt); err != nil {
			t.Fatalf("%s: %v", tc.file, err)
		}
		access := string(dt.ModuleAccess)
		wantAccess := ""
		if tc.owner != "" {
			wantAccess = "published"
		}
		if dt.ModuleOwnerID != tc.owner || dt.ModuleOwnershipMode != tc.mode || access != wantAccess {
			t.Errorf("%s: owner=%q mode=%q access=%q, want %q %q %q", tc.file, dt.ModuleOwnerID, dt.ModuleOwnershipMode, access, tc.owner, tc.mode, wantAccess)
		}
	}
	body, err := os.ReadFile(filepath.Join("..", "config", "inventory", "queries", "item_search.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "cost") {
		t.Error("item_search must never expose cost")
	}
}

// #406: a published query's grant covers its source only when the source is
// owned by the query's module (D7); otherwise the role also needs a read on
// the source. Every role granted a pack query must satisfy one of the two, so
// no grant is silently dead (the Cashier's item_categories was).
func TestPackQueryGrantsAreServableBySourceOwnershipOrRead(t *testing.T) {
	cfg := filepath.Join("..", "config")
	pm := NewPermissionMatrix()
	for _, pack := range []string{"pos", "inventory"} {
		roles, err := ParseRolesFile(filepath.Join(cfg, pack, "roles.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range roles {
			pm.AddRole(r)
		}
		perms, err := ParsePermissionsFile(filepath.Join(cfg, pack, "permissions.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range perms {
			pm.SetPermission(p)
		}
	}
	docs := map[string]DocType{}
	parentOf := map[string]string{}
	for _, pack := range []string{"pos", "inventory"} {
		files, _ := filepath.Glob(filepath.Join(cfg, pack, "doctypes", "*.yaml"))
		for _, f := range files {
			body, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			var dt DocType
			if err := yaml.Unmarshal(body, &dt); err != nil {
				t.Fatalf("%s: %v", f, err)
			}
			docs[dt.Name] = dt
			for _, fd := range dt.Fields {
				if fd.Fieldtype == "Table" {
					parentOf[fd.Options] = dt.Name
				}
			}
		}
	}
	for _, pack := range []string{"pos", "inventory"} {
		files, _ := filepath.Glob(filepath.Join(cfg, pack, "queries", "*.yaml"))
		for _, f := range files {
			body, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			var q struct {
				Name          string `yaml:"name"`
				ModuleOwnerID string `yaml:"module_owner_id"`
				Source        string `yaml:"source"`
			}
			if err := yaml.Unmarshal(body, &q); err != nil {
				t.Fatalf("%s: %v", f, err)
			}
			source := q.Source
			if p := parentOf[source]; p != "" {
				source = p
			}
			resource := "module." + q.ModuleOwnerID + ".query." + q.Name
			for _, role := range []string{"Cashier", "Manager", "Owner", "Inventory Manager"} {
				if !pm.Can(role, resource, "read") {
					continue
				}
				if docs[source].ModuleOwnerID == q.ModuleOwnerID || pm.Can(role, source, "read") {
					continue
				}
				t.Errorf("%s holds %s but %s is neither owned by %s nor readable by the role: the query would be denied", role, resource, source, q.ModuleOwnerID)
			}
		}
	}
}
