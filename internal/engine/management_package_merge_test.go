package engine

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/asenawritescode/kora/contract"
	"github.com/asenawritescode/kora/doctype"
)

func TestMergeManagementPackageSnapshotPreservesAndUpsertsTenantConfig(t *testing.T) {
	current := &doctype.ConfigSnapshot{
		DocTypes:      []*doctype.DocType{{Name: "Customer"}, {Name: "Stock"}},
		Roles:         []*doctype.Role{{Name: "Operator"}, {Name: "Viewer"}},
		Permissions:   []*doctype.Permission{{Doctype: "Customer", Role: "Viewer", Read: true}},
		Views:         []*doctype.View{{Name: "Customers", Route: "/customers"}},
		Reports:       []json.RawMessage{json.RawMessage(`{"name":"Existing report"}`)},
		Scripts:       []*doctype.ScriptSnapshot{{Name: "old_script", ScriptHash: "old-hash", Source: "return false;"}},
		ScheduledJobs: []contract.ScheduledJobDefinition{{Name: "old_job", Type: "custom", Schedule: "0 8 * * *"}},
	}
	incoming := &doctype.ConfigSnapshot{
		DocTypes:      []*doctype.DocType{{Name: "Stock", Module: "Retail"}, {Name: "Invoice"}},
		Roles:         []*doctype.Role{{Name: "Operator", Description: "Package managed"}},
		Permissions:   []*doctype.Permission{{Doctype: "Customer", Role: "Viewer", Read: false}, {Doctype: "Invoice", Role: "Operator", Create: true}},
		Views:         []*doctype.View{{Name: "Customers", Route: "/customers", Label: "Package label"}, {Name: "Invoices", Route: "/invoices"}},
		Scripts:       []*doctype.ScriptSnapshot{{Name: "new_script", ScriptHash: "new-hash", Source: "return true;"}},
		ScheduledJobs: []contract.ScheduledJobDefinition{{Name: "invoice_job", Type: "invoice", Schedule: "0 0 * * *"}},
	}
	merged, err := mergeManagementPackageSnapshot(current, incoming)
	if err != nil {
		t.Fatalf("mergeManagementPackageSnapshot() error = %v", err)
	}
	if len(merged.DocTypes) != 3 || merged.DocTypes[1].Name != "Invoice" || merged.DocTypes[2].Name != "Stock" || merged.DocTypes[2].Module != "Retail" {
		t.Fatalf("merged doctypes = %#v", merged.DocTypes)
	}
	if len(merged.Roles) != 2 || merged.Roles[0].Name != "Operator" || merged.Roles[0].Description != "Package managed" {
		t.Fatalf("merged roles = %#v", merged.Roles)
	}
	if len(merged.Permissions) != 2 || merged.Permissions[1].Doctype != "Invoice" || merged.Permissions[0].Read {
		t.Fatalf("merged permissions = %#v", merged.Permissions)
	}
	if len(merged.Views) != 2 || merged.Views[0].Label != "Package label" || merged.Views[1].Name != "Invoices" {
		t.Fatalf("merged views = %#v", merged.Views)
	}
	if len(merged.Reports) != 1 || string(merged.Reports[0]) != string(current.Reports[0]) {
		t.Fatalf("reports were not preserved: %#v", merged.Reports)
	}
	if len(merged.Scripts) != 2 || merged.Scripts[0].Name != "new_script" || merged.Scripts[1].Name != "old_script" {
		t.Fatalf("merged scripts = %#v", merged.Scripts)
	}
	if len(merged.ScheduledJobs) != 2 || merged.ScheduledJobs[0].Name != "invoice_job" || merged.ScheduledJobs[1].Name != "old_job" {
		t.Fatalf("merged scheduled jobs = %#v", merged.ScheduledJobs)
	}
	if current.DocTypes[1].Module != "" || current.Views[0].Label != "" {
		t.Fatal("merge mutated the current snapshot")
	}
}

func TestMergeManagementPackageSnapshotPreservesExplicitAssignmentScope(t *testing.T) {
	current := &doctype.ConfigSnapshot{
		DocTypes: []*doctype.DocType{{Name: "Lead", Fields: []doctype.Field{{Fieldname: "assignee_user", Fieldtype: "User"}}}},
		Roles:    []*doctype.Role{{Name: "Sales Rep"}},
		Permissions: []*doctype.Permission{{
			Doctype: "Lead", Role: "Sales Rep", Read: true, IfOwner: true,
		}},
	}
	incoming := &doctype.ConfigSnapshot{
		Permissions: []*doctype.Permission{{
			Doctype: "Lead", Role: "Sales Rep", Read: true, IfOwner: true,
			IfAssigned: []string{"assignee_user"},
		}},
	}

	merged, err := mergeManagementPackageSnapshot(current, incoming)
	if err != nil {
		t.Fatalf("merge package assignment policy: %v", err)
	}
	encoded := doctype.ToSExpr(merged)
	restored, err := doctype.ParseConfig(encoded)
	if err != nil {
		t.Fatalf("restore versioned package snapshot: %v", err)
	}
	var lead *doctype.Permission
	for _, permission := range restored.Permissions {
		if permission.Doctype == "Lead" && permission.Role == "Sales Rep" {
			lead = permission
		}
	}
	if lead == nil || !lead.IfOwner || !lead.Read || !strings.Contains(strings.Join(lead.IfAssigned, ","), "assignee_user") {
		t.Fatalf("versioned package permission lost explicit assignment scope: %#v", lead)
	}
	matrix := doctype.NewPermissionMatrix()
	matrix.LoadPermissionsFromDB(restored.Roles, restored.Permissions)
	owner, fields := matrix.RowScopeFor([]string{"Sales Rep"}, "Lead", "read")
	if !owner || len(fields) != 1 || fields[0] != "assignee_user" {
		t.Fatalf("restored CRM row scope = (%t, %#v), want creator OR assigned user", owner, fields)
	}
}

func TestMergeManagementPackageSnapshotRejectsViewRouteCollision(t *testing.T) {
	_, err := mergeManagementPackageSnapshot(
		&doctype.ConfigSnapshot{Views: []*doctype.View{{Name: "Existing", Route: "/shared"}}},
		&doctype.ConfigSnapshot{Views: []*doctype.View{{Name: "Incoming", Route: "/shared"}}},
	)
	if err == nil {
		t.Fatal("accepted a package route that collides with a different tenant view")
	}
}

func TestMergeManagementPackageSnapshotRejectsUnownedReplacementOfOwnedResource(t *testing.T) {
	owned := contract.ModuleDescriptor{
		ID: "finance.ar", Version: "1.0.0",
		Resources: []contract.OwnedResource{{
			Ref:    contract.ResourceRef{Namespace: "tenant", Name: "Invoice", Version: 1},
			Access: contract.ModulePrivate, MutationMode: contract.MutationCommands,
			ScopeKind: contract.ScopeTenantShared,
		}},
	}
	_, err := mergeManagementPackageSnapshot(
		&doctype.ConfigSnapshot{Modules: []contract.ModuleDescriptor{owned}, DocTypes: []*doctype.DocType{{Name: "Invoice", ModuleOwnerID: "finance.ar"}}},
		&doctype.ConfigSnapshot{DocTypes: []*doctype.DocType{{Name: "Invoice", Module: "Other package"}}},
	)
	if err == nil || !strings.Contains(err.Error(), `owned by module "finance.ar"`) {
		t.Fatalf("unowned package replacement error = %v; want owner conflict", err)
	}
}

func TestMergeManagementPackageSnapshotRequiresVersionChangeForModuleDefinitionUpdate(t *testing.T) {
	current := contract.ModuleDescriptor{ID: "finance.ar", Version: "1.0.0"}
	changed := contract.ModuleDescriptor{
		ID: "finance.ar", Version: "1.0.0",
		Resources: []contract.OwnedResource{{
			Ref:    contract.ResourceRef{Namespace: "tenant", Name: "Invoice", Version: 1},
			Access: contract.ModulePrivate, MutationMode: contract.MutationCommands,
			ScopeKind: contract.ScopeTenantShared,
		}},
	}
	_, err := mergeManagementPackageSnapshot(
		&doctype.ConfigSnapshot{Modules: []contract.ModuleDescriptor{current}},
		&doctype.ConfigSnapshot{Modules: []contract.ModuleDescriptor{changed}},
	)
	if err == nil || !strings.Contains(err.Error(), "cannot change without an authorized version update") {
		t.Fatalf("same-version module update error = %v; want immutable version rejection", err)
	}
}

func TestPackageOwnershipRecognizesNonDocTypeResources(t *testing.T) {
	snapshot := &doctype.ConfigSnapshot{
		Scripts:       []*doctype.ScriptSnapshot{{Name: "invoice.post"}},
		ScheduledJobs: []contract.ScheduledJobDefinition{{Name: "invoice.due"}},
	}
	for _, resource := range []contract.OwnedResource{
		{Ref: contract.ResourceRef{Namespace: "script", Name: "invoice.post", Version: 1}},
		{Ref: contract.ResourceRef{Namespace: "job", Name: "invoice.due", Version: 1}},
	} {
		found, err := bindOwnedPackageResource(snapshot, "finance.ar", resource)
		if err != nil || !found {
			t.Fatalf("bind %s: found=%t err=%v", resource.Ref, found, err)
		}
	}
}
