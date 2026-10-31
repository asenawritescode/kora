package doctype

import (
	"strings"
	"sync"
	"testing"
)

func TestPermissionMatrixDigestIsStableAndTracksAtomicReplacement(t *testing.T) {
	matrix := NewPermissionMatrix()
	matrix.LoadPermissionsFromDB(
		[]*Role{{Name: "Viewer"}, {Name: "Editor"}},
		[]*Permission{{Role: "Viewer", Doctype: "Invoice", Read: true}, {Role: "Editor", Doctype: "Invoice", Read: true, Write: true}},
	)
	first := matrix.PolicyDigest()
	if first == "" {
		t.Fatal("policy digest is empty")
	}
	matrix.LoadPermissionsFromDB(
		[]*Role{{Name: "Editor"}, {Name: "Viewer"}},
		[]*Permission{{Role: "Editor", Doctype: "Invoice", Read: true, Write: true}, {Role: "Viewer", Doctype: "Invoice", Read: true}},
	)
	if got := matrix.PolicyDigest(); got != first {
		t.Fatalf("digest changed with input ordering: got %s want %s", got, first)
	}
	matrix.LoadPermissionsFromDB(
		[]*Role{{Name: "Viewer"}},
		[]*Permission{{Role: "Viewer", Doctype: "Invoice", Read: false}},
	)
	if got := matrix.PolicyDigest(); got == first {
		t.Fatal("digest did not change with policy replacement")
	}
	if matrix.GetRole("Editor") != nil {
		t.Fatal("atomic policy replacement retained a removed role")
	}
}

func TestPermissionAssignmentScopeIsOperationSpecificAndCopied(t *testing.T) {
	matrix := NewPermissionMatrix()
	fields := []string{"assignee_user", "assigned_to"}
	permission := &Permission{Role: "Rep", Doctype: "Lead", Read: true, IfOwner: true, IfAssigned: fields}
	matrix.SetPermission(permission)
	fields[0] = "untrusted"
	permission.IfAssigned[1] = "also_untrusted"
	owner, assigned := matrix.RowScopeFor([]string{"Rep"}, "Lead", "read")
	if !owner || len(assigned) != 2 || assigned[0] != "assigned_to" || assigned[1] != "assignee_user" {
		t.Fatalf("read scope = (%v, %#v), want owner plus copied assignment fields", owner, assigned)
	}
	owner, assigned = matrix.RowScopeFor([]string{"Rep"}, "Lead", "write")
	if owner || len(assigned) != 0 {
		t.Fatalf("unauthorized operation acquired row scope = (%v, %#v)", owner, assigned)
	}
	serialized, err := matrix.Serialize()
	if err != nil || !strings.Contains(string(serialized), `"if_assigned":["assignee_user","assigned_to"]`) {
		t.Fatalf("serialized assignment policy = %s, %v", serialized, err)
	}
}

func TestParsePermissionsSupportsLegacyKeysAndAssignmentScope(t *testing.T) {
	permissions, err := ParsePermissionsYAML([]byte(`- doctype: Lead
  role: Sales Rep
  can_read: true
  can_write: true
  if_owner: true
  if_assigned: [assignee_user]
`))
	if err != nil || len(permissions) != 1 {
		t.Fatalf("parse legacy permissions = %#v, %v", permissions, err)
	}
	if !permissions[0].Read || !permissions[0].Write || len(permissions[0].IfAssigned) != 1 || permissions[0].IfAssigned[0] != "assignee_user" {
		t.Fatalf("legacy permission fields were lost: %#v", permissions[0])
	}
	if _, err := ParsePermissionsYAML([]byte(`- doctype: Lead
  role: Rep
  read: true
  can_read: false
`)); err == nil {
		t.Fatal("conflicting permission aliases were accepted")
	}
}

func TestConfigSnapshotRoundTripsAssignmentScopeDeterministically(t *testing.T) {
	snapshot := &ConfigSnapshot{Permissions: []*Permission{{
		Doctype: "Lead",
		Role:    "Sales Rep",
		Read:    true,
		IfOwner: true,
		IfAssigned: []string{
			"assignee_user",
			"owner_user",
		},
	}}}

	encoded := ToSExpr(snapshot)
	decoded, err := ParseConfig(encoded)
	if err != nil {
		t.Fatalf("parse serialized config snapshot: %v", err)
	}
	if len(decoded.Permissions) != 1 {
		t.Fatalf("decoded permissions = %#v, want one permission", decoded.Permissions)
	}
	permission := decoded.Permissions[0]
	if permission.Doctype != "Lead" || permission.Role != "Sales Rep" || !permission.Read || !permission.IfOwner {
		t.Fatalf("permission identity/capabilities changed in round trip: %#v", permission)
	}
	if strings.Join(permission.IfAssigned, ",") != "assignee_user,owner_user" {
		t.Fatalf("assignment scope changed in round trip: %#v", permission.IfAssigned)
	}
	if second := ToSExpr(decoded); second != encoded {
		t.Fatalf("config serialization is not canonical after round trip:\nfirst: %s\nsecond: %s", encoded, second)
	}
}

func TestPermissionMatrixConcurrentPolicyReplacementAndReads(t *testing.T) {
	matrix := NewPermissionMatrix()
	roles := []*Role{{Name: "Reader"}}
	permissions := []*Permission{{Role: "Reader", Doctype: "Invoice", Read: true}}
	matrix.LoadPermissionsFromDB(roles, permissions)
	var workers sync.WaitGroup
	for range 4 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for range 200 {
				_ = matrix.PolicyDigest()
				_, _ = matrix.UserCan([]string{"Reader"}, "Invoice", "read")
				_ = matrix.AllRoles()
				_ = matrix.AllPermissions()
			}
		}()
	}
	for index := range 200 {
		permissions := []*Permission{{Role: "Reader", Doctype: "Invoice", Read: index%2 == 0}}
		matrix.LoadPermissionsFromDB(roles, permissions)
	}
	workers.Wait()
}
