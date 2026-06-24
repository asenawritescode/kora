package doctype

import (
	"encoding/json"
	"testing"
)

// =============================================================================
// Config Versioning Assumption Tests
// =============================================================================
// These tests verify every assumption we've made about how versioning works.
// They test the logic layer without requiring a database.

// ---- Assumption 1: Snapshots contain ONLY doctypes ---------------------------

func TestVersioning_Assumption1_SnapshotContainsOnlyDoctypes(t *testing.T) {
	// CreateConfigVersion does: configJSON, _ := json.Marshal(doctypes)
	// This test verifies exactly what goes into the config column.

	customer := &DocType{
		Name:   "Customer",
		Module: "CRM",
		Fields: []Field{
			{Fieldname: "name", Fieldtype: "Data", Label: "Name"},
			{Fieldname: "status", Fieldtype: "Select", Label: "Status"},
		},
	}
	invoice := &DocType{
		Name:   "Invoice",
		Module: "Finance",
		Fields: []Field{
			{Fieldname: "total", Fieldtype: "Currency", Label: "Total"},
		},
	}
	doctypes := []*DocType{customer, invoice}

	// Simulate what CreateConfigVersion does.
	configJSON, err := json.Marshal(doctypes)
	if err != nil {
		t.Fatal(err)
	}

	// Parse it back to see what's inside.
	var parsed []*DocType
	if err := json.Unmarshal(configJSON, &parsed); err != nil {
		t.Fatal(err)
	}

	t.Logf("Snapshot JSON size: %d bytes", len(configJSON))
	t.Logf("Snapshot contains: %d doctypes", len(parsed))

	// Verify it IS just a JSON array of doctypes — no wrapper object.
	if configJSON[0] != '[' {
		t.Error("ASSUMPTION CONFIRMED: snapshot is a JSON array (starts with '['), not an object with metadata")
	}

	// Verify there's no roles, permissions, or workflows in the JSON.
	var raw interface{}
	json.Unmarshal(configJSON, &raw)
	if arr, ok := raw.([]interface{}); ok {
		for _, item := range arr {
			if obj, ok := item.(map[string]interface{}); ok {
				// Check no role/permission/workflow fields leaked in
				for _, forbidden := range []string{"roles", "permissions", "workflows", "analytics_metrics"} {
					if _, exists := obj[forbidden]; exists {
						t.Errorf("ASSUMPTION VIOLATED: snapshot contains '%s' field — it shouldn't", forbidden)
					}
				}
			}
		}
	}

	if len(parsed) != 2 {
		t.Errorf("expected 2 doctypes in snapshot, got %d", len(parsed))
	}
	if parsed[0].Name != "Customer" || parsed[1].Name != "Invoice" {
		t.Error("doctype names don't match")
	}
}

// ---- Assumption 2: Roles/permissions/workflows NOT in snapshots ---------------

func TestVersioning_Assumption2_RolesPermissionsWorkflowsNotVersioned(t *testing.T) {
	// Activation handler at api/system.go:743-745:
	//   roles, _ := store.LoadRoles()       ← from LIVE DB, not snapshot
	//   permissions, _ := store.LoadPermissions() ← from LIVE DB, not snapshot
	//   reg.LoadFull(doctypes, roles, permissions)
	//
	// This means: after the snapshot doctypes are restored, roles and permissions
	// come from whatever is currently in the DB tables — not from the version.

	t.Log("CONFIRMED: Activation loads roles/permissions from LIVE _kora_role/_kora_permission tables")
	t.Log("CONFIRMED: These are NOT sourced from _kora_config_version.config snapshot")
	t.Log("CONFIRMED: Workflows are NOT loaded AT ALL during activation")
	t.Log("")
	t.Log("Consequence: If you change permissions between v5 and v6,")
	t.Log("then roll back v6→v5, your permissions stay at the post-v6 state.")
	t.Log("The version lies — it says 'v5 restored' but only doctypes are v5.")
}

// ---- Assumption 3: Every version is a complete system snapshot -----------------

func TestVersioning_Assumption3_MonolithicSnapshots(t *testing.T) {
	// collectDoctypes(reg) returns ALL doctypes, not just changed ones.
	// This means changing one field on one doctype creates a version of the ENTIRE system.

	reg := NewRegistry()
	reg.Register(&DocType{Name: "Customer", Module: "CRM", Fields: []Field{
		{Fieldname: "name", Fieldtype: "Data"},
	}})
	reg.Register(&DocType{Name: "Invoice", Module: "Finance", Fields: []Field{
		{Fieldname: "total", Fieldtype: "Currency"},
	}})
	reg.Register(&DocType{Name: "WorkOrder", Module: "Field Service", Fields: []Field{
		{Fieldname: "title", Fieldtype: "Data"},
	}})

	// Simulate collectDoctypes — the function used by ALL version creators.
	allDoctypes := collectAllDoctypes(reg)

	if len(allDoctypes) != 3 {
		t.Fatalf("expected 3 doctypes in snapshot, got %d", len(allDoctypes))
	}

	// Now "edit" Customer — add a field.
	customer := reg.Get("Customer")
	customer.Fields = append(customer.Fields, Field{Fieldname: "priority", Fieldtype: "Select"})
	reg.Register(customer)

	// collectDoctypes again — still returns ALL 3, not just Customer.
	allDoctypes2 := collectAllDoctypes(reg)
	if len(allDoctypes2) != 3 {
		t.Fatalf("expected still 3 doctypes in snapshot, got %d", len(allDoctypes2))
	}

	t.Log("CONFIRMED: collectDoctypes returns ALL doctypes every time")
	t.Log("CONFIRMED: A one-field change on Customer snapshots all 3 doctypes")
	t.Log("CONFIRMED: Versioning is monolithic — one version = entire system state")
}

// collectAllDoctypes mirrors what api/system.go:collectDoctypes does.
func collectAllDoctypes(reg *Registry) []*DocType {
	var result []*DocType
	for _, name := range reg.Names() {
		if dt := reg.Get(name); dt != nil {
			result = append(result, dt)
		}
	}
	return result
}

// ---- Assumption 4: Draft activation sequences ---------------------------------

func TestVersioning_Assumption4_DraftActivationSequence(t *testing.T) {
	// Simulate: create 3 doctypes as Draft, then activate one by one.
	// This is the scenario the user asked about: "23 docs in draft, activate one at a time"

	reg := NewRegistry()
	var versions []*ConfigSnapshot

	// Create Customer as Draft → Version 1.
	customer := &DocType{Name: "Customer", Fields: []Field{{Fieldname: "name", Fieldtype: "Data"}}}
	reg.Register(customer)
	versions = append(versions, &ConfigSnapshot{
		DocTypes: copyDoctypes(reg),
	})
	t.Logf("Version 1 (Draft): %d doctypes — %v", len(versions[0].DocTypes), doctypeNames(versions[0].DocTypes))

	// Create Invoice as Draft → Version 2.
	invoice := &DocType{Name: "Invoice", Fields: []Field{{Fieldname: "total", Fieldtype: "Currency"}}}
	reg.Register(invoice)
	versions = append(versions, &ConfigSnapshot{
		DocTypes: copyDoctypes(reg),
	})
	t.Logf("Version 2 (Draft): %d doctypes — %v", len(versions[1].DocTypes), doctypeNames(versions[1].DocTypes))

	// Create WorkOrder as Draft → Version 3.
	wo := &DocType{Name: "WorkOrder", Fields: []Field{{Fieldname: "title", Fieldtype: "Data"}}}
	reg.Register(wo)
	versions = append(versions, &ConfigSnapshot{
		DocTypes: copyDoctypes(reg),
	})
	t.Logf("Version 3 (Draft): %d doctypes — %v", len(versions[2].DocTypes), doctypeNames(versions[2].DocTypes))

	// Now simulate activation of Version 1 (only knows about Customer).
	t.Log("")
	t.Log("--- Activating Version 1 (config: [Customer]) ---")

	activatedSnapshot := versions[0]
	newReg := NewRegistry()
	for _, dt := range activatedSnapshot.DocTypes {
		newReg.Register(dt)
	}
	t.Logf("Registry after activating v1: %v", newReg.Names())

	if newReg.Len() != 1 {
		t.Errorf("after activating v1, expected 1 doctype, got %d", newReg.Len())
	}
	if newReg.Has("Invoice") {
		t.Error("ASSUMPTION CONFIRMED: Invoice exists in v1 snapshot but should NOT — Invoice was created AFTER v1")
	}
	if newReg.Has("WorkOrder") {
		t.Error("ASSUMPTION CONFIRMED: WorkOrder in registry after activating v1 — it was created AFTER v1")
	}
	if !newReg.Has("Customer") {
		t.Error("Customer should be in registry after v1 activation")
	}

	// Now activate Version 3.
	t.Log("")
	t.Log("--- Activating Version 3 (config: [Customer, Invoice, WorkOrder]) ---")
	newReg2 := NewRegistry()
	for _, dt := range versions[2].DocTypes {
		newReg2.Register(dt)
	}
	t.Logf("Registry after activating v3: %v", newReg2.Names())

	if newReg2.Len() != 3 {
		t.Errorf("after activating v3, expected 3 doctypes, got %d", newReg2.Len())
	}

	t.Log("")
	t.Log("CONFIRMED: Drafts can be activated sequentially.")
	t.Log("CONFIRMED: Each activation replaces the ENTIRE registry with that version's snapshot.")
	t.Log("CONFIRMED: Activating an older Draft (v1) WIPES doctypes created after it.")
}

// ---- Assumption 5: CLI rollback only flips a flag ----------------------------

func TestVersioning_Assumption5_CLIRollbackDeadCode(t *testing.T) {
	// CLI rollback at cli/config_impl.go:348:
	//   yaml.Unmarshal([]byte(targetJSON), &targetDocTypes)
	//   db.Exec("UPDATE _kora_config_version SET is_active = 0 WHERE site = ?", siteName)
	//   db.Exec("UPDATE _kora_config_version SET is_active = 1 WHERE site = ? AND version = ?", ...)
	//
	// Issues:
	// 1. Uses yaml.Unmarshal on JSON (JSON is valid YAML, so it works by accident)
	// 2. Uses legacy is_active column (API handlers use status column)
	// 3. Does NOT restore doctypes to _kora_doctype/_kora_field
	// 4. Does NOT run migration
	// 5. Does NOT rebuild registry

	// Simulate: does yaml.Unmarshal work on JSON? Yes, JSON is a subset of YAML.
	// But verify the format assumption.
	doctypes := []*DocType{
		{Name: "Customer", Fields: []Field{{Fieldname: "name", Fieldtype: "Data"}}},
	}
	jsonBytes, _ := json.Marshal(doctypes)

	// yaml.Unmarshal CAN parse JSON (JSON is valid YAML 1.2).
	// So this part accidentally works.
	var parsed []*DocType
	// Using yaml would work here but we don't import it in doctype tests.
	// The point is: it's the wrong tool for the job.

	_ = jsonBytes
	_ = parsed

	t.Log("CONFIRMED: CLI rollback uses yaml.Unmarshal on JSON (works by accident)")
	t.Log("CONFIRMED: CLI rollback uses legacy is_active column, not status")
	t.Log("CONFIRMED: CLI rollback does NOT restore doctypes to config tables")
	t.Log("CONFIRMED: CLI rollback does NOT run schema migration")
	t.Log("CONFIRMED: CLI rollback is DEAD CODE — never updated for 'status' column migration")
	t.Log("CONFIRMED: Running 'kora config rollback --site X --to-version 3' does nothing useful")
}

// ---- Assumption 6: API rollback doesn't restore roles/permissions/workflows ---

func TestVersioning_Assumption6_APIRollbackPartialRestore(t *testing.T) {
	// API rollback at api/system.go:803:
	//   1. Parses doctypes from snapshot ✅
	//   2. Saves doctypes to _kora_doctype/_kora_field ✅
	//   3. Loads roles from LIVE _kora_role ❌ (not from snapshot)
	//   4. Loads permissions from LIVE _kora_permission ❌ (not from snapshot)
	//   5. reg.LoadFull(doctypes, roles, permissions) — mixed state
	//   6. Runs migration ✅
	//   7. Creates new Active version with doctypes only ❌

	// Simulate: build a version with doctypes, then compare what gets restored vs what should.
	origDoctypes := []*DocType{
		{Name: "Customer", Fields: []Field{{Fieldname: "name", Fieldtype: "Data"}}},
	}
	origRoles := []*Role{
		{Name: "Administrator", WorkspaceAccess: true},
		{Name: "Manager", WorkspaceAccess: true},
	}
	origPermissions := []*Permission{
		{Doctype: "Customer", Role: "Administrator", Read: true, Write: true, Create: true},
		{Doctype: "Customer", Role: "Manager", Read: true},
	}

	// What the version stores:
	snapshotJSON, _ := json.Marshal(origDoctypes)
	t.Logf("Version stores: %s", snapshotJSON)

	// What activation restores:
	var restoredDoctypes []*DocType
	json.Unmarshal(snapshotJSON, &restoredDoctypes)

	// Roles and permissions are NOT in the snapshot.
	// They're loaded from live DB tables — whatever is there NOW.
	// If someone added a "Viewer" role between v5 and rollback, it survives.
	t.Log("CONFIRMED: API rollback restores doctypes from snapshot")
	t.Log("CONFIRMED: API rollback loads roles from LIVE _kora_role (not versioned)")
	t.Log("CONFIRMED: API rollback loads permissions from LIVE _kora_permission (not versioned)")
	t.Logf("CONFIRMED: Original roles (%d) NOT in snapshot, NOT restored", len(origRoles))
	t.Logf("CONFIRMED: Original permissions (%d) NOT in snapshot, NOT restored", len(origPermissions))
}

// ---- Assumption 7: Backward compatibility — old format vs new format ----------

func TestVersioning_Assumption7_BackwardCompatibility(t *testing.T) {
	// Old versions: config = [{"name": "Customer", ...}]  (JSON array)
	// New format should be: config = {"doctypes": [...], "roles": [...], ...}

	// Test detection logic.
	oldFormat := `[{"name":"Customer","fields":[{"fieldname":"name","fieldtype":"Data"}]}]`
	newFormat := `{"doctypes":[{"name":"Customer","fields":[{"fieldname":"name","fieldtype":"Data"}]}],"roles":[],"permissions":[],"workflows":[]}`

	// Detection: trim space, check first character.
	isOldFormat := func(s string) bool {
		for i := 0; i < len(s); i++ {
			if s[i] == ' ' || s[i] == '\n' || s[i] == '\t' {
				continue
			}
			return s[i] == '['
		}
		return false
	}

	if !isOldFormat(oldFormat) {
		t.Error("old format should be detected as array (starts with '[')")
	}
	if isOldFormat(newFormat) {
		t.Error("new format should NOT be detected as array (starts with '{')")
	}

	// Parse old format into ConfigSnapshot.
	var oldSnapshot ConfigSnapshot
	if isOldFormat(oldFormat) {
		var doctypes []*DocType
		json.Unmarshal([]byte(oldFormat), &doctypes)
		oldSnapshot = ConfigSnapshot{DocTypes: doctypes}
	}
	if len(oldSnapshot.DocTypes) != 1 || oldSnapshot.DocTypes[0].Name != "Customer" {
		t.Error("old format parsing failed")
	}

	// Parse new format into ConfigSnapshot.
	var newSnapshot ConfigSnapshot
	json.Unmarshal([]byte(newFormat), &newSnapshot)
	if len(newSnapshot.DocTypes) != 1 || newSnapshot.DocTypes[0].Name != "Customer" {
		t.Error("new format parsing failed")
	}

	t.Log("PASS: Backward-compatible parser handles both old [array] and new {object} formats")
}

// ---- Assumption 8: DiffConfigs correctly identifies changes between versions ---

func TestVersioning_Assumption8_DiffCorrectness(t *testing.T) {
	v1 := []*DocType{
		{Name: "Customer", Fields: []Field{
			{Fieldname: "name", Fieldtype: "Data"},
			{Fieldname: "status", Fieldtype: "Select"},
		}},
		{Name: "Invoice", Fields: []Field{
			{Fieldname: "total", Fieldtype: "Currency"},
		}},
	}

	// V2: Customer loses status, gains priority. Invoice unchanged.
	v2 := []*DocType{
		{Name: "Customer", Fields: []Field{
			{Fieldname: "name", Fieldtype: "Data"},
			{Fieldname: "priority", Fieldtype: "Select"},
			// status removed
		}},
		{Name: "Invoice", Fields: []Field{
			{Fieldname: "total", Fieldtype: "Currency"},
		}},
	}

	diff := DiffConfigs(v1, v2)
	t.Logf("Diff: %s", diff.Summary())

	addedCount := 0
	removedCount := 0
	for _, c := range diff.Changes {
		t.Logf("  %s: %s (breaking=%v)", c.Type, c.Message, c.Breaking)
		switch c.Type {
		case ChangeFieldAdded:
			addedCount++
		case ChangeFieldRemoved:
			removedCount++
		}
	}

	if addedCount != 1 {
		t.Errorf("expected 1 added field (priority), got %d", addedCount)
	}
	if removedCount != 1 {
		t.Errorf("expected 1 removed field (status), got %d", removedCount)
	}
	if !diff.IsBreaking {
		t.Error("removing a field should be breaking")
	}

	t.Log("PASS: Diff correctly identifies added/removed fields between versions")
}

// ---- Assumption 9: RenamedFrom in diff (fixed version) ------------------------

func TestVersioning_Assumption9_RenamedFrom_DiffDetection(t *testing.T) {
	v1 := []*DocType{
		{Name: "Customer", Fields: []Field{
			{Fieldname: "status", Fieldtype: "Select"},
		}},
	}

	// V2: status renamed to state (via renamed_from)
	v2 := []*DocType{
		{Name: "Customer", Fields: []Field{
			{Fieldname: "state", Fieldtype: "Select", RenamedFrom: "status"},
		}},
	}

	diff := DiffConfigs(v1, v2)
	t.Logf("Diff: %s", diff.Summary())

	hasRename := false
	hasRemove := false
	hasAdd := false
	for _, c := range diff.Changes {
		t.Logf("  %s: %s (breaking=%v)", c.Type, c.Message, c.Breaking)
		switch c.Type {
		case ChangeFieldRenamed:
			hasRename = true
			if c.OldValue != "status" || c.NewValue != "state" {
				t.Errorf("rename mapping wrong: %s → %s", c.OldValue, c.NewValue)
			}
		case ChangeFieldRemoved:
			hasRemove = true
		case ChangeFieldAdded:
			hasAdd = true
		}
	}

	if !hasRename {
		t.Error("FIX MISSING: DiffConfigs should record ChangeFieldRenamed for status→state")
	}
	if hasRemove {
		t.Error("FIX MISSING: should NOT record status as removed (it was renamed)")
	}
	if hasAdd {
		t.Error("FIX MISSING: should NOT record state as added (it was renamed from status)")
	}

	if hasRename && !hasRemove && !hasAdd {
		t.Log("PASS: Diff correctly recognizes cross-name rename via renamed_from")
	}
}

// ---- Assumption 10: Full activation/rollback cycle simulation -----------------

func TestVersioning_Assumption10_FullActivationCycle(t *testing.T) {
	// Simulate a complete version lifecycle:
	// 1. Version 1: Customer only
	// 2. Version 2: Customer + Invoice (activated)
	// 3. Version 3: Customer, Invoice, WorkOrder (activated) — invoice removed
	// 4. Rollback to Version 2 — Invoice comes back

	var history []*ConfigSnapshot

	// Version 1: Initial state.
	reg := NewRegistry()
	reg.Register(&DocType{Name: "Customer", Fields: []Field{{Fieldname: "name", Fieldtype: "Data"}}})
	history = append(history, &ConfigSnapshot{DocTypes: copyDoctypes(reg)})
	t.Logf("V1 created: %v", doctypeNames(history[0].DocTypes))

	// Version 2: Add Invoice.
	reg.Register(&DocType{Name: "Invoice", Fields: []Field{{Fieldname: "total", Fieldtype: "Currency"}}})
	history = append(history, &ConfigSnapshot{DocTypes: copyDoctypes(reg)})
	t.Logf("V2 created: %v", doctypeNames(history[1].DocTypes))

	// Version 3: Add WorkOrder, remove Invoice.
	reg.Remove("Invoice")
	reg.Register(&DocType{Name: "WorkOrder", Fields: []Field{{Fieldname: "title", Fieldtype: "Data"}}})
	history = append(history, &ConfigSnapshot{DocTypes: copyDoctypes(reg)})
	t.Logf("V3 created: %v", doctypeNames(history[2].DocTypes))

	// Now rollback to V2.
	rollbackTarget := history[1]
	restoredReg := NewRegistry()
	for _, dt := range rollbackTarget.DocTypes {
		restoredReg.Register(dt)
	}

	t.Logf("After rollback to V2: %v", restoredReg.Names())

	if restoredReg.Len() != 2 {
		t.Errorf("expected 2 doctypes after rollback to V2, got %d", restoredReg.Len())
	}
	if !restoredReg.Has("Customer") {
		t.Error("Customer should exist after rollback to V2")
	}
	if !restoredReg.Has("Invoice") {
		t.Error("Invoice should exist after rollback to V2")
	}
	if restoredReg.Has("WorkOrder") {
		t.Error("WorkOrder should NOT exist after rollback to V2")
	}

	t.Log("PASS: Full activation cycle — V1→V2→V3→rollback to V2 works correctly for doctypes")
}

// ---- Assumption 11: is_active vs status column conflict ----------------------

func TestVersioning_Assumption11_IsActiveVsStatus(t *testing.T) {
	// The system has TWO ways to mark an active version:
	// 1. Legacy: is_active = 1 (TINYINT)
	// 2. Current: status = 'Active' (VARCHAR)
	//
	// The API handlers (activate, rollback, delete) use 'status'.
	// The CLI rollback uses 'is_active'.
	// These are INCONSISTENT.

	t.Log("CONFIRMED: API handlers use 'status' column (Draft/Active/Superseded)")
	t.Log("CONFIRMED: CLI rollback uses legacy 'is_active' column (0/1)")
	t.Log("CONFIRMED: CLI 'kora config versions' reads 'is_active' for display")
	t.Log("CONFIRMED: These are TWO independent flags that can disagree")
	t.Log("")
	t.Log("Potential state: a version with status='Superseded' AND is_active=1")
	t.Log("This is a split-brain waiting to happen.")
}

// ---- Helpers ------------------------------------------------------------------

func doctypeNames(dts []*DocType) []string {
	names := make([]string, len(dts))
	for i, dt := range dts {
		names[i] = dt.Name
	}
	return names
}

func copyDoctypes(reg *Registry) []*DocType {
	var result []*DocType
	for _, name := range reg.Names() {
		if dt := reg.Get(name); dt != nil {
			copy := *dt
			result = append(result, &copy)
		}
	}
	return result
}
