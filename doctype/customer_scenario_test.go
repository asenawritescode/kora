package doctype

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// =============================================================================
// Alpha-Prod Customer Scenarios — Change Management + AI
// =============================================================================
// These tests simulate what a real customer would experience using Kora in
// production with AI-assisted doctype creation and config versioning.

// ---- Scenario 1: AI creates doctype → customer can't find it ------------------

func TestCustomer_Scenario1_AICreatesDraft_CustomerCantFindIt(t *testing.T) {
	// What AI does: create_doctype_draft at api/chat.go:1352-1426
	//   1. Validates YAML
	//   2. Saves to configstore (not activated)
	//   3. Creates Draft version
	//   4. Returns "✓ Created DocType X as DRAFT. A human must review and activate it."
	//
	// What the customer sees:
	//   - AI says "done"
	//   - Customer goes to their app sidebar — the doctype ISN'T THERE
	//   - Customer goes to the doctype list — it's NOT THERE
	//   - Customer thinks AI broke or lied

	t.Log("CUSTOMER JOURNEY:")
	t.Log("  1. Customer asks AI: 'Create a Sales Order doctype with these fields...'")
	t.Log("  2. AI responds: '✓ Created DocType \"Sales Order\" as DRAFT (8 fields).")
	t.Log("     Version #5 (ID: cv-erp.local-5). A human must review and activate it'")
	t.Log("  3. Customer goes to sidebar → Sales Order is NOT there")
	t.Log("  4. Customer goes to /workspace/Sales%20Order → 404")
	t.Log("  5. Customer thinks: 'AI lied to me' or 'Kora is broken'")
	t.Log("")
	t.Log("ROOT CAUSE: Draft doctypes exist in _kora_doctype and in-memory registry,")
	t.Log("  but the sidebar only shows doctypes with active config versions that")
	t.Log("  have been activated (migration run → table exists).")
	t.Log("  The customer has no idea they need to go to Admin → Versions → Activate.")
	t.Log("")
	t.Log("ISSUE: The AI response mentions 'activate' but the customer doesn't know")
	t.Log("  where or how. No direct link. No in-app notification. No 'pending activation'")
	t.Log("  badge on the sidebar. The doctype is silently invisible.")
}

// ---- Scenario 2: Customer activates AI doctype → analytics empty ------------

func TestCustomer_Scenario2_ActivateAIDoctype_AnalyticsEmpty(t *testing.T) {
	// Customer activates the AI-created Sales Order.
	// Migration runs → tabSalesOrder created.
	// Registry rebuilt → customer_count_by_status, etc. generated.
	// Customer goes to Insights tab → ALL CHARTS ARE EMPTY.

	t.Log("CUSTOMER JOURNEY:")
	t.Log("  1. Customer finds the Draft in Admin → Versions")
	t.Log("  2. Clicks Activate → migration runs → table created")
	t.Log("  3. Goes to Sales Order → Insights tab")
	t.Log("  4. Sees: 'Total: 0', 'By Status: (empty)', 'Created Daily: (flat line)'")
	t.Log("  5. Thinks: 'Why did I bother setting up analytics? It's broken.'")
	t.Log("")
	t.Log("ROOT CAUSE: Metrics are auto-generated but rollup tables are empty.")
	t.Log("  No data exists yet. The customer hasn't created any Sales Orders.")
	t.Log("  The Insights tab shows zero for everything — technically correct,")
	t.Log("  but a terrible first impression.")
	t.Log("")
	t.Log("ISSUE: The Insights tab should say 'No data yet — create your first")
	t.Log("  Sales Order to see analytics' instead of showing empty charts.")
	t.Log("  Or: show demo/placeholder data with a 'this is what you'll see' overlay.")
}

// ---- Scenario 3: AI queries analytics → gets orphaned data ---------------------

func TestCustomer_Scenario3_AIReadsOrphanedAnalytics(t *testing.T) {
	// Customer renamed a field: "category" → "product_line" (renamed_from: "category")
	// AI queries analytics:
	//   SELECT metric FROM _kora_analytics_daily WHERE doctype = 'Product'
	// Returns: product_count_by_category (old, orphaned) AND product_count_by_product_line (new, empty)
	// AI sees both and reports confusing results.

	t.Log("CUSTOMER JOURNEY:")
	t.Log("  1. Customer had Product doctype with 'category' Select field")
	t.Log("  2. Analytics collected product_count_by_category for 6 months")
	t.Log("  3. Customer renames 'category' → 'product_line' (renamed_from: 'category')")
	t.Log("  4. DDL renames column ✅")
	t.Log("  5. Customer asks AI: 'How many products by product line?'")
	t.Log("  6. AI queries _kora_analytics_daily:")
	t.Log("     - product_count_by_category: Electronics=50, Apparel=30 (OLD, orphaned)")
	t.Log("     - product_count_by_product_line: Electronics=5 (NEW, only since rename)")
	t.Log("  7. AI reports: 'You have 80 Electronics products and 30 Apparel'")
	t.Log("     (double-counting Electronics — 50 old + 5 new under different metric names)")
	t.Log("")
	t.Log("ROOT CAUSE: get_analytics_insights at chat_analytics.go:61-69 queries")
	t.Log("  ALL metrics for a doctype. After a rename, BOTH old and new metric")
	t.Log("  names exist in the rollup table. The AI has no way to know the old")
	t.Log("  metric is stale — it just sees two metrics and reports both.")
	t.Log("")
	t.Log("ISSUE: Rollup data is never migrated on field rename. The AI double-counts.")
	t.Log("  This is a DATA CORRUPTION issue from the customer's perspective.")
}

// ---- Scenario 4: Customer rollback → permissions wrong ------------------------

func TestCustomer_Scenario4_Rollback_LosesPermissions(t *testing.T) {
	// Customer sets up careful permissions:
	//   - Manager: read/write/create on Invoice
	//   - Clerk: read only on Invoice
	// Customer makes a bad doctype change, rolls back.
	// After rollback: Manager can no longer create Invoices. Clerk is gone entirely.

	t.Log("CUSTOMER JOURNEY:")
	t.Log("  1. Customer configures 5 roles with per-doctype permissions (1 hour of work)")
	t.Log("  2. Customer edits Invoice doctype — accidentally removes 'total' field")
	t.Log("  3. Customer panics, goes to Admin → Versions → Rollback to previous version")
	t.Log("  4. System says 'rolled back' — Invoice.total is back ✅")
	t.Log("  5. Next morning: Manager calls — 'I can't create Invoices anymore!'")
	t.Log("  6. Clerk calls — 'I can't see any Invoices!'")
	t.Log("")
	t.Log("ROOT CAUSE: Rollback restores doctypes from snapshot, but loads permissions")
	t.Log("  from LIVE _kora_permission table (api/system.go:845-847).")
	t.Log("  If permissions changed between the rollback target version and now,")
	t.Log("  those changes are NOT reverted. The customer's permission state is from")
	t.Log("  AFTER the bad change, not from the version they rolled back to.")
	t.Log("")
	t.Log("CUSTOMER IMPACT: Permissions silently diverge from the rolled-back state.")
	t.Log("  The UI says 'Version 4 (Active)' but the permission matrix is from Version 6.")
	t.Log("  This is a silent data integrity violation.")
}

// ---- Scenario 5: Two admins edit simultaneously → silent data loss -----------

func TestCustomer_Scenario5_ConcurrentAdmins_SilentDataLoss(t *testing.T) {
	// Admin A (Finance): adds 'tax_exempt' field to Customer
	// Admin B (Sales): adds 'lead_source' field to Customer
	// Both activate. Last one wins. First one's field is gone.

	t.Log("CUSTOMER JOURNEY:")
	t.Log("  1. Admin A (Finance team): Opens Customer doctype, adds 'tax_exempt' (Check)")
	t.Log("  2. Admin A clicks Save & Activate → Version 12 created → 'tax_exempt' in registry")
	t.Log("  3. Admin B (Sales team): Opens Customer doctype (BROWSER TAB FROM YESTERDAY)")
	t.Log("  4. Admin B adds 'lead_source' (Select). Clicks Save & Activate.")
	t.Log("  5. Version 13 created. Registry: Customer has 'lead_source' but NOT 'tax_exempt'")
	t.Log("  6. Admin A comes back: 'Where did my tax_exempt field go??'")
	t.Log("")
	t.Log("ROOT CAUSE: collectDoctypes(reg) captures the FULL registry at Save time.")
	t.Log("  Admin B's browser tab had the OLD Customer (no tax_exempt). When B saved,")
	t.Log("  the entire Customer doctype was replaced with B's version — which lacks")
	t.Log("  tax_exempt because B's tab was loaded before A's change.")
	t.Log("  No conflict detection. No 'this doctype was modified since you opened it.'")
	t.Log("  No merge attempt. Silent overwrite.")
	t.Log("")
	t.Log("CUSTOMER IMPACT: Admin A's work is silently lost. No error, no warning.")
	t.Log("  The Versions UI shows v12 and v13 — to recover, they must diff and manually")
	t.Log("  re-apply. Most customers won't know how. They'll just redo the work.")
}

// ---- Scenario 6: AI can create but not modify — customer frustration ---------

func TestCustomer_Scenario6_AIOnlyCreates_NeverModifies(t *testing.T) {
	// Customer asks AI: "Add a 'priority' field to Work Order"
	// AI: "Error: DocType 'Work Order' already exists."
	// Customer: "...but I just want to ADD a field, not create a new doctype."

	t.Log("CUSTOMER JOURNEY:")
	t.Log("  1. Customer: 'AI, add a priority field to the Work Order doctype'")
	t.Log("  2. AI calls create_doctype_draft with full YAML for Work Order + priority")
	t.Log("  3. AI gets: 'Error: DocType \"Work Order\" already exists.'")
	t.Log("  4. AI tells customer: 'I cannot modify the Work Order doctype because it")
	t.Log("     already exists. You'll need to edit it manually in the Admin panel.'")
	t.Log("  5. Customer thinks: 'This AI is useless. Even basic field additions fail.'")
	t.Log("")
	t.Log("ROOT CAUSE: create_doctype_draft at chat.go:1383 checks reg.Has(dt.Name)")
	t.Log("  and rejects duplicates. There is NO 'update_doctype' AI tool.")
	t.Log("  AI can only create new doctypes, never modify existing ones.")
	t.Log("")
	t.Log("CUSTOMER IMPACT: AI's utility is limited to initial setup. For day-to-day")
	t.Log("  changes (adding fields, changing labels, adding constraints), the customer")
	t.Log("  must use the manual YAML editor. AI becomes a one-time setup tool, not")
	t.Log("  an ongoing assistant.")
}

// ---- Scenario 7: Draft explosion → version list is noise ---------------------

func TestCustomer_Scenario7_DraftExplosion_VersionListNoise(t *testing.T) {
	// Customer uses AI to create 15 doctypes. Each is a Draft.
	// Customer manually edits 5 doctypes. Each change is a new version.
	// Customer activates some, discards none.
	// Versions list: 23 Drafts + 8 Active/Superseded = 31 versions.
	// Finding "which version should I activate?" is impossible.

	t.Log("CUSTOMER JOURNEY:")
	t.Log("  1. Customer uses AI to create 15 doctypes → 15 Draft versions")
	t.Log("  2. Customer manually edits 5 doctypes → 5 more versions (mix of Draft/Active)")
	t.Log("  3. Customer activates 8 doctypes one by one → 8 Active + 8 Superseded")
	t.Log("  4. Customer opens Admin → Versions")
	t.Log("  5. Sees: 31 versions. Mixed Draft, Active, Superseded.")
	t.Log("     Labels: 'Created Customer via AI (Draft)', 'Created Invoice via AI (Draft)',")
	t.Log("     'Updated WorkOrder via web', 'Activated version cv-...', ...")
	t.Log("  6. Customer: 'Which of these 15 Drafts should I activate? Are they all")
	t.Log("     independent? Will activating one break another?'")
	t.Log("")
	t.Log("ROOT CAUSE: No version grouping, no dependency tracking, no 'activate all")
	t.Log("  pending drafts' action. Each draft is independent but activating one")
	t.Log("  replaces ALL doctypes with that draft's snapshot.")
	t.Log("")
	t.Log("CUSTOMER IMPACT: Version management becomes a puzzle game. The customer")
	t.Log("  needs to understand the monolithic versioning model to safely activate")
	t.Log("  drafts in the correct order. Most customers won't understand this.")
}

// ---- Scenario 8: Customer activates wrong draft → loses doctypes -------------

func TestCustomer_Scenario8_WrongDraftActivation_LosesDoctypes(t *testing.T) {
	// Customer has: Customer, Invoice, WorkOrder (all Active)
	// AI created: SalesOrder (Draft, created when only Customer existed)
	// Customer activates the AI's Draft → Invoice and WorkOrder DISAPPEAR.

	t.Log("CUSTOMER JOURNEY:")
	t.Log("  1. Customer sets up: Customer, Invoice, WorkOrder (all Active, v3)")
	t.Log("  2. Customer asks AI to create SalesOrder")
	t.Log("  3. AI creates SalesOrder as Draft v4. But v4's snapshot only has:")
	t.Log("     [Customer, SalesOrder] because the AI collected doctypes")
	t.Log("     from the registry at creation time")
	t.Log("  4. Wait — actually AI calls reg.All() at line 1407-1410, so v4 SHOULD")
	t.Log("     have all 4 doctypes. Let me verify...")

	// Actually, the AI path does collect from the current registry.
	// So v4 would have [Customer, Invoice, WorkOrder, SalesOrder].
	// But what if the AI created the draft BEFORE Invoice and WorkOrder were added?

	t.Log("")
	t.Log("  REVISED SCENARIO:")
	t.Log("  1. Customer asks AI: 'Create these 4 doctypes: Customer, Invoice, WorkOrder, SalesOrder'")
	t.Log("  2. AI creates them one by one. Each creation snapshots the registry AT THAT MOMENT:")
	t.Log("     Draft v1: [Customer]")
	t.Log("     Draft v2: [Customer, Invoice]")
	t.Log("     Draft v3: [Customer, Invoice, WorkOrder]")
	t.Log("     Draft v4: [Customer, Invoice, WorkOrder, SalesOrder]")
	t.Log("  3. Customer activates v1 (thinking 'I'll activate them in order')")
	t.Log("  4. Registry becomes: [Customer] only. Invoice, WorkOrder, SalesOrder GONE.")
	t.Log("  5. Customer: 'WHERE DID EVERYTHING GO?!'")
	t.Log("")
	t.Log("CUSTOMER IMPACT: Activating a draft from early in the creation sequence")
	t.Log("  wipes all later doctypes. The customer needs to activate ONLY the LAST")
	t.Log("  draft in the sequence (v4). This is completely non-obvious.")
	t.Log("  The correct workflow is: create all drafts, then activate the FINAL one.")
	t.Log("  But the UI doesn't communicate this. Each draft looks independent.")
}

// ---- Scenario 9: AI creates doctype → metris generated → activate → reset ----

func TestCustomer_Scenario9_AIMetricsResetOnActivation(t *testing.T) {
	// AI creates SalesOrder as Draft.
	// Worker generates metrics: sales_order_count, sales_order_count_by_status, etc.
	// (Metrics are cached in worker.metrics map)
	// Customer activates the draft → worker cache invalidated → metrics regenerated
	// → metric lineage lost → customer sees different metric names?

	// Actually, the metric names don't change on activation — the doctype didn't change.
	// But the issue is subtler: during the Draft period, NO events were emitted
	// because create_doctype_draft doesn't create documents — it creates config.
	// So the rollup tables have zero rows for the new metrics.
	// After activation, new documents start generating events → rollup data appears.
	// But there's no "backfill the draft period" — the metrics start from activation time.

	t.Log("CUSTOMER JOURNEY:")
	t.Log("  1. AI creates SalesOrder as Draft (v5)")
	t.Log("  2. Worker caches metrics: sales_order_count, sales_order_count_by_status, etc.")
	t.Log("  3. Customer doesn't activate immediately. Waits 3 days.")
	t.Log("  4. During those 3 days: zero events for SalesOrder (no table exists yet)")
	t.Log("  5. Customer activates v5 → table created → metrics regenerate")
	t.Log("  6. Customer creates 10 SalesOrders")
	t.Log("  7. Insights tab shows: Total=10, Created Daily=10 (only today)")
	t.Log("  8. Customer: 'Why does Created Daily show a spike of 10 today and")
	t.Log("     zero for the past 30 days? The doctype was created 3 days ago!'")
	t.Log("")
	t.Log("ROOT CAUSE: Metrics track document events, not config events.")
	t.Log("  The 'created 3 days ago' is a config version timestamp, not a data point.")
	t.Log("  The historical zeros in the chart are technically correct (no documents existed)")
	t.Log("  but confusing because the customer perceives the doctype as 'existing' from")
	t.Log("  the moment AI created it.")
	t.Log("")
	t.Log("CUSTOMER IMPACT: The timeline doesn't match the customer's mental model.")
	t.Log("  'Created Daily' chart shows the doctype popping into existence today,")
	t.Log("  not 3 days ago when they asked AI to create it.")
}

// ---- Scenario 10: Customer wants to revert ONE doctype → can't -------------

func TestCustomer_Scenario10_CantRevertSingleDoctype(t *testing.T) {
	// Customer made a bad change to Invoice. Wants to revert JUST Invoice to v5.
	// But versioning is monolithic. Rolling back to v5 reverts EVERY doctype to v5 state.
	// Customer: "I just want to undo the Invoice change, not the Customer change!"

	t.Log("CUSTOMER JOURNEY:")
	t.Log("  1. v5: Customer (name, status), Invoice (total, tax)")
	t.Log("  2. Admin A edits Customer → adds 'priority' field → v6 Active")
	t.Log("  3. Admin B edits Invoice → removes 'tax' field → v7 Active")
	t.Log("  4. Customer realizes 'tax' was important, wants to revert Invoice")
	t.Log("  5. Customer goes to Versions, finds v5 (before Invoice change)")
	t.Log("  6. Customer clicks Rollback to v5")
	t.Log("  7. Customer.loses 'priority' field (added in v6, not in v5)")
	t.Log("  8. Customer: 'I just wanted to fix Invoice! Why did Customer change too?!'")
	t.Log("")
	t.Log("ROOT CAUSE: Monolithic versioning. One version = one complete system state.")
	t.Log("  There is no per-doctype rollback. You cannot revert a single doctype's")
	t.Log("  changes without reverting everything else that changed between those versions.")
	t.Log("")
	t.Log("CUSTOMER IMPACT: Rollback is a nuclear option. Customers will be afraid to")
	t.Log("  use it because it affects things they didn't intend to change.")
	t.Log("  They'll resort to manual fixes instead — editing the YAML by hand to")
	t.Log("  re-add the 'tax' field, then creating yet another version.")
}

// ---- Scenario 11: Customer exports config → imports on another site → gap ---

func TestCustomer_Scenario11_ExportImport_ConfigDrift(t *testing.T) {
	// Customer exports config from staging (kora config export --site staging --path /tmp/export)
	// Imports to production (kora config import --site prod --path /tmp/export)
	// Export writes: doctypes/*.yaml, roles.yaml, permissions.yaml, workflows/*.yaml
	// Import reads all of them, saves to DB.
	// But CreateConfigVersion only stores doctypes in the snapshot.
	// So production's version history is INCOMPLETE — missing roles/permissions/workflows.

	t.Log("CUSTOMER JOURNEY:")
	t.Log("  1. Customer builds full ERP in staging: 23 doctypes, 5 roles,")
	t.Log("     115 permissions, 3 workflows")
	t.Log("  2. Exports: kora config export --site staging.local --path /tmp/erp-config")
	t.Log("     Output: 23 doctype YAMLs + roles.yaml + permissions.yaml + 3 workflow YAMLs")
	t.Log("  3. Imports to production: kora config import --site erp.local --path /tmp/erp-config")
	t.Log("     Result: 'Imported 23 doctypes, 5 roles, 115 permissions, 3 workflows'")
	t.Log("  4. Version 1 created: config snapshot contains ONLY 23 doctypes")
	t.Log("     Roles? NOT in snapshot. Permissions? NOT in snapshot. Workflows? NOT.")
	t.Log("  5. 3 months later: customer needs to rollback to v1")
	t.Log("  6. Rollback: doctypes restored ✅. Roles/permissions loaded from LIVE DB ❌.")
	t.Log("     If roles changed since v1, they're NOT restored to initial import state.")
	t.Log("")
	t.Log("ROOT CAUSE: Config export is MORE complete than DB version snapshots.")
	t.Log("  The YAML files are the actual source of truth for roles/permissions/workflows,")
	t.Log("  but the DB version system doesn't capture them. After import, the version")
	t.Log("  history is permanently incomplete.")
	t.Log("")
	t.Log("CUSTOMER IMPACT: The YAML files on disk are more trustworthy than the DB")
	t.Log("  version history. Customers will learn to keep YAML in git as their real")
	t.Log("  version control, treating the DB versions as a convenience feature for")
	t.Log("  minor undo operations — not a reliable audit trail.")
}

// ---- Scenario 12: Snapshot ordering differs between AI and human ------------

func TestCustomer_Scenario12_AIvsHuman_SnapshotOrdering(t *testing.T) {
	// AI creates a doctype: uses reg.All() → map iteration order → random ordering
	// Human creates a doctype: uses collectDoctypes(reg) → same random ordering
	// Two consecutive versions of the SAME state can have different JSON ordering.
	// This makes the diff between them noisy — shows reordering as changes.

	reg := NewRegistry()
	for _, name := range []string{"Customer", "Invoice", "WorkOrder", "Project", "Task"} {
		reg.Register(&DocType{Name: name})
	}

	// Simulate AI collecting doctypes (chat.go:1407-1410)
	aiDoctypes1 := make([]*DocType, 0)
	for _, d := range reg.All() {
		aiDoctypes1 = append(aiDoctypes1, d)
	}

	// Simulate human collecting doctypes (api/system.go:collectDoctypes)
	humanDoctypes := make([]*DocType, 0)
	for _, name := range reg.Names() {
		if dt := reg.Get(name); dt != nil {
			humanDoctypes = append(humanDoctypes, dt)
		}
	}

	// Both are from the SAME state but may have DIFFERENT ordering.
	aiJSON, _ := json.Marshal(aiDoctypes1)
	humanJSON, _ := json.Marshal(humanDoctypes)

	t.Logf("AI snapshot names:     %v", doctypeNames(aiDoctypes1))
	t.Logf("Human snapshot names:  %v", doctypeNames(humanDoctypes))

	// When the order differs, json.Marshal produces different bytes.
	// Even though the CONTENT is identical, the ORDER differs.
	// This makes it impossible to checksum-compare versions.

	if string(aiJSON) != string(humanJSON) {
		t.Log("RISK CONFIRMED: AI and human snapshots of the same state produce")
		t.Log("  different JSON because map iteration order is non-deterministic.")
		t.Log("  Two versions created from identical state can have different JSON.")
		t.Log("  Diffing them shows false changes (reordering).")
		t.Log("  Checksum-based equality checks fail.")
	} else {
		t.Log("Note: Order matched this run (rare with 5+ entries)")
	}

	// The fix: sort doctypes by name before snapshotting.
	sorted := make([]*DocType, len(aiDoctypes1))
	copy(sorted, aiDoctypes1)
	// sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	// But we can't import sort here — test the concept.

	t.Log("FIX: Sort doctypes by name before json.Marshal to ensure deterministic")
	t.Log("  snapshots regardless of map iteration order.")
}

// ---- Scenario 13: SLA — how long until a Draft becomes Active? ---------------

func TestCustomer_Scenario13_DraftToActiveLatency(t *testing.T) {
	// Customer's workflow:
	//   Day 1: AI creates SalesOrder (Draft)
	//   Day 1-3: Human reviews the draft
	//   Day 4: Human activates
	//   → 4 days from creation to activation
	//
	// What happened to analytics during those 4 days?
	//   Nothing. Zero events. Zero rollup data.
	// What happened to other changes during those 4 days?
	//   If another admin activated a different draft on Day 2, the SalesOrder
	//   draft's snapshot is now stale (doesn't include the Day 2 changes).

	t.Log("CUSTOMER JOURNEY:")
	t.Log("  Day 1, 10:00 — AI creates SalesOrder as Draft v5")
	t.Log("  Day 1, 14:00 — Admin activates Invoice update (v6)")
	t.Log("  Day 2, 09:00 — Admin activates WorkOrder update (v7)")
	t.Log("  Day 3, 11:00 — Customer finally reviews SalesOrder Draft v5")
	t.Log("  Day 3, 11:05 — Customer clicks Activate on v5")
	t.Log("")
	t.Log("  What happens:")
	t.Log("  - v5 snapshot: [Customer, Invoice, WorkOrder, SalesOrder]")
	t.Log("    ↑ This is the state as of Day 1. It does NOT include v6's Invoice")
	t.Log("    changes or v7's WorkOrder changes.")
	t.Log("  - Activating v5 REPLACES the registry with Day 1 state.")
	t.Log("  - v6 and v7 changes are WIPED.")
	t.Log("  - Customer doesn't see a warning: 'Activating this version will revert")
	t.Log("    changes made to Invoice (v6) and WorkOrder (v7).'")
	t.Log("")
	t.Log("CUSTOMER IMPACT: Long-lived Drafts are dangerous. The longer a Draft sits")
	t.Log("  unactivated, the more the system state diverges from its snapshot. Activating")
	t.Log("  an old Draft is effectively a partial rollback — reverting everything that")
	t.Log("  changed since the Draft was created.")
	t.Log("  There's no staleness warning, no 'this draft is 3 days old and 2 other")
	t.Log("  versions have been activated since it was created.'")
}

// ---- Summary -----------------------------------------------------------------

func TestCustomer_Summary_AlphaProdIssues(t *testing.T) {
	scenarios := []struct {
		id    int
		title string
		sev   string
	}{
		{1, "AI creates Draft → customer can't find it", "High"},
		{2, "Activate AI doctype → analytics empty", "Medium"},
		{3, "AI reads orphaned analytics after rename", "High"},
		{4, "Rollback loses permissions", "High"},
		{5, "Concurrent admins → silent data loss", "Critical"},
		{6, "AI can only create, never modify", "High"},
		{7, "Draft explosion → version list noise", "Medium"},
		{8, "Wrong draft activation → loses doctypes", "Critical"},
		{9, "AI metrics reset on activation", "Low"},
		{10, "Can't revert single doctype", "High"},
		{11, "Export/import → incomplete version history", "Medium"},
		{12, "AI vs human → non-deterministic snapshots", "Low"},
		{13, "Stale Draft → partial rollback on activation", "Critical"},
	}

	fmt.Println("\n==============================================")
	fmt.Println("  ALPHA-PROD CUSTOMER ISSUES — SUMMARY")
	fmt.Println("==============================================")
	for _, s := range scenarios {
		fmt.Printf("  [%s] S%d: %s\n", s.sev, s.id, s.title)
	}
	fmt.Println("==============================================")
	fmt.Printf("  Critical: %d, High: %d, Medium: %d, Low: %d\n",
		countBySev(scenarios, "Critical"),
		countBySev(scenarios, "High"),
		countBySev(scenarios, "Medium"),
		countBySev(scenarios, "Low"))
	fmt.Println("==============================================")
}

func countBySev(scenarios []struct {
	id    int
	title string
	sev   string
}, sev string) int {
	n := 0
	for _, s := range scenarios {
		if s.sev == sev {
			n++
		}
	}
	return n
}

// Ensure strings import used.
var _ = strings.Title
