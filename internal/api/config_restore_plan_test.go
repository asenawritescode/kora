package api

import (
	"strings"
	"testing"

	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
	"github.com/asenawritescode/kora/internal/schema"
)

func TestBuildConfigRestoreDDLReplansQuarantineRestore(t *testing.T) {
	current := &doctype.DocType{Name: "Invoice", Fields: []doctype.Field{
		{Fieldname: "subject", Fieldtype: "Data"},
		{Fieldname: "extra", Fieldtype: "Data"},
	}}
	target := &doctype.ConfigSnapshot{DocTypes: []*doctype.DocType{{Name: "Invoice", Fields: []doctype.Field{
		{Fieldname: "subject", Fieldtype: "Data"},
		{Fieldname: "legacy", Fieldtype: "Data"},
	}}}}
	quarantinedLegacy := doctype.QuarantineNameForRevision("legacy", "old-activation")
	live := map[string]*schema.TableInfo{"tabInvoice": {
		Name: "tabInvoice", Columns: map[string]*schema.ColumnInfo{
			"subject": {Name: "subject"}, "extra": {Name: "extra"}, quarantinedLegacy: {Name: quarantinedLegacy},
		},
	}}
	first, err := buildConfigRestoreDDL([]*doctype.DocType{current}, target, live, db.MySQL(), "restore-operation")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(first, "\n")
	if !strings.Contains(joined, "RENAME COLUMN `"+quarantinedLegacy+"` TO `legacy`") {
		t.Fatalf("restore plan did not recover the parked legacy column value: %s", joined)
	}
	quarantinedExtra := doctype.QuarantineNameForRevision("extra", "restore-operation")
	if !strings.Contains(joined, "RENAME COLUMN `extra` TO `"+quarantinedExtra+"`") {
		t.Fatalf("restore plan did not retain the removed active field: %s", joined)
	}
	if strings.Contains(joined, "ADD COLUMN `legacy`") {
		t.Fatalf("restore plan would create an empty replacement instead of restoring data: %s", joined)
	}

	// Model the physical effect of the first statement sequence. A retry must
	// see the target column restored and the removed field already quarantined.
	retriedLive := map[string]*schema.TableInfo{"tabInvoice": {
		Name: "tabInvoice", Columns: map[string]*schema.ColumnInfo{
			"subject": {Name: "subject"}, "legacy": {Name: "legacy"}, quarantinedExtra: {Name: quarantinedExtra},
		},
	}}
	retry, err := buildConfigRestoreDDL([]*doctype.DocType{current}, target, retriedLive, db.MySQL(), "restore-operation")
	if err != nil {
		t.Fatal(err)
	}
	if len(retry) != 0 {
		t.Fatalf("retry should not repeat already-applied DDL, got %v", retry)
	}
}

func TestRestoreOperationIDIsStableAndScoped(t *testing.T) {
	first := restoreOperationID("tenant-a", "version-1", "request-1")
	if len(first) != 36 {
		t.Fatalf("operation identity length = %d, want 36", len(first))
	}
	if retry := restoreOperationID("tenant-a", "version-1", "request-1"); retry != first {
		t.Fatalf("retry operation identity = %q, want %q", retry, first)
	}
	if restoreOperationID("tenant-a", "version-2", "request-1") == first || restoreOperationID("tenant-b", "version-1", "request-1") == first {
		t.Fatal("different tenant or target version reused an operation identity")
	}
}

func TestBuildConfigRestoreDDLRestoresQuarantinedTable(t *testing.T) {
	targetType := &doctype.DocType{Name: "Archive", Fields: []doctype.Field{{Fieldname: "subject", Fieldtype: "Data"}}}
	quarantinedTable := doctype.QuarantineTableNameForRevision(targetType.RawTableName(), "restore-operation")
	live := map[string]*schema.TableInfo{quarantinedTable: {
		Name: quarantinedTable, Columns: map[string]*schema.ColumnInfo{"subject": {Name: "subject"}},
	}}
	plan, err := buildConfigRestoreDDL(nil, &doctype.ConfigSnapshot{DocTypes: []*doctype.DocType{targetType}}, live, db.MySQL(), "restore-operation")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 1 || !strings.Contains(plan[0], "RENAME TO `tabArchive`") {
		t.Fatalf("table restore plan = %v", plan)
	}
}

func TestBuildConfigRestoreDDLRejectsAmbiguousQuarantine(t *testing.T) {
	targetType := &doctype.DocType{Name: "Invoice", Fields: []doctype.Field{{Fieldname: "legacy", Fieldtype: "Data"}}}
	first := doctype.QuarantineNameForRevision("legacy", "restore-1")
	second := doctype.QuarantineNameForRevision("legacy", "restore-2")
	_, err := buildConfigRestoreDDL(nil, &doctype.ConfigSnapshot{DocTypes: []*doctype.DocType{targetType}}, map[string]*schema.TableInfo{
		"tabInvoice": {Name: "tabInvoice", Columns: map[string]*schema.ColumnInfo{first: {Name: first}, second: {Name: second}}},
	}, db.MySQL(), "restore-operation")
	if err == nil || !strings.Contains(err.Error(), "manual recovery selection") {
		t.Fatalf("ambiguous quarantine was not rejected: %v", err)
	}
}
