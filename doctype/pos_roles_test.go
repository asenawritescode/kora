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
	}
	for _, tc := range tests {
		if got := pm.Can(tc.role, tc.dt, tc.op); got != tc.want {
			t.Errorf("%s %s %s = %v, want %v", tc.role, tc.op, tc.dt, got, tc.want)
		}
	}
	for _, n := range []string{"Cashier", "Manager", "Owner"} {
		if pm.GetRole(n) == nil {
			t.Errorf("missing role %s", n)
		}
	}
}
