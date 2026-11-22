package doctype

import (
	"strings"
	"testing"
)

func TestParseRolesYAMLInsightsAccess(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr bool
		want    *bool // nil when the role does not say
	}{
		{name: "absent keeps insights", yaml: "- name: Manager\n  workspace_access: true\n", want: nil},
		{name: "false removes insights", yaml: "- name: Cashier\n  workspace_access: true\n  insights_access: false\n", want: boolPtr(false)},
		{name: "true grants insights", yaml: "- name: Owner\n  insights_access: true\n", want: boolPtr(true)},
		{name: "unknown key is rejected", yaml: "- name: Cashier\n  insight_access: false\n", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			roles, err := ParseRolesYAML([]byte(tc.yaml))
			if tc.wantErr {
				if err == nil {
					t.Fatal("ParseRolesYAML accepted an unknown key")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got := roles[0].InsightsAccess
			if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
				t.Fatalf("insights_access = %v, want %v", ptrString(got), ptrString(tc.want))
			}
		})
	}
}

func TestPermissionMatrixInsightsAllowed(t *testing.T) {
	pm := NewPermissionMatrix()
	pm.AddRole(&Role{Name: "Cashier", InsightsAccess: boolPtr(false)})
	pm.AddRole(&Role{Name: "Manager"})
	pm.AddRole(&Role{Name: "Clerk", InsightsAccess: boolPtr(false)})
	tests := []struct {
		name  string
		roles []string
		want  bool
	}{
		{name: "cashier alone is refused", roles: []string{"Cashier"}, want: false},
		{name: "manager is allowed", roles: []string{"Manager"}, want: true},
		{name: "cashier with manager is allowed", roles: []string{"Cashier", "Manager"}, want: true},
		{name: "admin is always allowed", roles: []string{"Cashier", AdminRole}, want: true},
		{name: "undefined role keeps insights", roles: []string{"Auditor"}, want: true},
		{name: "no roles is not denied by a role flag", roles: nil, want: true},
		{name: "empty roles is not denied by a role flag", roles: []string{}, want: true},
		{name: "two denied roles are refused", roles: []string{"Cashier", "Clerk"}, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := pm.InsightsAllowed(tc.roles); got != tc.want {
				t.Fatalf("InsightsAllowed(%v) = %v, want %v", tc.roles, got, tc.want)
			}
		})
	}
}

func boolPtr(b bool) *bool { return &b }

func ptrString(b *bool) string {
	if b == nil {
		return "unset"
	}
	if *b {
		return "true"
	}
	return "false"
}

func TestRoleInsightsSExprIsCanonical(t *testing.T) {
	tests := []struct {
		name string
		role *Role
		want string
	}{
		{name: "unset and explicit true share one form", role: &Role{Name: "Manager"}, want: "(role Manager)"},
		{name: "explicit true is not written", role: &Role{Name: "Manager", InsightsAccess: boolPtr(true)}, want: "(role Manager)"},
		{name: "denied role is written", role: &Role{Name: "Cashier", WorkspaceAccess: true, InsightsAccess: boolPtr(false)}, want: "(role Cashier\n  :insights false  :workspace true)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var b strings.Builder
			writeSExprRole(&b, tc.role, "")
			if got := strings.TrimPrefix(b.String(), "\n"); got != tc.want {
				t.Fatalf("writeSExprRole = %q, want %q", got, tc.want)
			}
		})
	}
}
