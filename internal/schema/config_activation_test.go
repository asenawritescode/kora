package schema

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/asenawritescode/kora/internal/db"
	"github.com/asenawritescode/kora/internal/doctype"
)

func TestGenerateSnapshotActivationDDLPreservesRevisionedFieldChanges(t *testing.T) {
	target := &doctype.DocType{Name: "Invoice", Fields: []doctype.Field{{Fieldname: "title", Fieldtype: "Data"}}}
	snapshot := &doctype.ConfigSnapshot{DocTypes: []*doctype.DocType{target}}
	registry := doctype.NewRegistry()
	registry.Register(&doctype.DocType{Name: "Invoice", Fields: []doctype.Field{
		{Fieldname: "title", Fieldtype: "Data"},
		{Fieldname: "legacy_code", Fieldtype: "Data"},
		{Fieldname: "amount", Fieldtype: "Data"},
	}})
	live := map[string]*TableInfo{
		"tabInvoice": {Name: "tabInvoice", Columns: map[string]*ColumnInfo{
			"title":       {Name: "title"},
			"legacy_code": {Name: "legacy_code"},
			"amount":      {Name: "amount"},
		}},
	}
	changeList := `{"doctypes":{"changes":[{"type":"field_removed","doctype":"Invoice","field":"legacy_code"},{"type":"field_type_changed","doctype":"Invoice","field":"amount","old_value":"Data","new_value":"Int"}]}}`
	statements, err := GenerateSnapshotActivationDDL(snapshot, registry.Get, live, db.Resolve("mysql"), changeList, "revision-42")
	if err != nil {
		t.Fatalf("GenerateSnapshotActivationDDL(): %v", err)
	}
	joined := strings.Join(statements, "\n")
	if !strings.Contains(joined, doctype.QuarantineNameForRevision("legacy_code", "revision-42")) {
		t.Fatalf("plan omitted revision-scoped quarantine: %s", joined)
	}
	if !strings.Contains(strings.ToLower(joined), "amount") {
		t.Fatalf("plan omitted explicit field type migration: %s", joined)
	}
}

func TestGenerateSnapshotActivationDDLReadsLegacySExprChangeList(t *testing.T) {
	target := &doctype.DocType{Name: "Invoice", Fields: []doctype.Field{{Fieldname: "title", Fieldtype: "Data"}}}
	snapshot := &doctype.ConfigSnapshot{DocTypes: []*doctype.DocType{target}}
	legacyChanges, err := json.Marshal([]doctype.Change{{
		Type: "remove-field", Section: "doctypes", Entity: "Invoice", Field: "legacy_code",
	}})
	if err != nil {
		t.Fatal("marshal legacy change list:", err)
	}
	registry := doctype.NewRegistry()
	registry.Register(&doctype.DocType{Name: "Invoice", Fields: []doctype.Field{
		{Fieldname: "title", Fieldtype: "Data"}, {Fieldname: "legacy_code", Fieldtype: "Data"},
	}})
	live := map[string]*TableInfo{"tabInvoice": {Name: "tabInvoice", Columns: map[string]*ColumnInfo{
		"title": {Name: "title"}, "legacy_code": {Name: "legacy_code"},
	}}}
	statements, err := GenerateSnapshotActivationDDL(snapshot, registry.Get, live, db.Resolve("mysql"), string(legacyChanges), "legacy-review")
	if err != nil {
		t.Fatalf("GenerateSnapshotActivationDDL(): %v", err)
	}
	if !strings.Contains(strings.Join(statements, "\n"), doctype.QuarantineNameForRevision("legacy_code", "legacy-review")) {
		t.Fatalf("legacy removal was ignored: %#v", statements)
	}
}

func TestApplySnapshotActivationDDLUsesDialectExecutionSemantics(t *testing.T) {
	t.Run("postgres sequential and stops on error", func(t *testing.T) {
		database, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		defer database.Close()
		mock.ExpectExec("CREATE TABLE first").WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectExec("CREATE INDEX second").WillReturnError(errors.New("ddl rejected"))
		err = ApplySnapshotActivationDDL(context.Background(), database, db.Resolve("postgres"), []string{"CREATE TABLE first", "CREATE INDEX second", "CREATE TABLE must_not_run"})
		if err == nil || !strings.Contains(err.Error(), "statement 2") {
			t.Fatalf("ApplySnapshotActivationDDL() error = %v, want indexed second-statement error", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("libsql sends one batch", func(t *testing.T) {
		database, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		defer database.Close()
		mock.ExpectExec("CREATE TABLE first;.*CREATE INDEX second;").WillReturnResult(sqlmock.NewResult(0, 0))
		if err := ApplySnapshotActivationDDL(context.Background(), database, db.Resolve("libsql"), []string{"CREATE TABLE first", "CREATE INDEX second"}); err != nil {
			t.Fatalf("ApplySnapshotActivationDDL() error = %v", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("cancellation prevents writes", func(t *testing.T) {
		database, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		defer database.Close()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := ApplySnapshotActivationDDL(ctx, database, db.Resolve("mysql"), []string{"CREATE TABLE first"}); !errors.Is(err, context.Canceled) {
			t.Fatalf("ApplySnapshotActivationDDL() error = %v, want context.Canceled", err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestGenerateSnapshotActivationDDLReplansWithoutHistoricalChangeList(t *testing.T) {
	snapshot := &doctype.ConfigSnapshot{DocTypes: []*doctype.DocType{{Name: "Invoice", Fields: []doctype.Field{{Fieldname: "title", Fieldtype: "Data"}}}}}
	statements, err := GenerateSnapshotActivationDDL(snapshot, nil, map[string]*TableInfo{}, db.Resolve("mysql"), "", "revision-43")
	if err != nil {
		t.Fatalf("GenerateSnapshotActivationDDL(): %v", err)
	}
	if len(statements) == 0 || !strings.Contains(strings.Join(statements, "\n"), "tabInvoice") {
		t.Fatalf("plan did not create a missing target table: %#v", statements)
	}
}

func TestGenerateSnapshotActivationDDLResolvesNewChildDoctypeFromTargetSnapshot(t *testing.T) {
	parent := &doctype.DocType{Name: "Purchase", Fields: []doctype.Field{{
		Fieldname: "purchase_items", Fieldtype: "Table", Options: "Purchase Item",
	}}}
	child := &doctype.DocType{Name: "Purchase Item", IsChildTable: true, Fields: []doctype.Field{
		{Fieldname: "product", Fieldtype: "Link", Options: "Product"},
		{Fieldname: "quantity", Fieldtype: "Int"},
	}}
	snapshot := &doctype.ConfigSnapshot{DocTypes: []*doctype.DocType{parent, child}}
	staleRegistry := doctype.NewRegistry()
	staleRegistry.Register(&doctype.DocType{Name: "Purchase", Fields: []doctype.Field{{
		Fieldname: "purchase_items", Fieldtype: "Table", Options: "Purchase Item",
	}}})

	statements, err := GenerateSnapshotActivationDDL(snapshot, staleRegistry.Get, map[string]*TableInfo{}, db.Resolve("mysql"), "", "revision-child")
	if err != nil {
		t.Fatalf("GenerateSnapshotActivationDDL(): %v", err)
	}
	joined := strings.Join(statements, "\n")
	childTable := parent.RawChildTableName("purchase_items")
	if !strings.Contains(joined, "CREATE TABLE") || !strings.Contains(joined, childTable) {
		t.Fatalf("activation plan omitted child table %q: %s", childTable, joined)
	}
	if !strings.Contains(joined, "`product`") || !strings.Contains(joined, "`quantity`") {
		t.Fatalf("activation plan omitted target child doctype fields: %s", joined)
	}
}

func TestGenerateSnapshotActivationDDLQuarantinesRemovedDoctypeAndChildTable(t *testing.T) {
	old := &doctype.DocType{Name: "Opportunity", Fields: []doctype.Field{{Fieldname: "reference", Fieldtype: "Data"}, {Fieldname: "items", Fieldtype: "Table", Options: "Opportunity Item"}}}
	registry := doctype.NewRegistry()
	registry.Register(old)
	snapshot := &doctype.ConfigSnapshot{DocTypes: []*doctype.DocType{}}
	changeList := `{"doctypes":{"changes":[{"type":"doctype_removed","doctype":"Opportunity","breaking":true}]}}`
	childTable := old.RawChildTableName("items")
	live := map[string]*TableInfo{
		old.RawTableName(): {Name: old.RawTableName(), Columns: map[string]*ColumnInfo{"name": {Name: "name"}}},
		childTable:         {Name: childTable, Columns: map[string]*ColumnInfo{"parent": {Name: "parent"}}},
	}
	statements, err := GenerateSnapshotActivationDDL(snapshot, registry.Get, live, db.Resolve("postgres"), changeList, "crm-retire-1")
	if err != nil {
		t.Fatalf("GenerateSnapshotActivationDDL(): %v", err)
	}
	if len(statements) != 2 {
		t.Fatalf("got %d quarantine statements; want main and child tables: %v", len(statements), statements)
	}
	for i, raw := range []string{old.RawTableName(), childTable} {
		want := doctype.QuarantineTableNameForRevision(raw, "crm-retire-1")
		if !strings.Contains(statements[i], raw) || !strings.Contains(statements[i], want) || !strings.Contains(strings.ToLower(statements[i]), "rename to") {
			t.Fatalf("statement %d does not preserve %q as %q: %s", i, raw, want, statements[i])
		}
	}
	// A retry sees only the already-quarantined tables and must not try to
	// rename or overwrite them again.
	retryLive := map[string]*TableInfo{
		doctype.QuarantineTableNameForRevision(old.RawTableName(), "crm-retire-1"): {Name: doctype.QuarantineTableNameForRevision(old.RawTableName(), "crm-retire-1"), Columns: map[string]*ColumnInfo{"name": {Name: "name"}}},
		doctype.QuarantineTableNameForRevision(childTable, "crm-retire-1"):         {Name: doctype.QuarantineTableNameForRevision(childTable, "crm-retire-1"), Columns: map[string]*ColumnInfo{"parent": {Name: "parent"}}},
	}
	retried, err := GenerateSnapshotActivationDDL(snapshot, registry.Get, retryLive, db.Resolve("postgres"), changeList, "crm-retire-1")
	if err != nil {
		t.Fatalf("retry GenerateSnapshotActivationDDL(): %v", err)
	}
	if len(retried) != 0 {
		t.Fatalf("retry should be a no-op, got %v", retried)
	}
}

func TestGenerateSnapshotActivationDDLRemovedDoctypeFailsClosedOnQuarantineCollision(t *testing.T) {
	old := &doctype.DocType{Name: "Opportunity", Fields: []doctype.Field{{Fieldname: "reference", Fieldtype: "Data"}}}
	registry := doctype.NewRegistry()
	registry.Register(old)
	snapshot := &doctype.ConfigSnapshot{DocTypes: []*doctype.DocType{}}
	changeList := `{"doctypes":{"changes":[{"type":"doctype_removed","doctype":"Opportunity","breaking":true}]}}`
	quarantine := doctype.QuarantineTableNameForRevision(old.RawTableName(), "crm-retire-2")
	live := map[string]*TableInfo{
		old.RawTableName(): {Name: old.RawTableName(), Columns: map[string]*ColumnInfo{"name": {Name: "name"}}},
		quarantine:         {Name: quarantine, Columns: map[string]*ColumnInfo{"name": {Name: "name"}}},
	}
	if _, err := GenerateSnapshotActivationDDL(snapshot, registry.Get, live, db.Resolve("postgres"), changeList, "crm-retire-2"); err == nil || !strings.Contains(err.Error(), "destination table") {
		t.Fatalf("expected fail-closed quarantine collision, got %v", err)
	}
}

func TestGenerateSnapshotActivationDDLRemovedDoctypeRequiresPreviousSchema(t *testing.T) {
	snapshot := &doctype.ConfigSnapshot{DocTypes: []*doctype.DocType{}}
	changeList := `{"doctypes":{"changes":[{"type":"doctype_removed","doctype":"Opportunity","breaking":true}]}}`
	live := map[string]*TableInfo{"tabOpportunity": {Name: "tabOpportunity", Columns: map[string]*ColumnInfo{"name": {Name: "name"}}}}
	if _, err := GenerateSnapshotActivationDDL(snapshot, nil, live, db.Resolve("mysql"), changeList, "crm-retire-3"); err == nil || !strings.Contains(err.Error(), "without its previous schema") {
		t.Fatalf("expected fail-closed missing prior schema error, got %v", err)
	}
}
