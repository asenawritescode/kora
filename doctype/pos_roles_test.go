package doctype

import (
	"path/filepath"
	"testing"
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
		{"Cashier", "Sale", "create", true},
		{"Cashier", "Sale", "delete", false},
		{"Cashier", "Stock Entry", "create", false},
		{"Cashier", "Stock Balance", "write", false},
		{"Cashier", "Item", "read", false}, // Item carries cost
		{"Manager", "Sale", "write", true},
		{"Manager", "Sale", "delete", false},
		{"Owner", "Sale", "delete", true},
		{"Owner", "Item", "read", true},
		{"Owner", "Item", "create", true},
		{"Owner", "Location", "create", true},
		{"Owner", "Stock Balance", "create", true},
		{"Owner", "Stock Entry", "create", true},
	}
	for _, tc := range tests {
		if got := pm.Can(tc.role, tc.dt, tc.op); got != tc.want {
			t.Errorf("%s %s %s = %v, want %v", tc.role, tc.op, tc.dt, got, tc.want)
		}
	}
	posOps := []string{"open_shift", "close_shift", "open_till", "close_till", "record_sale", "return_sale", "void_sale"}
	invOps := []string{"receive_stock", "adjust_stock"}
	posQ := []string{"open_till", "payments_by_method", "recent_sales", "today_totals"}
	invQ := []string{"item_search", "item_stock", "low_stock"}
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
	grant("Cashier", "pos.core", "query", []string{"open_till"}, "read", true)
	grant("Cashier", "pos.core", "query", []string{"recent_sales", "today_totals", "payments_by_method"}, "read", false)
	grant("Cashier", "inventory.core", "query", []string{"item_search"}, "read", true)
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
