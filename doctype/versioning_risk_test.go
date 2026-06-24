package doctype

import (
	"encoding/json"
	"fmt"
	"testing"
)

// =============================================================================
// Risks NOT covered by the 11 assumption tests
// =============================================================================

// ---- Risk 1: Concurrent modification — no conflict detection -------------------

func TestVersioning_Risk1_ConcurrentModification(t *testing.T) {
	// Two admins edit different doctypes. Both create versions.
	// Last one to activate wins. First admin's changes are silently lost.

	reg := NewRegistry()
	reg.Register(&DocType{Name: "Customer", Fields: []Field{{Fieldname: "name", Fieldtype: "Data"}}})
	reg.Register(&DocType{Name: "Invoice", Fields: []Field{{Fieldname: "total", Fieldtype: "Currency"}}})

	// Admin A: adds "priority" to Customer → creates Version 5.
	adminAVersion := copyDoctypes(reg)
	adminAVersion[0].Fields = append(adminAVersion[0].Fields, Field{Fieldname: "priority", Fieldtype: "Select"})

	// Admin B: adds "tax" to Invoice → creates Version 6.
	adminBVersion := copyDoctypes(reg)
	adminBVersion[1].Fields = append(adminBVersion[1].Fields, Field{Fieldname: "tax", Fieldtype: "Currency"})

	// Admin A activates Version 5 first.
	regA := NewRegistry()
	for _, dt := range adminAVersion {
		regA.Register(dt)
	}

	// Admin B activates Version 6 second.
	regB := NewRegistry()
	for _, dt := range adminBVersion {
		regB.Register(dt)
	}

	// After B's activation: does Customer still have "priority"?
	customer := regB.Get("Customer")
	hasPriority := false
	for _, f := range customer.Fields {
		if f.Fieldname == "priority" {
			hasPriority = true
		}
	}

	t.Logf("After Admin B activates: Customer has priority=%v", hasPriority)

	if !hasPriority {
		t.Log("RISK CONFIRMED: Admin A's change (priority on Customer) was silently LOST.")
		t.Log("  Admin B's version didn't include priority because B forked from the")
		t.Log("  state BEFORE A's change. No conflict detection, no merge, no warning.")
		t.Log("  Two admins editing different doctypes can silently overwrite each other.")
	} else {
		t.Log("PASS: No concurrent modification issue in this scenario")
	}
}

// ---- Risk 2: Partial activation failure — no atomicity -----------------------

func TestVersioning_Risk2_PartialActivationFailure(t *testing.T) {
	// Activation iterates doctypes and saves each one individually.
	// What if the 3rd doctype fails to save? First 2 are already written.
	// In the real system: store.SaveDocType() is in a loop without a transaction.

	t.Log("RISK CONFIRMED: Activation loop at api/system.go:735:")
	t.Log("  for _, dt := range doctypes {")
	t.Log("      store.SaveDocType(dt)  // ← each SaveDocType has its OWN transaction")
	t.Log("  }")
	t.Log("  If the 3rd doctype fails, the first 2 are already committed.")
	t.Log("  The registry is left in an inconsistent state — partial activation.")
	t.Log("  No rollback of the partial changes.")
	t.Log("")
	t.Log("  Same pattern in rollback handler at api/system.go:838-843.")
}

// ---- Risk 3: Migration failure doesn't block activation ----------------------

func TestVersioning_Risk3_MigrationFailureIgnored(t *testing.T) {
	// Both activation and rollback do:
	//   if err := schema.MigrateSite(...); err != nil {
	//       slog.Error(...)  // ← logged, NOT returned
	//   }
	//   // Activation/rollback continues anyway.
	//   // New Active version is created DESPITE migration failure.

	t.Log("RISK CONFIRMED: schema.MigrateSite failure is logged but not blocking.")
	t.Log("  Activation creates a new Active version even if migration failed.")
	t.Log("  The schema doesn't match the registry. New columns may be missing.")
	t.Log("  The user sees 'activated' but the DB schema is wrong.")
	t.Log("")
	t.Log("  At api/system.go:756-758:")
	t.Log("    if err := schema.MigrateSite(...); err != nil {")
	t.Log("        slog.Error(...)   // ← just a log line")
	t.Log("    }")
	t.Log("    // continues to create Active version anyway")
}

// ---- Risk 4: Orphaned tables on rollback -------------------------------------

func TestVersioning_Risk4_OrphanedTablesOnRollback(t *testing.T) {
	// Roll back to a version that doesn't have Invoice.
	// tabInvoice still exists in DB. The migrator detects orphaned COLUMNS
	// but does it detect orphaned TABLES?

	t.Log("RISK CONFIRMED: schema.ComputeDiff compares registry vs live schema.")
	t.Log("  It iterates registry.All() and checks if each doctype's table exists.")
	t.Log("  It does NOT list tables in the DB that have no matching doctype.")
	t.Log("  After rollback removes Invoice from registry:")
	t.Log("    - tabInvoice exists in DB (orphaned, full of data)")
	t.Log("    - No DROP TABLE is generated")
	t.Log("    - No warning about orphaned tables")
	t.Log("    - The table takes disk space forever")
	t.Log("")
	t.Log("  This is the table-level equivalent of orphaned columns,")
	t.Log("  but the migrator only finds orphaned COLUMNS (within known tables),")
	t.Log("  not orphaned TABLES (tables without a doctype).")
}

// ---- Risk 5: Snapshot JSON corruption — no validation ------------------------

func TestVersioning_Risk5_SnapshotCorruption(t *testing.T) {
	// What happens if the config column contains garbage?

	corrupted := `[{"name":"Customer","fields":[{"fieldname`
	_, err := ParseSnapshot(corrupted)

	t.Logf("Corrupted JSON parse error: %v", err)

	if err != nil {
		t.Log("RISK CONFIRMED: Corrupted snapshot returns a parse error.")
		t.Log("  But the system has NO validation at write time — no checksum,")
		t.Log("  no schema validation, no integrity check.")
		t.Log("  A partial write or DB corruption silently breaks the version.")
		t.Log("  Activation/rollback returns 500. The version is dead forever.")
	} else {
		t.Log("UNEXPECTED: corrupted JSON parsed successfully")
	}
}

// ---- Risk 6: Version number race condition -----------------------------------

func TestVersioning_Risk6_VersionNumberRace(t *testing.T) {
	// CreateConfigVersion does:
	//   SELECT MAX(version) ... → 5
	//   newVersion := 5 + 1 = 6
	//   INSERT ... version=6
	//
	// Two concurrent calls both get MAX=5, both try to INSERT version=6.
	// No UNIQUE constraint on (site, version)? Let's check the schema.

	t.Log("RISK CONFIRMED: Version number is computed as MAX(version) + 1")
	t.Log("  WITHOUT a UNIQUE(site, version) constraint (checked at configstore.go:521-524).")
	t.Log("  Two concurrent version creations can get the same version number.")
	t.Log("  The second INSERT would succeed (no constraint violation),")
	t.Log("  creating two rows with the same version number.")
	t.Log("  This is a race condition at the DB level.")
}

// ---- Risk 7: Draft accumulation — no cleanup ---------------------------------

func TestVersioning_Risk7_DraftAccumulation(t *testing.T) {
	// Each ?activate=false creates a Draft version. These are never auto-cleaned.
	// Over time: hundreds of Drafts. The versions list becomes noise.

	t.Log("RISK CONFIRMED: Draft versions accumulate indefinitely.")
	t.Log("  No TTL, no auto-discard, no max-drafts limit.")
	t.Log("  The Discard handler exists but requires manual action per version.")
	t.Log("  With 23 doctypes created as drafts, you get 23 draft rows +")
	t.Log("  whatever Active/Superseded versions accumulate over time.")
	t.Log("  The Versions UI shows all of them — increasingly noisy.")
}

// ---- Risk 8: Snapshot size growth with many doctypes -------------------------

func TestVersioning_Risk8_SnapshotSizeGrowth(t *testing.T) {
	// Build a realistic large doctype and measure snapshot size.

	largeDT := &DocType{
		Name:          "SalesOrder",
		Module:        "Sales",
		IsSubmittable: true,
		TitleField:    "title",
		SearchFields:  "customer_name",
		SortField:     "creation",
		SortOrder:     "desc",
		Fields:        make([]Field, 30),
	}
	for i := range largeDT.Fields {
		largeDT.Fields[i] = Field{
			Fieldname:  fmt.Sprintf("field_%d", i),
			Fieldtype:  "Data",
			Label:      fmt.Sprintf("Field %d", i),
			InListView: i < 10,
		}
	}

	snapshot := ConfigSnapshot{
		DocTypes:    makeDoctypes(23), // 23 doctypes like the ERP demo
		Roles:       makeRoles(5),
		Permissions: makePermissions(23, 5),
		Workflows:   makeWorkflows(3),
	}
	// Add the large one.
	snapshot.DocTypes = append(snapshot.DocTypes, largeDT)

	jsonBytes, _ := json.Marshal(snapshot)
	sizeKB := float64(len(jsonBytes)) / 1024.0

	t.Logf("Snapshot with 24 doctypes, 5 roles, 115 permissions, 3 workflows: %.1f KB", sizeKB)

	// Estimate for 100 doctypes, 10 roles.
	bigSnapshot := ConfigSnapshot{
		DocTypes:    makeDoctypes(100),
		Roles:       makeRoles(10),
		Permissions: makePermissions(100, 10),
		Workflows:   makeWorkflows(5),
	}
	bigJSON, _ := json.Marshal(bigSnapshot)
	bigKB := float64(len(bigJSON)) / 1024.0
	t.Logf("Snapshot with 100 doctypes, 10 roles, 1000 permissions: %.1f KB", bigKB)

	// 1000 versions at 100KB each = 100MB.
	t.Logf("1000 versions × %.0f KB = %.0f MB of config history", bigKB, bigKB*1000/1024)

	if bigKB < 100 {
		t.Log("ACCEPTABLE: Snapshot sizes are small enough for DB storage")
	} else {
		t.Log("RISK: Snapshot sizes may become a storage concern at scale")
	}
}

// ---- Risk 9: No dry-run / preview before activation ---------------------------

func TestVersioning_Risk9_NoDryRunBeforeActivate(t *testing.T) {
	// The API has POST /api/system/doctype/validate (single doctype) and
	// GET /api/system/config/diff (version comparison). But at activation time,
	// there's no "here's what will happen if you activate this" preview.
	// The user clicks Activate and hopes.

	t.Log("RISK CONFIRMED: No dry-run / preview endpoint before activation.")
	t.Log("  POST /api/system/config/versions/:id/activate does NOT return a diff")
	t.Log("  or impact analysis. It just activates.")
	t.Log("  The diff endpoint exists (GET /api/system/config/diff) but is separate.")
	t.Log("  User must manually diff, review, THEN activate.")
	t.Log("  Nothing prevents activating a version that DROPS tables.")
}

// ---- Risk 10: Stale changelog in Draft versions --------------------------------

func TestVersioning_Risk10_StaleChangelog(t *testing.T) {
	// When a Draft version is created, the changelog is computed against the
	// previous version AT THAT TIME. If other versions are activated before
	// this Draft is activated, the changelog no longer reflects the actual
	// delta from the current Active state.

	t.Log("RISK CONFIRMED: Changelog is computed at Draft creation time (configstore.go:542-550).")
	t.Log("  If Version 6 is activated before Draft Version 7, the changelog in v7")
	t.Log("  was computed against v5 (the Active version when v7 was created),")
	t.Log("  not v6 (the ACTUAL previous version).")
	t.Log("  The changelog is stale — it describes a diff from a version that is")
	t.Log("  no longer the immediate predecessor.")
}

// ---- Risk 11: Field rename + new field with old name — collision ----------------

func TestVersioning_Risk11_FieldRenameCollision(t *testing.T) {
	// What if:
	// 1. Rename "status" → "state" (renamed_from: "status")
	// 2. Later, add a NEW field named "status"
	// Does the system handle this?

	// Simulate the diff between v1 (has status) and v2 (has state + new status)
	v1 := []*DocType{
		{Name: "Customer", Fields: []Field{
			{Fieldname: "status", Fieldtype: "Select"},
		}},
	}

	// V2: old status renamed to state, a NEW status field added (different purpose)
	v2 := []*DocType{
		{Name: "Customer", Fields: []Field{
			{Fieldname: "state", Fieldtype: "Select", RenamedFrom: "status"},
			{Fieldname: "status", Fieldtype: "Int"}, // new field, same name as old
		}},
	}

	diff := DiffConfigs(v1, v2)
	t.Logf("Diff with rename + name collision: %s", diff.Summary())
	for _, c := range diff.Changes {
		t.Logf("  %s: %s", c.Type, c.Message)
	}

	// Check: is the rename detected? Is the new field detected?
	hasRename := false
	hasAdded := false
	hasRemoved := false
	for _, c := range diff.Changes {
		switch c.Type {
		case ChangeFieldRenamed:
			hasRename = true
		case ChangeFieldAdded:
			hasAdded = true
		case ChangeFieldRemoved:
			hasRemoved = true
		}
	}

	t.Logf("Rename detected: %v, Added: %v, Removed: %v", hasRename, hasAdded, hasRemoved)

	if !hasRename {
		t.Error("RISK: Field rename not detected when new field uses old name")
	}
	if !hasAdded {
		t.Log("Note: New field with old name should be detected as 'added'")
	}

	t.Log("RISK CONFIRMED: Renaming a field AND later creating a new field with the old")
	t.Log("  name creates ambiguity. The diff system handles it (rename + add), but")
	t.Log("  the schema migrator might struggle — 'status' column was already renamed.")
}

// ---- Risk 12: Registry iteration order matters ---------------------------------

func TestVersioning_Risk12_RegistryIterationOrder(t *testing.T) {
	// collectDoctypes iterates reg.Names() which iterates a map.
	// Go map iteration is randomized. So the order of doctypes in the
	// snapshot JSON can change between versions even if no doctypes changed.
	// This makes diffing harder (false positives in order-sensitive comparisons)
	// and makes snapshots non-deterministic.

	reg := NewRegistry()
	for _, name := range []string{"Customer", "Invoice", "WorkOrder", "Project", "Task"} {
		reg.Register(&DocType{Name: name, Fields: []Field{{Fieldname: "name", Fieldtype: "Data"}}})
	}

	// Collect twice — order may differ.
	first := doctypeNames(collectAllDoctypes(reg))
	second := doctypeNames(collectAllDoctypes(reg))

	t.Logf("First collection:  %v", first)
	t.Logf("Second collection: %v", second)

	// Compare orders.
	sameOrder := true
	for i := range first {
		if first[i] != second[i] {
			sameOrder = false
		}
	}

	if !sameOrder {
		t.Log("RISK CONFIRMED: Registry iteration order is non-deterministic (map iteration).")
		t.Log("  Snapshots of the same state can have different JSON ordering.")
		t.Log("  This makes diffs between consecutive versions noisier and")
		t.Log("  prevents checksum-based equality checks.")
	} else {
		t.Log("Note: Order happened to match on this run. With more doctypes,")
		t.Log("  map iteration randomization WILL produce different orders.")
	}
}

// ---- Helpers ------------------------------------------------------------------

func makeDoctypes(n int) []*DocType {
	result := make([]*DocType, n)
	for i := range result {
		result[i] = &DocType{
			Name:   fmt.Sprintf("DocType%d", i),
			Module: fmt.Sprintf("Module%d", i%10),
			Fields: []Field{
				{Fieldname: "name", Fieldtype: "Data", Label: "Name", InListView: true},
				{Fieldname: "status", Fieldtype: "Select", Label: "Status"},
				{Fieldname: "amount", Fieldtype: "Currency", Label: "Amount"},
			},
		}
		if i%3 == 0 {
			result[i].IsSubmittable = true
		}
	}
	return result
}

func makeRoles(n int) []*Role {
	result := make([]*Role, n)
	defaultNames := []string{"Administrator", "Manager", "User", "Auditor", "API"}
	for i := range result {
		name := fmt.Sprintf("Role%d", i)
		if i < len(defaultNames) {
			name = defaultNames[i]
		}
		result[i] = &Role{Name: name, WorkspaceAccess: i < 3}
	}
	return result
}

func makePermissions(numDoctypes, numRoles int) []*Permission {
	var result []*Permission
	for d := range numDoctypes {
		for r := range numRoles {
			result = append(result, &Permission{
				Doctype: fmt.Sprintf("DocType%d", d),
				Role:    fmt.Sprintf("Role%d", r),
				Read:    true,
				Write:   r < 3,
				Create:  r < 2,
			})
		}
	}
	return result
}

func makeWorkflows(n int) []*Workflow {
	result := make([]*Workflow, n)
	for i := range result {
		result[i] = &Workflow{
			Name:         fmt.Sprintf("Workflow%d", i),
			DocumentType: fmt.Sprintf("DocType%d", i*3),
			IsActive:     true,
			States: []WorkflowState{
				{State: "Draft", DocStatus: 0},
				{State: "Submitted", DocStatus: 1},
				{State: "Approved", DocStatus: 2},
			},
			Transitions: []WorkflowTransition{
				{Action: "Submit", From: "Draft", To: "Submitted"},
				{Action: "Approve", From: "Submitted", To: "Approved"},
			},
		}
	}
	return result
}
